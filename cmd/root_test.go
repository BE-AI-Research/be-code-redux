package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ide"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// ollamaProbeStub counts every management endpoint startup might reach for.
// /api/generate is the one that matters most: calling it is what "loading
// the model to read its window" was, and it is the multi-minute stall this
// wiring exists to delete.
type ollamaProbeStub struct {
	srv                *httptest.Server
	ps, show, generate int32
	psBody             string
}

func newOllamaProbeStub(t *testing.T, psBody string) *ollamaProbeStub {
	t.Helper()
	s := &ollamaProbeStub{psBody: psBody}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			atomic.AddInt32(&s.ps, 1)
			w.Write([]byte(s.psBody))
		case "/api/show":
			atomic.AddInt32(&s.show, 1)
			w.Write([]byte(`{"parameters":"num_ctx 4096"}`))
		case "/api/generate":
			atomic.AddInt32(&s.generate, 1)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func testAgentFor(t *testing.T, cfg *config.Config, p provider.Provider, model string) (*agent.Agent, *tools.Registry) {
	t.Helper()
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return agent.New(cfg, p, model, reg, ""), reg
}

// TestStartupAppliesAConfiguredWindowWithoutProbing: the whole point of
// context_window is that the harness stops asking. No /api/show, and above
// all no /api/generate — loading a 27B model to read a number the user
// already wrote down is minutes of nothing.
func TestStartupAppliesAConfiguredWindowWithoutProbing(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0 // unset: the window is the budget
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 32768 {
		t.Fatalf("window %d", ag.Window())
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d never reached the provider", p.Options().NumCtx)
	}
	if atomic.LoadInt32(&stub.show) != 0 {
		t.Fatalf("probed the Modelfile %d times with an explicit window", stub.show)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("startup loaded the model to read its window; that stall is what this replaced")
	}
}

// TestStartupNeverReloadsAModelNobodyAskedAbout: the model is resident at
// 8192 for somebody else and config wants 32768. Nothing has wired an
// approver during buildAgent, so there is no one to ask — and no one to ask
// means no, not yes. The session runs inside the window it found.
func TestStartupNeverReloadsAModelNobodyAskedAbout(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 8192 {
		t.Fatalf("window %d; an unasked reload is a change to somebody else's server", ag.Window())
	}
	if p.Options().NumCtx != 8192 {
		t.Fatalf("num_ctx %d; our own requests must not reload it either", p.Options().NumCtx)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("the model was loaded at startup")
	}
}

// TestStartupConsentIsDeferredNotDenied: the registry's approver is the seam
// the UI wires *after* buildAgent, and the loader must read it when it asks
// rather than capture the nil it saw at startup. Otherwise a model switch
// later in the session could never ask either.
func TestStartupConsentIsDeferredNotDenied(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")
	if ag.Window() != 8192 {
		t.Fatalf("window %d before an approver existed", ag.Window())
	}

	// The UI comes up and wires its approver, exactly as tui/ui do.
	var asked string
	reg.Approve = func(action, detail string) bool { asked = action; return true }
	if w, err := ag.Loader().Apply(t.Context(), "m"); err != nil || w != 32768 {
		t.Fatalf("window %d err %v; the question must still be askable", w, err)
	}
	if asked != "model_reload" {
		t.Fatalf("asked %q", asked)
	}
}

// TestStartupFitsTheServerWhenNothingIsConfigured: 0.10.0's behaviour, which
// still has to work — and the Modelfile is the fallback when /api/ps has
// nothing, which is the one probe that stays.
func TestStartupFitsTheServerWhenNothingIsConfigured(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	before := ag.History.Budget
	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.Window() != 4096 {
		t.Fatalf("window %d; the Modelfile num_ctx is the answer here", ag.Window())
	}
	// The advice must name the budget the session was going to use, not the
	// one it has just been cut down to: "clamped to 4096 ... start the server
	// with OLLAMA_CONTEXT_LENGTH=4096" tells the user to ask for what they
	// already have.
	if !strings.Contains(out, fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", before)) {
		t.Fatalf("advice does not name a window worth asking for (budget was %d):\n%s", before, out)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("an unknown window must not be resolved by loading the model")
	}
}

// TestStartupLeavesNonOllamaBackendsAlone: there is no window to set and no
// budget to clamp on an OpenAI-compatible endpoint.
func TestStartupLeavesNonOllamaBackendsAlone(t *testing.T) {
	cfg := config.Default()
	p := provider.NewOpenAICompat("x", "http://127.0.0.1:1/v1", "")
	ag, reg := testAgentFor(t, cfg, p, "m")
	before := ag.History.Budget

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 0 || ag.History.Budget != before {
		t.Fatalf("window %d budget %d", ag.Window(), ag.History.Budget)
	}
}

// TestConfiguredWindowAboveContextTokensIsNotLostSilently: the complaint that
// started this work. context_tokens caps the budget below the window, and
// because the budget was already under the window ApplyWindow reports no
// clamp — so before this, half a deliberately configured 32768-token window
// vanished with nothing printed at all. Note that this test does *not* zero
// cfg.ContextTokens: every existing config file on disk carries a literal,
// and that is exactly the case that was invisible.
func TestConfiguredWindowAboveContextTokensIsNotLostSilently(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 16384 // what every pre-existing config.json says
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.Window() != 32768 {
		t.Fatalf("window %d", ag.Window())
	}
	if ag.History.Budget != 16384 {
		t.Fatalf("budget %d; an explicit context_tokens still wins", ag.History.Budget)
	}
	if !strings.Contains(out, "context_tokens=16384") || !strings.Contains(out, "go unused") {
		t.Fatalf("the loss was not reported:\n%s", out)
	}
}

// TestDerivedBudgetUsesTheWholeWindowQuietly: with no context_tokens the
// window is the budget, and there is nothing to warn about.
func TestDerivedBudgetUsesTheWholeWindowQuietly(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default() // ContextTokens is 0 here by design now
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.History.Budget != 32768 {
		t.Fatalf("budget %d; an unset context_tokens derives from the window", ag.History.Budget)
	}
	if strings.Contains(out, "warn:") {
		t.Fatalf("nothing is wrong here:\n%s", out)
	}
}

// Ruling T8-a, first half. buildAgent runs before any UI exists, so the
// consent question has nobody to put it to and is refused — correctly, but
// the session then runs at whatever window the server happened to hold,
// with the explanation on stderr, which under a TUI is wiped and in a
// hosted session is a log file. runInteractive and runSessionHost re-run
// the resolution once Registry.Approve is wired; this is that re-run.
func TestTheDeferredQuestionIsAskedOnceAUIExists(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")
	if ag.Window() != 8192 {
		t.Fatalf("window %d before an approver existed", ag.Window())
	}

	// The UI comes up: it wires the approval seam and re-runs the loader,
	// exactly as runInteractive and runSessionHost now do.
	asked := make(chan string, 1)
	reg.Approve = func(action, detail string) bool { asked <- action; return true }
	ag.ResolveModel()

	select {
	case action := <-asked:
		if action != "model_reload" {
			t.Fatalf("asked %q", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the question was never put to the UI")
	}
	deadline := time.Now().Add(5 * time.Second)
	for ag.Window() != 32768 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ag.Window() != 32768 {
		t.Fatalf("window %d; the consented window never reached the session", ag.Window())
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d never reached the provider", p.Options().NumCtx)
	}
}

// Ruling T8-a, second half. A loader notice is only useful where it can be
// read: the transcript when a UI is up, stderr only while one is not.
func TestLoaderNoticesGoToTheTranscriptWhenThereIsOne(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.ReloadOnMismatch = "never" // the loader explains itself and keeps 8192
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	var mu sync.Mutex
	var notices []string
	ag.Events.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	mu.Lock()
	got := strings.Join(notices, "\n")
	mu.Unlock()
	if !strings.Contains(got, "reload_on_mismatch") {
		t.Fatalf("the reason never reached the transcript: %q", got)
	}
	if strings.Contains(out, "reload_on_mismatch") {
		t.Fatalf("it went to stderr as well, where a TUI wipes it:\n%s", out)
	}
}

// A reviewer or co-worker provider is built fresh and used to skip the loader,
// so its requests carried no num_ctx — and on Ollama an absent num_ctx means
// the server default, which reloads a model someone else holds. It now goes
// through a loader with no approver: it never asks, never reloads, and puts
// the window the server already holds on the wire.
func TestASecondaryProviderSendsTheWindowTheServerHolds(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"rev","model":"rev","context_length":8192}]}`)
	cfg := config.Default()
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"rev": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")

	secondaryLoad(cfg, p, "rev", nil)

	if got := p.Options().NumCtx; got != 8192 {
		t.Fatalf("num_ctx %d on the wire; a secondary model must keep the server's 8192, never reload to 32768 and never send nothing", got)
	}
	if n := atomic.LoadInt32(&stub.generate); n != 0 {
		t.Fatalf("a secondary provider loaded a model %d time(s)", n)
	}
}

// --- chooseIDELock: quiet-path Visual Studio auto-attach (spec §6) ---

// writeIDELock stores a lock as <pid>-<port>.json, matching internal/ide's
// own test fixtures, so chooseIDELock (via ide.Discover/DiscoverCovering)
// finds it.
func writeIDELock(t *testing.T, dir string, l ide.Lock, mtime time.Time) string {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, strconv.Itoa(l.PID)+"-"+strconv.Itoa(l.Port)+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// A live Visual Studio lock covering the workspace attaches on the quiet
// path (no --ide, no TERM_PROGRAM=vscode) — spec §6.
func TestChooseIDELockAttachesToCoveringVisualStudioQuietly(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())

	lock, warn, err := chooseIDELock(dir, "/tmp/proj/sub", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil || lock.IDEName != "visualstudio" {
		t.Fatalf("got lock=%+v", lock)
	}
	if warn {
		t.Fatal("quiet path must never warn")
	}
}

// A VS Code lock does not auto-attach on the quiet path: VS Code still
// needs its own terminal (TERM_PROGRAM=vscode) or --ide.
func TestChooseIDELockIgnoresVSCodeLockQuietly(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())

	lock, warn, err := chooseIDELock(dir, "/tmp/proj", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if lock != nil {
		t.Fatalf("got lock=%+v, want nil", lock)
	}
	if warn {
		t.Fatal("quiet path must never warn")
	}
}

// A Visual Studio lock for a different workspace does not attach: covering
// is required, not just "some Visual Studio is open somewhere".
func TestChooseIDELockIgnoresVisualStudioForOtherWorkspace(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{"/tmp/other"}}, time.Now())

	lock, _, err := chooseIDELock(dir, "/tmp/proj", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if lock != nil {
		t.Fatalf("got lock=%+v, want nil", lock)
	}
}

// --ide still attaches to either kind of lock, using Discover's ordinary
// fallback-to-newest behaviour.
func TestChooseIDELockFlagAttachesToEitherKind(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())

	// warnIfMissing only matters to the caller when lock is nil (attachIDE
	// checks it under `lock == nil`), so it is not asserted here.
	lock, _, err := chooseIDELock(dir, "/tmp/proj", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil || lock.IDEName != "visualstudio" {
		t.Fatalf("got lock=%+v", lock)
	}

	dir2 := t.TempDir()
	writeIDELock(t, dir2, ide.Lock{PID: me, Port: 2, Token: "b", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())
	lock2, _, err := chooseIDELock(dir2, "/tmp/proj", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if lock2 == nil || lock2.IDEName != "vscode" {
		t.Fatalf("got lock=%+v", lock2)
	}
}

// --ide with nothing listening still warns (today's behaviour, unchanged).
func TestChooseIDELockFlagWarnsWhenNothingListening(t *testing.T) {
	dir := t.TempDir()
	lock, warn, err := chooseIDELock(dir, "/tmp/proj", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if lock != nil {
		t.Fatalf("got lock=%+v, want nil", lock)
	}
	if !warn {
		t.Fatal("--ide with no lock found must warn")
	}
}

// TERM_PROGRAM=vscode keeps today's exact behaviour: Discover's
// fallback-to-newest, not the quiet path's visualstudio-only filter.
func TestChooseIDELockVSCodeTerminalUsesOrdinaryDiscover(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	// Nothing covers /tmp/proj, so ordinary Discover falls back to newest.
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/elsewhere"}}, time.Now())

	lock, warn, err := chooseIDELock(dir, "/tmp/proj", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil || lock.Port != 1 {
		t.Fatalf("got lock=%+v, want the fallback lock", lock)
	}
	if warn {
		t.Fatal("TERM_PROGRAM=vscode without --ide must not warn when nothing covers")
	}
}

// A newer vscode lock and an older visualstudio lock both cover the
// workspace; on the quiet path the visualstudio one wins, since a covering
// vscode lock never auto-attaches without its own terminal or --ide.
func TestChooseIDELockPrefersVisualStudioOverNewerCoveringVSCodeQuietly(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	now := time.Now()
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 1, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{"/tmp/proj"}}, now.Add(-time.Hour))
	writeIDELock(t, dir, ide.Lock{PID: me, Port: 2, Token: "b", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/proj"}}, now)

	lock, warn, err := chooseIDELock(dir, "/tmp/proj", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil || lock.IDEName != "visualstudio" || lock.Port != 1 {
		t.Fatalf("got lock=%+v, want the visualstudio lock", lock)
	}
	if warn {
		t.Fatal("quiet path must never warn")
	}
}

// --no-ide is handled entirely in ideLockToAttach before chooseIDELock would
// ever be consulted; this documents that attachIDE's own early return (via
// ideLockToAttach) means chooseIDELock's decision is never reached, rather
// than duplicating the flag inside chooseIDELock itself.
func TestNoIDEFlagShortCircuitsAttachIDE(t *testing.T) {
	origIDE, origNoIDE := flagIDE, flagNoIDE
	t.Cleanup(func() { flagIDE, flagNoIDE = origIDE, origNoIDE })
	flagIDE = true
	flagNoIDE = true

	cfg := config.Default()
	reg := &tools.Registry{Root: t.TempDir()}
	if sess := attachIDE(cfg, reg, nil, false); sess != nil {
		t.Fatalf("got session %+v, want nil", sess)
	}
}

// --- ideLockToAttach: the wiring attachIDE relies on before it ever dials
// anything (TERM_PROGRAM, ide.enabled, headless, ide.LockDir() itself) ---
//
// These tests write real lock files into the package's isolated
// ide.LockDir() (HOME is pointed at a throwaway directory for the whole
// package by TestMain in testmain_test.go) rather than passing a directory
// in, specifically so a typo in the TERM_PROGRAM comparison or a swapped
// gate is caught: chooseIDELock's own unit tests take vscodeTerminal as a
// parameter and so can never exercise the os.Getenv call or the gates ahead
// of it.

// requireIsolatedIDELockDir returns ide.LockDir(), first asserting it is
// underneath the package's isolated test HOME. These tests write real lock
// files, so this must fail loudly rather than ever reach a developer's
// actual ~/.be-code/ide.
func requireIsolatedIDELockDir(t *testing.T) string {
	t.Helper()
	dir, err := ide.LockDir()
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	if home == "" || !strings.HasPrefix(dir, home) || !strings.Contains(dir, "be-code-test-home") {
		t.Fatalf("refusing to write a lock file: ide.LockDir() = %q is not under the isolated test HOME (HOME=%q); is testmain_test.go's TestMain guard still in place?", dir, home)
	}
	return dir
}

// writeLiveIDELock writes a lock file into dir (the isolated ide.LockDir())
// and removes it when the test ends, so lock files from one test never leak
// into another sharing the same package-wide temp HOME.
func writeLiveIDELock(t *testing.T, dir string, l ide.Lock, mtime time.Time) {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, strconv.Itoa(l.PID)+"-"+strconv.Itoa(l.Port)+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(p) })
}

// setIDEFlags sets flagIDE/flagNoIDE for the duration of the test and
// restores them on cleanup.
func setIDEFlags(t *testing.T, ideFlag, noIDEFlag bool) {
	t.Helper()
	origIDE, origNoIDE := flagIDE, flagNoIDE
	t.Cleanup(func() { flagIDE, flagNoIDE = origIDE, origNoIDE })
	flagIDE, flagNoIDE = ideFlag, noIDEFlag
}

// captureStderr (used below) is already declared in engine_test.go.

// TERM_PROGRAM="" + ide.enabled + a live visualstudio lock covering the
// workspace: attaches. This is the case the quiet path exists for.
func TestIDELockToAttachQuietPathAttachesToCoveringVisualStudio(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "")
	setIDEFlags(t, false, false)
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41101, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{ws}}, time.Now())

	cfg := config.Default()
	cfg.IDE.Enabled = true

	lock := ideLockToAttach(cfg, false, ws)
	if lock == nil || lock.IDEName != "visualstudio" {
		t.Fatalf("got %+v, want the visualstudio lock", lock)
	}
}

// TERM_PROGRAM="" + ide.enabled + only a covering vscode lock: does not
// attach — VS Code still needs its own terminal or --ide.
func TestIDELockToAttachQuietPathIgnoresCoveringVSCode(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "")
	setIDEFlags(t, false, false)
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41102, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{ws}}, time.Now())

	cfg := config.Default()
	cfg.IDE.Enabled = true

	lock := ideLockToAttach(cfg, false, ws)
	if lock != nil {
		t.Fatalf("got %+v, want nil", lock)
	}
}

// TERM_PROGRAM="vscode" + ide.enabled + a covering vscode lock: attaches,
// exactly as before this change.
func TestIDELockToAttachVSCodeTerminalAttachesToCoveringVSCode(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "vscode")
	setIDEFlags(t, false, false)
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41103, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{ws}}, time.Now())

	cfg := config.Default()
	cfg.IDE.Enabled = true

	lock := ideLockToAttach(cfg, false, ws)
	if lock == nil || lock.IDEName != "vscode" {
		t.Fatalf("got %+v, want the vscode lock", lock)
	}
}

// ide.enabled=false blocks discovery entirely unless --ide overrides it.
func TestIDELockToAttachRespectsEnabledAndIDEFlag(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "")
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41104, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{ws}}, time.Now())
	cfg := config.Default()
	cfg.IDE.Enabled = false

	setIDEFlags(t, false, false)
	if lock := ideLockToAttach(cfg, false, ws); lock != nil {
		t.Fatalf("ide.enabled=false without --ide: got %+v, want nil", lock)
	}

	setIDEFlags(t, true, false)
	lock := ideLockToAttach(cfg, false, ws)
	if lock == nil || lock.IDEName != "visualstudio" {
		t.Fatalf("--ide with ide.enabled=false: got %+v, want the visualstudio lock", lock)
	}
}

// A headless run never discovers unless --ide overrides it (existing guard,
// unchanged).
func TestIDELockToAttachRespectsHeadlessAndIDEFlag(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "")
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41105, Token: "a", IDEName: "visualstudio", WorkspaceFolders: []string{ws}}, time.Now())
	cfg := config.Default()
	cfg.IDE.Enabled = true

	setIDEFlags(t, false, false)
	if lock := ideLockToAttach(cfg, true, ws); lock != nil {
		t.Fatalf("headless without --ide: got %+v, want nil", lock)
	}

	setIDEFlags(t, true, false)
	lock := ideLockToAttach(cfg, true, ws)
	if lock == nil || lock.IDEName != "visualstudio" {
		t.Fatalf("headless with --ide: got %+v, want the visualstudio lock", lock)
	}
}

// --no-ide wins over --ide, ide.enabled, and a covering lock that would
// otherwise attach on any path.
func TestIDELockToAttachNoIDEBeatsEverything(t *testing.T) {
	dir := requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "vscode")
	ws := t.TempDir()
	writeLiveIDELock(t, dir, ide.Lock{PID: os.Getpid(), Port: 41106, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{ws}}, time.Now())
	cfg := config.Default()
	cfg.IDE.Enabled = true
	setIDEFlags(t, true, true) // both --ide and --no-ide given

	if lock := ideLockToAttach(cfg, false, ws); lock != nil {
		t.Fatalf("--no-ide must win: got %+v, want nil", lock)
	}
}

// The quiet path with no live lock at all attaches nothing and prints
// nothing to stderr — silent terminals are the default.
func TestIDELockToAttachQuietPathNothingLiveIsSilent(t *testing.T) {
	requireIsolatedIDELockDir(t)
	t.Setenv("TERM_PROGRAM", "")
	setIDEFlags(t, false, false)
	ws := t.TempDir()
	cfg := config.Default()
	cfg.IDE.Enabled = true

	var lock *ide.Lock
	stderr := captureStderr(t, func() {
		lock = ideLockToAttach(cfg, false, ws)
	})
	if lock != nil {
		t.Fatalf("got %+v, want nil", lock)
	}
	if stderr != "" {
		t.Fatalf("quiet path printed to stderr: %q", stderr)
	}
}
