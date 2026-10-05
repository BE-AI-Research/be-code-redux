package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// onlineAgent is a test agent whose main model is online at "name", with
// HOME (and so online.json) under the test's own temp dir.
func onlineAgent(t *testing.T, p provider.Provider, name string, mut func(*config.Config)) (*Agent, string) {
	t.Helper()
	ag, dir := newTestAgent(t, p, mut)
	ag.SetOnline(name, "K", Pricing{Known: true})
	return ag, dir
}

// twinOn builds a second agent on the same workspace as first.
func twinOn(t *testing.T, dir string, p provider.Provider) *Agent {
	t.Helper()
	reg, err := tools.NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.VerifyOnDone, cfg.CompatToolCalls, cfg.RepoMap = false, "never", false
	return New(cfg, p, "test-model", reg, "")
}

func onlineStoreFor(t *testing.T, dir string) string {
	t.Helper()
	base, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "engine", engine.Key(dir), "online.json")
}

func TestOnlineProjectAskedOnceAndRemembered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, dir := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", nil)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if !ag.StartOnlineGate() {
		t.Fatal("yes passes the gate")
	}
	want := "online_project|This project's files, command output and conversation will be sent to openrouter (test-model). Allow for this project?"
	if got := l.get(); len(got) != 1 || got[0] != want {
		t.Fatalf("asked %q", got)
	}
	st, err := os.Stat(onlineStoreFor(t, dir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store: %v %v", st, err)
	}

	// A second session on the same workspace is not asked.
	ag2 := twinOn(t, dir, &recProvider{name: "openrouter"})
	ag2.SetOnline("openrouter", "K", Pricing{})
	var l2 safeLog
	ag2.Tools.Approve = l2.approver(false)
	if !ag2.StartOnlineGate() || len(l2.get()) != 0 {
		t.Fatalf("remembered yes asked again: %v", l2.get())
	}

	// A different provider asks again; its yes is merged, not overwritten.
	ag3 := twinOn(t, dir, &recProvider{name: "together"})
	ag3.SetOnline("together", "K", Pricing{})
	var l3 safeLog
	ag3.Tools.Approve = l3.approver(true)
	if !ag3.StartOnlineGate() || len(l3.get()) != 1 {
		t.Fatalf("another provider not asked: %v", l3.get())
	}
	m := readOnlineStore(onlineStoreFor(t, dir))
	if !m["openrouter"] || !m["together"] {
		t.Fatalf("store %v", m)
	}
}

func TestOnlineProjectNoSwitchesToHelper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	helper := &recProvider{name: "ollama", def: provider.ChatResponse{Content: "local answer"}}
	withHelper(t, helper, 8192, nil)
	online := &recProvider{name: "openrouter"}
	ag, dir := onlineAgent(t, online, "openrouter", helperCfg)
	notes := noteSink(ag)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	if !ag.StartOnlineGate() {
		t.Fatal("a helper to fall back to keeps the session")
	}
	if _, on := ag.Online(); on || ag.Provider != provider.Provider(helper) || ag.CurrentModel() != "helper-model" {
		t.Fatalf("main model: online=%v provider=%v model=%s", on, ag.Provider.Name(), ag.CurrentModel())
	}
	if n := notes(); !strings.Contains(n, "you declined sending this project to openrouter") {
		t.Fatalf("notice %q", n)
	}
	if _, err := os.Stat(onlineStoreFor(t, dir)); err == nil {
		t.Fatal("a no is not remembered")
	}
	if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(online.requests()) != 0 || len(helper.requests()) == 0 {
		t.Fatalf("online %d helper %d", len(online.requests()), len(helper.requests()))
	}
}

func TestOnlineProjectNoWithoutHelperSendsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	online := &recProvider{name: "openrouter", def: provider.ChatResponse{Content: "x"}}
	ag, _ := onlineAgent(t, online, "openrouter", nil)
	notes := noteSink(ag)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	if ag.StartOnlineGate() {
		t.Fatal("no helper: the gate fails")
	}
	msg := "this project is not approved for openrouter and no local_helper is configured; nothing was sent"
	if !strings.Contains(notes(), msg) || ag.OnlineRefusal() != msg {
		t.Fatalf("notice %q refusal %q", notes(), ag.OnlineRefusal())
	}
	_, _, err := ag.RunFull(context.Background(), "hi")
	if err == nil || err.Error() != "waiting for your answer about sending this project to openrouter" {
		t.Fatalf("backstop: %v", err)
	}
	// Chores never ask and never send either.
	if err := ag.checkOnlineGate(context.Background(), false); err == nil {
		t.Fatal("chore passed the gate")
	}
	if _, err := ag.WriteHandoff(context.Background(), true); err != nil {
		t.Logf("handoff: %v", err)
	}
	if n := len(online.requests()); n != 0 {
		t.Fatalf("%d requests reached the online model", n)
	}
}

func TestOnlineProjectWithdrawnStoresNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	helper := &recProvider{name: "ollama"}
	withHelper(t, helper, 8192, nil)
	ag, dir := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", helperCfg)
	ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		tools.MarkWithdrawn(ctx)
		return false
	}
	if ag.StartOnlineGate() {
		t.Fatal("a withdrawn question does not pass")
	}
	if _, on := ag.Online(); !on || ag.OnlineRefusal() != "" {
		t.Fatal("withdrawn is not a no: no helper switch, no refusal")
	}
	if _, err := os.Stat(onlineStoreFor(t, dir)); err == nil {
		t.Fatal("nothing stored")
	}
}

func TestOnlineProjectYesFlagRunOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	online := &recProvider{name: "openrouter", def: provider.ChatResponse{Content: "done"}}
	ag, dir := onlineAgent(t, online, "openrouter", nil)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.ApproveOnlineForRun()
	if !ag.OnlineApproved() || !ag.StartOnlineGate() || len(l.get()) != 0 {
		t.Fatalf("-y run: asked %v", l.get())
	}
	if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(online.requests()) == 0 {
		t.Fatal("the run sent nothing")
	}
	if _, err := os.Stat(onlineStoreFor(t, dir)); err == nil {
		t.Fatal("-y must not write online.json")
	}
	if !strings.Contains(ag.OnlineReport(), "approved for this run only") {
		t.Fatalf("report:\n%s", ag.OnlineReport())
	}
}

func TestScheduledEventRefusedWhenNotApproved(t *testing.T) {
	calls := 0
	ag, clock, _ := schedAgent(t, countingProvider(&calls))
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	notes := noteSink(ag)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	queueOne(t, ag, clock, mk("tick", "every 30m"), true)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("an unapproved project's event must not run")
	}
	if !strings.Contains(notes(), `scheduled event "tick" was not run: this project is not approved for openrouter`) {
		t.Fatalf("notice %q", notes())
	}
	for _, a := range l.get() {
		if strings.HasPrefix(a, "online_project") {
			t.Fatal("a scheduled event never raises the question")
		}
	}
}

func TestOnlineGateRefusedInFiredTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", nil)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	ag.onlineMu.Lock()
	ag.gateInline = true
	ag.onlineMu.Unlock()
	ag.Tools.SetAllowance(schedule.Allowance{}, time.Second)
	defer ag.Tools.ClearAllowance()
	err := ag.checkOnlineGate(context.Background(), true)
	if err == nil || err.Error() != "this project is not approved for openrouter" || len(l.get()) != 0 {
		t.Fatalf("err %v asked %v", err, l.get())
	}
}

func TestForgetOnline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, dir := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", nil)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if !ag.StartOnlineGate() {
		t.Fatal("yes")
	}
	if err := ag.ForgetOnline(); err != nil {
		t.Fatal(err)
	}
	if !ag.OnlineApproved() {
		t.Fatal("this session keeps its yes")
	}
	ag2 := twinOn(t, dir, &recProvider{name: "openrouter"})
	ag2.SetOnline("openrouter", "K", Pricing{})
	var l2 safeLog
	ag2.Tools.Approve = l2.approver(true)
	ag2.StartOnlineGate()
	if len(l2.get()) != 1 {
		t.Fatalf("the next session asks again: %v", l2.get())
	}
	local, _ := newTestAgent(t, &recProvider{name: "ollama"}, nil)
	if local.ForgetOnline() == nil {
		t.Fatal("nothing to forget on a local model")
	}
}

func TestCorruptOnlineStoreIsNotApproved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, dir := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", nil)
	p := onlineStoreFor(t, dir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("{not json"), 0o600)
	if ag.OnlineApproved() {
		t.Fatal("corrupt store approved")
	}
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if !ag.StartOnlineGate() || !readOnlineStore(p)["openrouter"] {
		t.Fatal("a yes rewrites the corrupt store")
	}
}

