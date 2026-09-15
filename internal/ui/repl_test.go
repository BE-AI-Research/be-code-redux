package ui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

type nullProvider struct{}

func (nullProvider) Name() string { return "null" }
func (nullProvider) Chat(context.Context, provider.ChatRequest, provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}
func (nullProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (nullProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

func newTestREPL(t *testing.T) *REPL {
	t.Helper()
	cfg := config.Default()
	cfg.RepoMap = false
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &REPL{Cfg: cfg, Agent: agent.New(cfg, nullProvider{}, "m", reg, ""), Provider: nullProvider{}}
}

// capture runs fn with os.Stdout redirected and returns what it printed.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = prev
	w.Close()
	out := <-done
	r.Close()
	return out
}

// saveLive writes a session plus a live record advertising its code, and
// returns the session.
func saveLive(t *testing.T, live_ bool) *store.Session {
	t.Helper()
	s := store.NewSession("ollama", "m", "/ws")
	s.Title = "the other terminal's work"
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if !live_ {
		return s
	}
	dir, err := live.Dir()
	if err != nil {
		t.Fatal(err)
	}
	rec := live.Record{
		Code: s.ResumeCode(), PID: os.Getpid(), Socket: filepath.Join(dir, s.ResumeCode()+".sock"),
		Workspace: "/ws", StartedAt: time.Now(),
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	return s
}

// Plain mode has no host to switch through, so /resume of a code that is
// live elsewhere loads nothing and says how to join it instead — the fork
// this whole feature exists to prevent.
func TestPlainResumeOfALiveCodeRefusesToFork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	r := newTestREPL(t)
	s := saveLive(t, true)

	out := capture(t, func() { r.command(context.Background(), "/resume "+s.ResumeCode()) })
	want := s.ResumeCode() + " is live elsewhere; join it with: be-code attach " + s.ResumeCode()
	if !strings.Contains(out, want) {
		t.Fatalf("/resume of a live code printed:\n%s\nwant %q", out, want)
	}
	if r.Agent.Session != nil || len(r.Agent.History.Messages) != 0 {
		t.Fatalf("the live session was loaded anyway: %+v", r.Agent.Session)
	}
}

// A saved session that is not live still resumes as it always did.
func TestPlainResumeOfAColdCodeStillLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	r := newTestREPL(t)
	s := saveLive(t, false)

	out := capture(t, func() { r.command(context.Background(), "/resume "+s.ResumeCode()) })
	if !strings.Contains(out, "resumed "+s.ResumeCode()) {
		t.Fatalf("/resume of a cold code printed:\n%s", out)
	}
	if r.Agent.Session == nil || r.Agent.Session.ID != s.ID {
		t.Fatalf("cold session was not loaded: %+v", r.Agent.Session)
	}
}

