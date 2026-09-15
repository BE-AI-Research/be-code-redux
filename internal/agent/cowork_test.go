package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func withCoworkers(names ...string) func(*config.Config) {
	return func(c *config.Config) {
		for i, n := range names {
			online := strings.HasPrefix(n, "online-")
			c.Coworkers = append(c.Coworkers, config.CoworkerConfig{Name: n, Provider: "ollama", Model: "cw-model-" + n, Skills: "skill " + n, Online: online})
			_ = i
		}
	}
}

// coworkerStub scripts the co-worker's replies and records every request.
func coworkerStub(t *testing.T, responses ...provider.ChatResponse) *scriptedProvider {
	t.Helper()
	p := &scriptedProvider{responses: responses}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	return p
}

func TestConsultRunsAReadOnlyScratchAgentAndReturnsTheAnswer(t *testing.T) {
	cw := coworkerStub(t,
		provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"main.go"}`}}},
		provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "write_file", Arguments: `{"path":"main.go","content":"x"}`}}},
		provider.ChatResponse{Content: "Use strings.Builder in main.go:12."},
	)
	ag, dir := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	var started, ended int
	ag.Events.OnConsultStart = func(name, q, origin string) { started++ }
	ag.Events.OnConsultEnd = func(res ConsultResult, err error) { ended++ }
	res, err := ag.Consult(context.Background(), ConsultRequest{Question: "why does main.go not compile?", Files: []string{"main.go"}, Origin: "tool"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Coworker != "big" || res.Answer != "Use strings.Builder in main.go:12." || res.Partial || res.Read != 1 {
		t.Fatalf("result = %+v", res)
	}
	if started != 1 || ended != 1 {
		t.Fatalf("events start=%d end=%d", started, ended)
	}
	// The write attempt was refused: the file is untouched and the model was told.
	if b, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(b) != "package main\n" {
		t.Fatal("the co-worker edited a file")
	}
	seed := cw.lastReq.Messages[1].Content // [0] system, [1] seed (the tool result turns follow)
	if !strings.Contains(cw.lastReq.Messages[0].Content, "keeps the pen") {
		t.Fatalf("system prompt is not the consult frame: %q", cw.lastReq.Messages[0].Content[:80])
	}
	_ = seed
	first := cw.lastReq.Messages[1].Content
	if !strings.Contains(first, "why does main.go not compile?") || !strings.Contains(first, "package main") {
		t.Fatalf("seed lacks the question or the named file:\n%s", first)
	}
	for _, spec := range cw.lastReq.Tools {
		if spec.Name == "write_file" || spec.Name == "shell" {
			t.Fatalf("co-worker was offered %s", spec.Name)
		}
	}
	if ag.ConsultsThisRun() != 1 || ag.ConsultCount("big") != 1 {
		t.Fatalf("counters: run=%d big=%d", ag.ConsultsThisRun(), ag.ConsultCount("big"))
	}
}

func TestConsultCapAndUnknownName(t *testing.T) {
	coworkerStub(t, provider.ChatResponse{Content: "ok"})
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("a", "b")(c); c.Cowork.MaxConsultsPerRun = 2 })
	for i := 0; i < 2; i++ {
		if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if err == nil || err.Error() != "consultation limit reached for this run (2)" {
		t.Fatalf("cap error = %v", err)
	}
	// /consult never counts.
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "user:desk"}); err != nil {
		t.Fatalf("user consult blocked by the cap: %v", err)
	}
	_, err = ag.Consult(context.Background(), ConsultRequest{Who: "nobody", Question: "q", Origin: "user:desk"})
	if err == nil || err.Error() != `unknown co-worker "nobody" (configured: a, b)` {
		t.Fatalf("unknown error = %v", err)
	}
	// RunFull resets the run counter.
	ag.RunFull(context.Background(), "hi")
	if ag.ConsultsThisRun() != 0 {
		t.Fatal("cap not reset by RunFull")
	}
}

func TestConsultConsentForOnlineCoworkers(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "advice"})
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("online-claude", "local"))
	var asked []string
	answer := false
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action+"|"+detail); return answer }
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "secret", Origin: "tool"}); err == nil || err.Error() != "consultation declined" {
		t.Fatalf("declined error = %v", err)
	}
	if len(cw.lastReq.Messages) != 0 {
		t.Fatal("something was sent to the online co-worker before consent")
	}
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "consult|coworker: online-claude") || !strings.Contains(asked[0], "question: secret") {
		t.Fatalf("approval detail: %q", asked)
	}
	answer = true
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q2", Origin: "tool"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q3", Origin: "tool"}); err != nil || len(asked) != 3 {
		t.Fatalf("a plain yes must allow one consultation only: err=%v asked=%d", err, len(asked))
	}
	ag.AllowCoworker("online-claude")
	ag.Consult(context.Background(), ConsultRequest{Question: "q4", Origin: "auto:verify"})
	if len(asked) != 3 {
		t.Fatal("AllowCoworker did not suppress the prompt")
	}
	// Local co-workers never prompt; /consult never prompts.
	ag.Consult(context.Background(), ConsultRequest{Who: "local", Question: "q", Origin: "tool"})
	ag.Consult(context.Background(), ConsultRequest{Who: "online-claude", Question: "q", Origin: "user:desk"})
	if len(asked) != 3 {
		t.Fatalf("local or user consult prompted: %d", len(asked))
	}
	// No approver at all (a non-interactive run without -y) declines.
	ag.Tools.Approve = nil
	ag2, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("online-x"))
	ag2.Tools.Approve = nil
	if _, err := ag2.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err == nil || err.Error() != "consultation declined" {
		t.Fatalf("headless without -y: %v", err)
	}
	// -y (AutoApproveShell) allows without a prompt.
	ag3, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("online-y")(c); c.AutoApproveShell = true })
	ag3.Tools.Approve = nil
	if _, err := ag3.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err != nil {
		t.Fatalf("-y: %v", err)
	}
}

func TestConsultReturnsPartialOnMidwayError(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{Content: "First thought.", ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{}`}}}, nil
		}
		return nil, errors.New("connection reset")
	}}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("flaky"))
	res, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if err != nil || !res.Partial || res.Answer != "First thought." {
		t.Fatalf("partial: res=%+v err=%v", res, err)
	}
	// An error before any answer is an error, and still counts.
	p.fn = func(req provider.ChatRequest) (*provider.ChatResponse, error) { return nil, errors.New("down") }
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err == nil {
		t.Fatal("expected an error")
	}
	if ag.ConsultsThisRun() != 2 {
		t.Fatalf("failed consultations must count: %d", ag.ConsultsThisRun())
	}
}

