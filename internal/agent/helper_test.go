package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// recProvider records every request and answers from a script, then with
// a default; fail makes every call fail. Safe for concurrent use.
type recProvider struct {
	name   string
	mu     sync.Mutex
	reqs   []provider.ChatRequest
	script []provider.ChatResponse
	def    provider.ChatResponse
	fail   error
}

func (r *recProvider) Name() string { return r.name }
func (r *recProvider) Chat(_ context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	if r.fail != nil {
		return nil, r.fail
	}
	if len(r.script) > 0 {
		resp := r.script[0]
		r.script = r.script[1:]
		return &resp, nil
	}
	resp := r.def
	return &resp, nil
}
func (r *recProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (r *recProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

func (r *recProvider) requests() []provider.ChatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]provider.ChatRequest(nil), r.reqs...)
}

// withHelper installs a test HelperFactory for the duration of the test and
// counts its builds.
func withHelper(t *testing.T, p provider.Provider, window int, err error) *atomic.Int32 {
	t.Helper()
	var builds atomic.Int32
	old := HelperFactory
	HelperFactory = func(ctx context.Context, cfg *config.Config) (provider.Provider, string, int, error) {
		builds.Add(1)
		if err != nil {
			return nil, "", 0, err
		}
		return p, cfg.LocalHelper.Model, window, nil
	}
	t.Cleanup(func() { HelperFactory = old })
	return &builds
}

func helperCfg(c *config.Config) {
	c.LocalHelper = config.LocalHelperConfig{Provider: "ollama", Model: "helper-model"}
}

// seedHistory gives Compact something to summarise and a handoff something
// to brief.
func seedHistory(ag *Agent, n, size int) {
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "build the widget"})
	for i := 0; i < n; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: fmt.Sprintf("step-%03d %s", i, strings.Repeat("x", size))})
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("ok-%03d", i)})
	}
}

// gitRepo makes dir a repository with one commit and one uncommitted change.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
}