func TestLocalSessionNeverAsks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := newTestAgent(t, &recProvider{name: "ollama", def: provider.ChatResponse{Content: "ok"}}, nil)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	called := false
	ag.StartOnlineGateAsync(func(ok bool) { called = ok })
	if !called || !ag.StartOnlineGate() || len(l.get()) != 0 {
		t.Fatal("a local session passes at once, on the caller's goroutine")
	}
	if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
}

func TestStartOnlineGateAsyncHoldsInputUntilAnswered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := onlineAgent(t, &recProvider{name: "openrouter"}, "openrouter", nil)
	release := make(chan struct{})
	ag.Tools.Approve = func(action, detail string) bool { <-release; return true }
	done := make(chan bool, 1)
	ag.StartOnlineGateAsync(func(ok bool) { done <- ok })
	if !ag.OnlineGatePending() {
		t.Fatal("pending as soon as it is started")
	}
	if err := ag.checkOnlineGate(context.Background(), true); err == nil ||
		err.Error() != "waiting for your answer about sending this project to openrouter" {
		t.Fatalf("backstop while pending: %v", err)
	}
	close(release)
	select {
	case ok := <-done:
		if !ok || ag.OnlineGatePending() {
			t.Fatal("answered yes")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never finished")
	}
}

// fakeResolver stands in for cmd's resolveOnline: "openrouter" is online,
// anything else local.
func fakeResolver(t *testing.T) {
	t.Helper()
	old := OnlineResolver
	OnlineResolver = func(_ context.Context, a *Agent, name, _ string) {
		if name == "openrouter" {
			a.SetOnline(name, "OPENROUTER_API_KEY", Pricing{Known: true})
			a.SetKeyEnv("OPENROUTER_API_KEY")
			return
		}
		a.SetOnline("", "", Pricing{})
		a.SetKeyEnv("")
	}
	t.Cleanup(func() { OnlineResolver = old })
}

func TestSwitchToLocalClearsOnlineState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeResolver(t)
	ag, _ := newTestAgent(t, &recProvider{name: "openrouter"}, nil)
	ag.SetModelNow(context.Background(), "vendor/m")
	if name, on := ag.Online(); !on || name != "openrouter" || ag.KeyEnv() != "OPENROUTER_API_KEY" {
		t.Fatalf("online %q %v key %q", name, on, ag.KeyEnv())
	}
	ag.SetProvider(&recProvider{name: "ollama"})
	ag.SetModel("qwen3")
	if _, on := ag.Online(); on || ag.KeyEnv() != "" {
		t.Fatal("a switch to a local provider leaves the session online")
	}
}

func TestSwitchToUnapprovedOnlineAsksInline(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Setenv("HOME", t.TempDir())
		fakeResolver(t)
		local := &recProvider{name: "ollama", def: provider.ChatResponse{Content: "local"}}
		ag, dir := newTestAgent(t, local, nil)
		notes := noteSink(ag)
		var l safeLog
		ag.Tools.Approve = l.approver(yes)
		if _, _, err := ag.RunFull(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		online := &recProvider{name: "openrouter", def: provider.ChatResponse{Content: "online"}}
		ag.SetProvider(online)
		ag.SetModelNow(context.Background(), "vendor/m")
		_, _, err := ag.RunFull(context.Background(), "second")
		if got := l.get(); len(got) != 1 || !strings.HasPrefix(got[0], "online_project|") {
			t.Fatalf("yes=%v: asked %v", yes, got)
		}
		if yes {
			if err != nil || len(online.requests()) == 0 || !readOnlineStore(onlineStoreFor(t, dir))["openrouter"] {
				t.Fatalf("yes: err %v requests %d", err, len(online.requests()))
			}
			continue
		}
		if err == nil || err.Error() != "not sent: this project is not approved for openrouter" {
			t.Fatalf("no: err %v", err)
		}
		if len(online.requests()) != 0 {
			t.Fatal("no: the request was sent")
		}
		if _, on := ag.Online(); on || ag.Provider != provider.Provider(local) || ag.CurrentModel() != "test-model" {
			t.Fatalf("no: not back on the local model (%s %s)", ag.Provider.Name(), ag.CurrentModel())
		}
		if !strings.Contains(notes(), "you declined sending this project to openrouter; back to test-model (ollama)") {
			t.Fatalf("notice %q", notes())
		}
	}
}