func TestConsultTurnCap(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{}`}}}, nil
	}}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("loop")(c); c.Cowork.ConsultTurns = 3 })
	ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if len(p.reqs) != 3 {
		t.Fatalf("co-worker ran %d turns, want 3", len(p.reqs))
	}
}

// A consultation the user abandoned is not a partial answer: whatever the
// co-worker had said comes back for display, but the error is the
// cancellation, so no caller can feed the abandoned text to the primary.
func TestConsultCancelledContextIsNotAPartialAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	p := &funcProvider{}
	p.fn = func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{Content: "Half an answer.", ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{}`}}}, nil
		}
		cancel() // the user pressed Esc while the co-worker was working
		return nil, context.Canceled
	}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	res, err := ag.Consult(ctx, ConsultRequest{Question: "q", Origin: "tool"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Partial {
		t.Fatal("an abandoned consultation was reported as a partial answer")
	}
	if res.Answer != "Half an answer." {
		t.Fatalf("the text it had produced was dropped: %q", res.Answer)
	}
}

// The counters are read and written by a UI goroutine (/coworkers, the
// approval modal's "a") while the agent goroutine is inside Consult. Under
// -race this fails without coworkMu; a concurrent map write would be fatal
// even without it.
func TestConsultCountersAreConcurrencySafe(t *testing.T) {
	coworkerStub(t, provider.ChatResponse{Content: "advice"})
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("online-claude"))
	release := make(chan struct{})
	ag.Tools.Approve = func(action, detail string) bool { <-release; return true }

	consulted := make(chan struct{})
	go func() {
		defer close(consulted)
		ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = ag.ConsultCount("online-claude")
			_ = ag.ConsultsThisRun()
			ag.AllowCoworker("someone-else")
		}
	}()

	close(release) // let the consultation past the approval seam and run
	<-consulted
	close(stop)
	wg.Wait()

	if ag.ConsultCount("online-claude") != 1 || ag.ConsultsThisRun() != 1 {
		t.Fatalf("counters: online-claude=%d run=%d", ag.ConsultCount("online-claude"), ag.ConsultsThisRun())
	}
}

// A byte cap must never land inside a multi-byte character: the model
// would read U+FFFD where the source had a letter.
func TestConsultSeedCutsAtRuneBoundaries(t *testing.T) {
	s := "x" + strings.Repeat("é", 10) // two-byte runes starting at odd offsets
	for n := 0; n <= len(s); n++ {
		if got := cutHead(s, n); !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("cutHead(%d) = %q", n, got)
		}
		if got := cutTail(s, n); !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("cutTail(%d) = %q", n, got)
		}
	}
	if cutHead(s, len(s)+1) != s || cutTail(s, len(s)+1) != s {
		t.Fatal("a cap wider than the string must leave it whole")
	}
}

// The seed says so when a named file could not be read, and does not
// repeat the git summary the scratch agent's own system prompt carries.
func TestConsultSeedLabelsFailedReadsAndLeavesGitToTheSystemPrompt(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "advice"})
	ag, dir := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v %s", err, out)
	}
	if _, err := ag.Consult(context.Background(), ConsultRequest{
		Question: "q", Files: []string{"missing.go"}, Origin: "tool",
	}); err != nil {
		t.Fatal(err)
	}
	sys, seed := cw.lastReq.Messages[0].Content, cw.lastReq.Messages[1].Content
	if !strings.Contains(seed, "### missing.go\n(could not read: ") {
		t.Fatalf("a failed read was passed off as file content:\n%s", seed)
	}
	if strings.Contains(seed, "## Git") {
		t.Fatalf("the git summary is in the seed as well as the system prompt:\n%s", seed)
	}
	if !strings.Contains(sys, "git branch:") {
		t.Fatal("the scratch agent's system prompt lost the git summary")
	}
}

func TestPlanAgentOffersConsult(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	ag.Tools.AddTool(tools.NewConsult(nil, func(context.Context, tools.ConsultArgs) (string, error) { return "", nil }))
	names := ag.planAgent().Tools.Names()
	found := false
	for _, n := range names {
		if n == "consult" {
			found = true
		}
		if n == "write_file" || n == "shell" {
			t.Fatalf("plan agent offers %s", n)
		}
	}
	if !found {
		t.Fatalf("plan agent lacks consult: %v", names)
	}
}

func TestRecentContextCarriesRequestReplyAndFailingTool(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"missing.go"}`}}},
		{Content: "I could not read it."},
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Run(context.Background(), "please fix missing.go")
	rc := ag.RecentContext()
	for _, want := range []string{"User request", "please fix missing.go", "Your last reply", "I could not read it.", "Failing tool output", "missing.go"} {
		if !strings.Contains(rc, want) {
			t.Fatalf("recent context lacks %q:\n%s", want, rc)
		}
	}
}