// /review reports where file changes are reviewed, and sets it.
func TestREPLReviewCommand(t *testing.T) {
	r := newTestREPL(t)
	r.SetReview(review.New(review.ModeAuto, nil, r.ReviewTerminal(), nil))
	out := capture(t, func() { r.command(context.Background(), "/review") })
	if !strings.Contains(out, "review: auto (resolves to editor)") {
		t.Fatalf("mode line missing: %q", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/review both") })
	if !strings.Contains(out, "review: both (resolves to both)") {
		t.Fatalf("mode not set: %q", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/review nonsense") })
	if !strings.Contains(out, "nonsense") {
		t.Fatalf("invalid mode not reported: %q", out)
	}
	if r.Review.Mode() != review.ModeBoth {
		t.Fatalf("mode = %q", r.Review.Mode())
	}
}

// /review is safe to run in the middle of a turn.
func TestReviewIsBusySafe(t *testing.T) {
	if !BusySafeCommand("/review both") {
		t.Fatal("/review must be usable while the agent is busy")
	}
	found := false
	for _, c := range SlashCommandTable {
		if c.Name == "/review" {
			found = true
			if !c.Args {
				t.Fatal("/review takes an argument")
			}
		}
	}
	if !found {
		t.Fatal("/review missing from SlashCommandTable")
	}
}

// A withdrawn question (the editor answered the same change first) must not
// swallow the line the user had just typed into it: it goes back to the line
// channel, where runBusy queues it for the agent, and the question stops
// taking further lines at once so a second one cannot wedge runBusy.
func TestAbandonedQuestionRequeuesTypedLine(t *testing.T) {
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	ch := make(chan string, 1)
	r.mu.Lock()
	r.ask = ch
	r.mu.Unlock()
	ch <- "y"
	r.abandonAsk(ch)
	r.mu.Lock()
	still := r.ask
	r.mu.Unlock()
	if still != nil {
		t.Fatal("a withdrawn question is still the destination for typed lines")
	}
	select {
	case ev := <-r.lines:
		if ev.line != "y" {
			t.Fatalf("requeued %q", ev.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the typed line was lost with the withdrawn question")
	}
}

// promptCtx returns nothing ("no") when the question is cancelled, and
// leaves nobody registered for typed lines.
func TestPromptCtxCancelled(t *testing.T) {
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	r.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- r.promptCtx(ctx, "approve? ") }()
	for i := 0; ; i++ {
		r.mu.Lock()
		set := r.ask != nil
		r.mu.Unlock()
		if set {
			break
		}
		if i > 200 {
			t.Fatal("promptCtx never registered the question")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("cancelled question answered %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promptCtx ignored the cancellation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ask != nil {
		t.Fatal("cancelled question still registered")
	}
}

// liveOnly advertises a host for a code that has no saved session file — a
// session that has not autosaved yet.
func liveOnly(t *testing.T, code string) {
	t.Helper()
	dir, err := live.Dir()
	if err != nil {
		t.Fatal(err)
	}
	rec := live.Record{Code: code, PID: os.Getpid(), Socket: filepath.Join(dir, code+".sock"), Workspace: "/w/unsaved", StartedAt: time.Now()}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
}

// A live code with no saved file yet is still a session to join, not a
// store error: the registry answers before the store.
func TestPlainResumeOfAnUnsavedLiveCodeSaysHowToJoin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	r := newTestREPL(t)
	liveOnly(t, "XYZ789")

	out := capture(t, func() { r.command(context.Background(), "/resume xyz789") })
	want := "XYZ789 is live elsewhere; join it with: be-code attach XYZ789"
	if !strings.Contains(out, want) {
		t.Fatalf("/resume of an unsaved live code printed:\n%s\nwant %q", out, want)
	}
	if strings.Contains(out, "error>") {
		t.Fatalf("store error surfaced for a live code:\n%s", out)
	}
}

// /sessions lists live sessions the store has no file for, and marks the
// saved ones that are live, as `be-code sessions` does.
func TestPlainSessionsListsLiveOnes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	r := newTestREPL(t)
	s := saveLive(t, true)
	liveOnly(t, "XYZ789")

	out := capture(t, func() { r.command(context.Background(), "/sessions") })
	if !strings.Contains(out, s.ResumeCode()+"  LIVE") {
		t.Fatalf("saved live session not marked:\n%s", out)
	}
	if !strings.Contains(out, "XYZ789  LIVE") || !strings.Contains(out, "no saved turns yet") || !strings.Contains(out, "/w/unsaved") {
		t.Fatalf("unsaved live session missing:\n%s", out)
	}
	if !strings.Contains(out, "be-code attach <code>") {
		t.Fatalf("no join hint:\n%s", out)
	}
}

func TestPlainCoworkersAndConsult(t *testing.T) {
	r := newTestREPL(t)
	out := capture(t, func() { r.command(context.Background(), "/coworkers") })
	if !strings.Contains(out, "no co-working models configured") {
		t.Fatalf("empty list:\n%s", out)
	}
	// A refusal before the co-worker starts fires no event, so /consult
	// must print it itself.
	out = capture(t, func() { r.command(context.Background(), "/consult anything?") })
	if !strings.Contains(out, "error>") || !strings.Contains(out, "no co-working models") {
		t.Fatalf("pre-flight refusal not printed:\n%s", out)
	}
	r.Cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"}}
	r.Agent = agent.New(r.Cfg, r.Provider, "m", r.Agent.Tools, "")
	r.Agent.Events = Events()
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "Try the other branch." }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	out = capture(t, func() { r.command(context.Background(), "/coworkers") })
	if !strings.Contains(out, "big") || !strings.Contains(out, "long reads") {
		t.Fatalf("list:\n%s", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/consult big which branch?") })
	for _, want := range []string{"big? which branch?", "big> Try the other branch.", "read 0 files"} {
		if !strings.Contains(out, want) {
			t.Fatalf("consult output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(capture(t, func() { r.command(context.Background(), "/consult") }), "usage: /consult") {
		t.Fatal("no usage line")
	}
}

// F2: plain mode's "a" on a consult approval records session consent for
// that co-worker, the way the TUI modal's "a" does — otherwise the only way
// to stop being asked in plain mode is -y, which allows every co-worker.
func TestPlainAlwaysAllowsTheCoworkerForTheSession(t *testing.T) {
	r := newTestREPL(t)
	r.Cfg.Coworkers = []config.CoworkerConfig{
		{Name: "claude", Provider: "ollama", Model: "opus", Online: true},
	}
	r.Agent = agent.New(r.Cfg, r.Provider, "m", r.Agent.Tools, "")
	r.Agent.Tools.Approve = r.approve
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "advice" }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })

	// approve reads the answer through prompt(), which takes one line off
	// r.lines while the REPL is idle.
	r.lines = make(chan lineEvent, 1)
	r.lines <- lineEvent{line: "a"}
	var err error
	out := capture(t, func() {
		_, err = r.Agent.Consult(context.Background(), agent.ConsultRequest{Question: "q", Origin: "tool"})
	})
	if err != nil {
		t.Fatalf("approved consultation failed: %v", err)
	}
	if !strings.Contains(out, "coworker: claude") {
		t.Fatalf("the consent detail was not shown:\n%s", out)
	}
	if !strings.Contains(out, "co-worker claude allowed for this session") {
		t.Fatalf("the session allow was not reported:\n%s", out)
	}
	if r.Cfg.AutoApproveShell || r.Cfg.AutoApproveConsult {
		t.Fatal("a consult approval flipped a shell/global auto-approve flag")
	}

	// The second consultation must not ask again: nothing is queued on
	// r.lines, and a prompt would be a failure rather than a hang.
	r.Agent.Tools.Approve = func(action, detail string) bool {
		t.Errorf("prompted again after the session allow (%s)", action)
		return false
	}
	out = capture(t, func() {
		_, err = r.Agent.Consult(context.Background(), agent.ConsultRequest{Question: "q2", Origin: "tool"})
	})
	if err != nil {
		t.Fatalf("second consultation: %v (%s)", err, out)
	}
}

// TestPlainTaskAndNotesCommands covers /task and /notes in plain mode: both
// say so when no store is attached, and otherwise read and edit it directly
// from the UI goroutine (the store has its own lock).
func TestPlainTaskAndNotesCommands(t *testing.T) {
	r := newTestREPL(t)
	out := capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "working memory is off") {
		t.Fatalf("no engine:\n%s", out)
	}
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), r.Agent.Tools.Root, "s", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	r.Agent.SetEngine(st)
	st.SetPlan("add flag", []string{"parse", "wire"})
	st.SetStep(1, "doing")
	out = capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "task: add flag") || !strings.Contains(out, "[>] 1. parse") {
		t.Fatalf("/task:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/notes add tests need go") })
	out = capture(t, func() { r.command(context.Background(), "/notes") })
	if !strings.Contains(out, "1. tests need go") {
		t.Fatalf("/notes:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/notes drop 1") })
	if out = capture(t, func() { r.command(context.Background(), "/notes") }); !strings.Contains(out, "no notes") {
		t.Fatalf("after drop:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/task clear") })
	if st.Ledger().Task != "" {
		t.Fatal("/task clear did not clear")
	}
	// Whatever spacing was typed, the note is the text after the add token.
	capture(t, func() { r.command(context.Background(), "/notes  add  spaced text") })
	if out = capture(t, func() { r.command(context.Background(), "/notes") }); !strings.Contains(out, "1. spaced text") {
		t.Fatalf("spaced add:\n%s", out)
	}
	if out = capture(t, func() { r.command(context.Background(), "/notes drop") }); !strings.Contains(out, "usage: /notes drop N") {
		t.Fatalf("bare drop:\n%s", out)
	}
}