func TestChoresUseHelperWhileOnline(t *testing.T) {
	primary := &recProvider{name: "primary", script: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"w.go","content":"package w\n"}`}}},
		{Content: "wrote it"},
	}, def: provider.ChatResponse{Content: "PRIMARY"}}
	helper := &recProvider{name: "helper", def: provider.ChatResponse{Content: "APPROVED summary notes fix: x"}}
	builds := withHelper(t, helper, 32768, nil)
	ag, dir := newTestAgent(t, primary, func(c *config.Config) { helperCfg(c); c.ReviewOnDone = true })
	ag.SetOnline("openrouter", "K", Pricing{Prompt: 1e-6, Completion: 1e-6, Known: true})
	ag.Session = store.NewSession("rec", "test-model", dir)
	cp, err := checkpoint.New(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag.Checkpoints = cp

	// The review, after a real turn that wrote a file.
	if _, rep, err := ag.RunFull(context.Background(), "write w.go"); err != nil || rep == nil || !rep.Reviewed {
		t.Fatalf("runfull: %v %+v", err, rep)
	}
	primaryTurns := len(primary.requests())
	if primaryTurns != 2 {
		t.Fatalf("primary got %d requests, want the 2 turn requests", primaryTurns)
	}
	if n := len(helper.requests()); n != 1 || !strings.Contains(helper.requests()[0].Messages[0].Content, "code reviewer") {
		t.Fatalf("review did not go to the helper: %d", n)
	}

	seedHistory(ag, 4, 50)
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.WriteHandoff(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ag.InitProject(context.Background(), factsFixture()); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, dir)
	if _, err := ag.GenerateCommit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(primary.requests()); n != primaryTurns {
		t.Fatalf("a chore reached the online model: %d requests", n)
	}
	got := helper.requests()
	if len(got) < 5 {
		t.Fatalf("helper got %d requests, want review+summary+handoff+init+commit", len(got))
	}
	for _, r := range got {
		if r.Model != "helper-model" {
			t.Fatalf("helper request for model %q", r.Model)
		}
	}
	if b := builds.Load(); b != 1 {
		t.Fatalf("helper built %d times", b)
	}
}

func TestChoresUsePrimaryWhenLocal(t *testing.T) {
	primary := &recProvider{name: "primary", def: provider.ChatResponse{Content: "SUMMARY"}}
	helper := &recProvider{name: "helper"}
	builds := withHelper(t, helper, 32768, nil)
	ag, dir := newTestAgent(t, primary, helperCfg)
	ag.Session = store.NewSession("rec", "test-model", dir)
	seedHistory(ag, 4, 50)
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.WriteHandoff(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 0 || len(helper.requests()) != 0 {
		t.Fatal("helper built or used for a local session")
	}
	if len(primary.requests()) != 2 {
		t.Fatalf("primary got %d", len(primary.requests()))
	}
}

func helperNotices(ag *Agent) func() int {
	l := spendNotices(ag)
	return func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		n := 0
		for _, s := range l.log {
			if s == helperUnavailableNote {
				n++
			}
		}
		return n
	}
}

func TestHelperDownFallsBackOnceWithNotice(t *testing.T) {
	primary := &recProvider{name: "primary", def: provider.ChatResponse{Content: "FROM PRIMARY"}}
	builds := withHelper(t, nil, 0, errors.New("connection refused"))
	ag, dir := newTestAgent(t, primary, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.Session = store.NewSession("rec", "test-model", dir)
	count := helperNotices(ag)

	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	oldNow := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = oldNow })

	seedHistory(ag, 4, 50)
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := ag.WriteHandoff(context.Background(), true)
	if err != nil || !strings.Contains(h, "FROM PRIMARY") {
		t.Fatalf("handoff %q %v", h, err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
	if b := builds.Load(); b != 1 {
		t.Fatalf("built %d times within the down window", b)
	}
	if len(primary.requests()) != 2 {
		t.Fatalf("primary got %d", len(primary.requests()))
	}
	// Past the window the helper is tried again.
	now = now.Add(helperDownFor + time.Second)
	if _, _, err := ag.InitProject(context.Background(), factsFixture()); err != nil {
		t.Fatal(err)
	}
	if b := builds.Load(); b != 2 {
		t.Fatalf("not retried after the window: %d builds", b)
	}
}

func TestHelperChatErrorRetriesOnPrimary(t *testing.T) {
	primary := &recProvider{name: "primary", def: provider.ChatResponse{Content: "FROM PRIMARY"}}
	helper := &recProvider{name: "helper", fail: errors.New("connection reset")}
	withHelper(t, helper, 32768, nil)
	ag, dir := newTestAgent(t, primary, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.Session = store.NewSession("rec", "test-model", dir)
	count := helperNotices(ag)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "build the widget"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	h, err := ag.WriteHandoff(context.Background(), true)
	if err != nil || !strings.Contains(h, "FROM PRIMARY") {
		t.Fatalf("handoff %q %v", h, err)
	}
	if len(helper.requests()) != 1 || len(primary.requests()) != 1 {
		t.Fatalf("helper %d primary %d", len(helper.requests()), len(primary.requests()))
	}
	if primary.requests()[0].Model != "test-model" {
		t.Fatalf("retry model %q", primary.requests()[0].Model)
	}
	if count() != 1 {
		t.Fatalf("notices %d", count())
	}
	if u := ag.Usage(); u.HelperPromptTokens != 0 {
		t.Fatalf("a failed helper call counted tokens: %+v", u)
	}
}

func TestHelperPromptTrimmedToWindow(t *testing.T) {
	primary := &recProvider{name: "primary"}
	helper := &recProvider{name: "helper", def: provider.ChatResponse{Content: "SUMMARY"}}
	withHelper(t, helper, 4096, nil)
	ag, _ := newTestAgent(t, primary, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	seedHistory(ag, 40, 500)
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(primary.requests()) != 0 {
		t.Fatal("summary went to the primary")
	}
	reqs := helper.requests()
	if len(reqs) != 1 {
		t.Fatalf("helper got %d", len(reqs))
	}
	r := reqs[0]
	if est := helperPromptTokens(r.Messages); est+r.MaxTokens > 4096 {
		t.Fatalf("request %d + reply %d does not fit 4096", est, r.MaxTokens)
	}
	user := r.Messages[1].Content
	if !strings.Contains(user, "Original task:\nbuild the widget") {
		t.Fatal("task header lost")
	}
	// keepTail is 2: the newest kept-out-of-summary message is step-039's
	// reply's partner; step-038 is the newest in the transcript.
	if !strings.Contains(user, "step-038") || strings.Contains(user, "step-000") {
		t.Fatalf("not cut from the oldest end:\n%.300s", user)
	}
	if !strings.Contains(user, "[earlier transcript omitted") {
		t.Fatal("no omission marker")
	}
}

func TestHelperPromptTooBigContinuesFromTaskRecord(t *testing.T) {
	primary := &recProvider{name: "primary"}
	helper := &recProvider{name: "helper", def: provider.ChatResponse{Content: "SUMMARY"}}
	withHelper(t, helper, 2400, nil) // the header alone does not fit
	ag, _ := newTestAgent(t, primary, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: strings.Repeat("t", 1900)})
	seedHistory(ag, 10, 200)
	_ = ag.Compact(context.Background())
	if len(primary.requests()) != 0 || len(helper.requests()) != 0 {
		t.Fatalf("an oversized summary was sent: primary %d helper %d", len(primary.requests()), len(helper.requests()))
	}
}

func TestHelperUsageNotSpend(t *testing.T) {
	primary := &recProvider{name: "primary"}
	helper := &recProvider{name: "helper", def: provider.ChatResponse{Content: "SUMMARY",
		Usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 200}}}
	withHelper(t, helper, 32768, nil)
	ag, _ := newTestAgent(t, primary, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Prompt: 1e-3, Completion: 1e-3, Known: true})
	seedHistory(ag, 4, 50)
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := ag.Usage()
	if u.SpendUSD != 0 || u.PromptTokens != 0 || u.Requests != 0 {
		t.Fatalf("helper usage reached the primary's stats: %+v", u)
	}
	if u.HelperPromptTokens != 1000 || u.HelperCompletionTokens != 200 {
		t.Fatalf("helper tokens %d/%d", u.HelperPromptTokens, u.HelperCompletionTokens)
	}
	if rep := ag.StatsReport(nil); !strings.Contains(rep, "helper tokens") || !strings.Contains(rep, "1000 prompt · 200 completion") {
		t.Fatalf("stats:\n%s", rep)
	}
}

func TestHelperBuiltOnceUnderConcurrency(t *testing.T) {
	helper := &recProvider{name: "helper", def: provider.ChatResponse{Content: "x"}}
	var builds atomic.Int32
	old := HelperFactory
	HelperFactory = func(ctx context.Context, cfg *config.Config) (provider.Provider, string, int, error) {
		builds.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the race a second build would need
		return helper, "helper-model", 8192, nil
	}
	t.Cleanup(func() { HelperFactory = old })
	ag, _ := newTestAgent(t, &recProvider{name: "primary"}, helperCfg)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, h := ag.choreModel(context.Background()); !h {
				t.Error("chore not on the helper")
			}
		}()
	}
	wg.Wait()
	if builds.Load() != 1 {
		t.Fatalf("built %d times", builds.Load())
	}
}

func TestPlanModeSpendCountedWhileOnline(t *testing.T) {
	primary := &recProvider{name: "primary", def: provider.ChatResponse{Content: "1. do it",
		Usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 1000}}}
	ag, _ := newTestAgent(t, primary, func(c *config.Config) { c.MaxSpendUSD = 1 })
	ag.SetOnline("openrouter", "K", Pricing{Prompt: 1e-4, Completion: 1e-4, Known: true})
	if _, err := ag.Plan(context.Background(), "plan it"); err != nil {
		t.Fatal(err)
	}
	if got := ag.Usage().SpendUSD; got < 0.2-1e-9 || got > 0.2+1e-9 {
		t.Fatalf("plan spend %v, want 0.2", got)
	}
	// Past the cap, the plan's own call asks the primary's spend_cap.
	ag.addStats(Stats{SpendUSD: 1})
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action); return false }
	_, err := ag.Plan(context.Background(), "plan again")
	if err == nil || !strings.Contains(err.Error(), "spend cap reached") {
		t.Fatalf("plan past the cap: %v", err)
	}
	if len(asked) != 1 || asked[0] != "spend_cap" {
		t.Fatalf("asked %v", asked)
	}
	if len(primary.requests()) != 1 {
		t.Fatalf("a request went out past the cap: %d", len(primary.requests()))
	}
}
