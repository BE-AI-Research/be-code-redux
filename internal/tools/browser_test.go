package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

type askLog struct {
	mu      sync.Mutex
	actions []string
	details []string
}

func (l *askLog) approver(answer bool) ApproveFunc {
	return func(action, detail string) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.actions = append(l.actions, action)
		l.details = append(l.details, detail)
		return answer
	}
}

func (l *askLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.actions)
}

// browserFixture is a registry with the browser tool attached to a scripted
// browser showing the sign-in fixture at url, its password field sensitive.
func browserFixture(t *testing.T, url string, sites map[string]string, approve ApproveFunc) (*Registry, *BrowserTool, *browsertest.PageScript, *browsertest.Browser) {
	t.Helper()
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, url, "Sign in — Acme", browsertest.FormTree)
	ps.Lock()
	ps.Sensitive = map[int][]string{60: {"type", "password"}}
	ps.Unlock()
	reg, err := NewRegistry(t.TempDir(), approve)
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), Sites: sites, SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	return reg, bt, ps, fb
}

func do(bt *BrowserTool, args map[string]any) Result { return bt.Run(context.Background(), args) }

func noMouse(ps *browsertest.PageScript) bool {
	for _, in := range ps.Inputs() {
		if strings.HasPrefix(in, "mouse") {
			return false
		}
	}
	return true
}

func TestBrowserEveryResultStartsWithTheHeader(t *testing.T) {
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	res := do(bt, map[string]any{"action": "snapshot"})
	if res.IsError || !strings.HasPrefix(res.Content, WebHeader+"\n") || !strings.Contains(res.Content, `textbox "Email" [e1]`) {
		t.Fatalf("result:\n%s", res.Content)
	}
}

func TestBrowserDefaultTierAsksOnceThenRemembers(t *testing.T) {
	var log askLog
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	if res := do(bt, map[string]any{"action": "click", "ref": "e4"}); res.IsError {
		t.Fatalf("click: %s", res.Content)
	}
	if log.count() != 1 || log.actions[0] != "browser" {
		t.Fatalf("asks %v", log.actions)
	}
	for _, want := range []string{"act on acme.test?", `click button "Sign in" [e4]`, "y allows acme.test for the rest of this session"} {
		if !strings.Contains(log.details[0], want) {
			t.Fatalf("prompt lacks %q:\n%s", want, log.details[0])
		}
	}
	do(bt, map[string]any{"action": "click", "ref": "e4"})
	if log.count() != 1 {
		t.Fatal("a granted site asked again")
	}
}

func TestBrowserDeclineIsReportedAndNothingIsClicked(t *testing.T) {
	var log askLog
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(false))
	do(bt, map[string]any{"action": "snapshot"})
	res := do(bt, map[string]any{"action": "click", "ref": "e4"})
	if !res.IsError || res.Content != `the user declined: click button "Sign in" [e4] on acme.test` {
		t.Fatalf("result %q", res.Content)
	}
	if !noMouse(ps) {
		t.Fatal("a declined click was clicked")
	}
}

func TestBrowserWatchAsksEveryTime(t *testing.T) {
	var log askLog
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", map[string]string{"acme.test": "watch"}, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	do(bt, map[string]any{"action": "click", "ref": "e4"})
	do(bt, map[string]any{"action": "click", "ref": "e4"})
	if log.count() != 2 || log.actions[0] != "browser_watch" || !strings.Contains(log.details[1], "(watched: every action asks)") {
		t.Fatalf("asks %v %q", log.actions, log.details)
	}
}

func TestBrowserDenyRefusesWithoutAsking(t *testing.T) {
	var log askLog
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", map[string]string{"acme.test": "deny"}, log.approver(true))
	if res := do(bt, map[string]any{"action": "snapshot"}); res.IsError {
		t.Fatal("reading a denied site must still work")
	}
	res := do(bt, map[string]any{"action": "click", "ref": "e4"})
	if !res.IsError || res.Content != "interacting with acme.test is denied by browser.sites; you can still read it" {
		t.Fatalf("result %q", res.Content)
	}
	if log.count() != 0 || !noMouse(ps) {
		t.Fatal("a denied site asked or acted")
	}
}

func TestBrowserLoopbackIsAllowed(t *testing.T) {
	var log askLog
	_, bt, _, _ := browserFixture(t, "http://localhost:3000/", nil, log.approver(false))
	do(bt, map[string]any{"action": "snapshot"})
	if res := do(bt, map[string]any{"action": "click", "ref": "e4"}); res.IsError {
		t.Fatalf("click on localhost: %s", res.Content)
	}
	if log.count() != 0 {
		t.Fatal("localhost asked")
	}
}

func TestBrowserWithNoApproverRefuses(t *testing.T) {
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	do(bt, map[string]any{"action": "snapshot"})
	if res := do(bt, map[string]any{"action": "click", "ref": "e4"}); !res.IsError || !strings.HasPrefix(res.Content, "the user declined") {
		t.Fatalf("result %q", res.Content)
	}
	if !noMouse(ps) {
		t.Fatal("clicked with nobody to ask")
	}
}

func TestBrowserNeverTypesIntoAPasswordField(t *testing.T) {
	var log askLog
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	res := do(bt, map[string]any{"action": "type", "ref": "e2", "text": "hunter3"})
	if !res.IsError || res.Content != "sign-in is yours — ask the user to sign in in the browser window, then continue" {
		t.Fatalf("result %q", res.Content)
	}
	if log.count() != 0 {
		t.Fatal("asked to approve typing a password")
	}
	for _, in := range ps.Inputs() {
		if strings.HasPrefix(in, "text ") {
			t.Fatal("typed into a password field")
		}
	}
}

func TestBrowserMarksUntrustedWeb(t *testing.T) {
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	do(bt, map[string]any{"action": "snapshot"})
	if !reg.UntrustedWeb() {
		t.Fatal("reading acme.test did not mark the request")
	}
	reg.ClearUntrustedWeb()
	if reg.UntrustedWeb() {
		t.Fatal("Clear did not clear")
	}
	reg2, bt2, _, _ := browserFixture(t, "http://localhost:3000/", nil, nil)
	do(bt2, map[string]any{"action": "snapshot"})
	if reg2.UntrustedWeb() {
		t.Fatal("an allow-tier page marked the request")
	}
}

func TestBrowserActionsAliasesAndUsage(t *testing.T) {
	_, bt, _, fb := browserFixture(t, "https://acme.test/login", nil, nil)
	if res := do(bt, map[string]any{"action": "navigate", "url": "acme.test/next"}); res.IsError {
		t.Fatalf("navigate: %s", res.Content)
	}
	if len(fb.Calls("Page.navigate")) != 1 {
		t.Fatal("the navigate alias did not open the page")
	}
	if res := do(bt, map[string]any{"action": "fly"}); !res.IsError || !strings.Contains(res.Content, `unknown browser action "fly"`) {
		t.Fatalf("unknown action: %q", res.Content)
	}
	if res := do(bt, map[string]any{}); !res.IsError || !strings.HasPrefix(res.Content, "browser needs an action") {
		t.Fatalf("no action: %q", res.Content)
	}
}

func TestBrowserStaleRefComesBackWithTheSnapshot(t *testing.T) {
	var log askLog
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	ps.Lock()
	ps.Disconnected[80] = true
	ps.Unlock()
	res := do(bt, map[string]any{"action": "click", "ref": "14"}) // loose ref form, unknown element
	if !res.IsError || !strings.Contains(res.Content, "e14 is not an element on this page") || !strings.Contains(res.Content, "page: Sign in — Acme") {
		t.Fatalf("unknown ref:\n%s", res.Content)
	}
	res = do(bt, map[string]any{"action": "click", "ref": "e4"})
	if !res.IsError || !strings.Contains(res.Content, "e4 is no longer on the page") || !strings.Contains(res.Content, "page: Sign in — Acme") {
		t.Fatalf("stale ref:\n%s", res.Content)
	}
}

func TestBrowserTabsListIsHeaded(t *testing.T) {
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	do(bt, map[string]any{"action": "snapshot"}) // connect first: the first result carries "attached to …"
	res := do(bt, map[string]any{"action": "tabs"})
	if res.IsError || !strings.HasPrefix(res.Content, WebHeader+"\ntabs:\n  1. Sign in — Acme — acme.test/login (current)") {
		t.Fatalf("tabs:\n%s", res.Content)
	}
}

func TestBrowserStatusForgetAndClose(t *testing.T) {
	var log askLog
	reg, bt, _, fb := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	if got := bt.StatusLines(); got[0] != "browser: not connected (it starts on the model's first browser call)" {
		t.Fatalf("before: %q", got)
	}
	do(bt, map[string]any{"action": "snapshot"})
	do(bt, map[string]any{"action": "click", "ref": "e4"})
	got := bt.StatusLines()
	want := []string{"browser: attached to FakeChrome/1.0 at " + fb.Addr(), "tab: Sign in — Acme — acme.test/login", "allowed this session: acme.test"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("status:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !bt.Forget("acme.test") || bt.Forget("acme.test") {
		t.Fatal("Forget")
	}
	reg.Close()
	if got := bt.StatusLines(); got[0] != "browser: not connected (it starts on the model's first browser call)" {
		t.Fatalf("after Registry.Close: %q", got)
	}
}

// TestBrowserBlankHostAlwaysAsksAsWatch: HostOf("about:blank") is "", which
// is TierAsk (not Allow, since 2026-09-25) — but a page with no address is
// never granted a session-wide "always" either (fix round 1, item 3): every
// interaction on it asks, watch-style, action browser_watch, and the prompt
// never shows an empty host.
func TestBrowserBlankHostAlwaysAsksAsWatch(t *testing.T) {
	var log askLog
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "about:blank", "Sign in — Acme", browsertest.FormTree)
	reg, err := NewRegistry(t.TempDir(), log.approver(true))
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	do(bt, map[string]any{"action": "snapshot"})
	if res := do(bt, map[string]any{"action": "click", "ref": "e4"}); res.IsError {
		t.Fatalf("click 1: %s", res.Content)
	}
	if res := do(bt, map[string]any{"action": "click", "ref": "e4"}); res.IsError {
		t.Fatalf("click 2: %s", res.Content)
	}
	if log.count() != 2 || log.actions[0] != "browser_watch" || log.actions[1] != "browser_watch" {
		t.Fatalf("asks %v", log.actions)
	}
	for _, d := range log.details {
		if !strings.Contains(d, "act on a page with no address (about:blank)? (every action on a page with no address asks)") {
			t.Fatalf("prompt lacks the blank-host phrasing:\n%s", d)
		}
		if strings.Contains(d, "y allows") {
			t.Fatalf("a blank-host prompt offered \"always\":\n%s", d)
		}
	}
}

// TestBrowserRefusesWhenThePageMovedWhileWaitingForApproval (fix round 1,
// item 2): the host gate judged and the host the interaction would actually
// run on can differ if the page navigates while the approval prompt is
// open. press has no ref to protect it (unlike click/type/select, whose ref
// resolution fails on a new document), so Run must re-check the host itself.
func TestBrowserRefusesWhenThePageMovedWhileWaitingForApproval(t *testing.T) {
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree)
	approve := func(action, detail string) bool {
		fb.Emit("S-T1", "Page.frameNavigated", map[string]any{
			"frame": map[string]any{"id": "F-T1", "loaderId": "L2", "url": "https://evil.test/"},
		})
		time.Sleep(50 * time.Millisecond) // let the event land before answering
		return true
	}
	reg, err := NewRegistry(t.TempDir(), approve)
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	do(bt, map[string]any{"action": "snapshot"})
	res := do(bt, map[string]any{"action": "press", "key": "Enter"})
	if !res.IsError || !strings.Contains(res.Content, "the page moved to evil.test while waiting for approval; take a snapshot and try again") {
		t.Fatalf("result %q", res.Content)
	}
	for _, in := range ps.Inputs() {
		if strings.HasPrefix(in, "key ") {
			t.Fatal("a key was dispatched after the page moved out from under the approval")
		}
	}
}

// TestBrowserDescribePressIsQuoted (fix round 1, item 5): describe()'s
// "press" case must quote the key, matching type/select.
func TestBrowserDescribePressIsQuoted(t *testing.T) {
	var log askLog
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", map[string]string{"acme.test": "watch"}, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	do(bt, map[string]any{"action": "press", "key": "Enter"})
	if log.count() != 1 || !strings.Contains(log.details[0], `press "Enter"`) {
		t.Fatalf("asks %v %q", log.actions, log.details)
	}
}

// TestBrowserSnapshotFailureIsHeadedAndMarksUntrusted (fix round 1, item 4):
// the "reading the page failed:" result must still carry the header (it can
// include page-controlled notes, like an alert's message) and must still
// mark the request untrusted.
func TestBrowserSnapshotFailureIsHeadedAndMarksUntrusted(t *testing.T) {
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree)
	reg, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	do(bt, map[string]any{"action": "snapshot"}) // connect first
	fb.Emit("S-T1", "Page.javascriptDialogOpening", map[string]any{"type": "alert", "message": "hi"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, in := range ps.Inputs() {
			if in == "dialog accept=true" {
				found = true
			}
		}
		if found || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fb.Handle("Accessibility.getFullAXTree", func(string, json.RawMessage) (any, error) {
		return nil, errors.New("boom")
	})
	res := do(bt, map[string]any{"action": "snapshot"})
	if !res.IsError || !strings.HasPrefix(res.Content, WebHeader+"\n") {
		t.Fatalf("result:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, `alert: "hi"`) {
		t.Fatalf("missing the alert note:\n%s", res.Content)
	}
	if !reg.UntrustedWeb() {
		t.Fatal("a failed snapshot on an untrusted host did not mark the request")
	}
}

// TestBrowserTypeRefusesWhenFieldBecomesSensitiveAtWriteTime (fix round 1,
// item 6a): the write-time re-check inside browser.Page.Type, not the
// gate's own earlier IsSensitive check, is what fires here — the field was
// ordinary when the gate looked at it (so the model's own consent prompt
// still ran) and only turned sensitive while that prompt was open.
func TestBrowserTypeRefusesWhenFieldBecomesSensitiveAtWriteTime(t *testing.T) {
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree)
	approve := func(action, detail string) bool {
		ps.Lock()
		ps.Sensitive[40] = []string{"autocomplete", "one-time-code"} // e1, the email field
		ps.Unlock()
		return true
	}
	reg, err := NewRegistry(t.TempDir(), approve)
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	do(bt, map[string]any{"action": "snapshot"})
	res := do(bt, map[string]any{"action": "type", "ref": "e1", "text": "oops"})
	if !res.IsError || res.Content != "sign-in is yours — ask the user to sign in in the browser window, then continue" {
		t.Fatalf("result %q", res.Content)
	}
	for _, in := range ps.Inputs() {
		if strings.HasPrefix(in, "text ") {
			t.Fatal("typed into a field that became sensitive mid-approval")
		}
	}
}

// TestBrowserReadMarksUntrustedWeb (fix round 1, item 6b).
func TestBrowserReadMarksUntrustedWeb(t *testing.T) {
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	if res := do(bt, map[string]any{"action": "read"}); res.IsError {
		t.Fatalf("read: %s", res.Content)
	}
	if !reg.UntrustedWeb() {
		t.Fatal("read on an untrusted host did not mark the request")
	}
}

// TestBrowserBlankHostSnapshotMarksUntrustedWeb (fix round 1, item 6b).
func TestBrowserBlankHostSnapshotMarksUntrustedWeb(t *testing.T) {
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "about:blank", "", browsertest.EmptyTree)
	reg, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bt := NewBrowser(BrowserConfig{Address: fb.Addr(), SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	reg.AddTool(bt)
	t.Cleanup(reg.Close)
	if res := do(bt, map[string]any{"action": "snapshot"}); res.IsError {
		t.Fatalf("snapshot: %s", res.Content)
	}
	if !reg.UntrustedWeb() {
		t.Fatal("a blank-host snapshot did not mark the request")
	}
}

// TestBrowserFillAliasIsGated (fix round 1, item 6c): "fill" aliases to
// "type", which must still gate consent like the canonical name.
func TestBrowserFillAliasIsGated(t *testing.T) {
	var log askLog
	_, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	do(bt, map[string]any{"action": "fill", "ref": "e1", "text": "ann@example.com"})
	if log.count() != 1 || log.actions[0] != "browser" {
		t.Fatalf("the fill alias did not gate: %v", log.actions)
	}
}

// TestShellAfterUntrustedWebWithNoApproverRefuses (fix round 1, item 1): a
// nil approver on the shell_after_web ask must refuse, never proceed.
func TestShellAfterUntrustedWebWithNoApproverRefuses(t *testing.T) {
	reg, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.ShellAllow = []string{"echo*"}
	reg.MarkUntrustedWeb()
	res := reg.byName["shell"].Run(context.Background(), map[string]any{"command": "echo hi"})
	if !res.IsError || !strings.Contains(res.Content, "shell is not auto-approved after reading an untrusted web page in this request") {
		t.Fatalf("result %q", res.Content)
	}
}

func TestSubRegistriesNeverGetTheBrowser(t *testing.T) {
	reg, _, _, _ := browserFixture(t, "https://acme.test/login", nil, nil)
	if reg.Browser() == nil {
		t.Fatal("Registry.Browser did not find the tool")
	}
	for name, sub := range map[string]*Registry{
		"scoped (sub-agent)": reg.Scoped([]string{"internal"}, nil, "big"),
		"plan mode":          reg.Subset("read_file", "list_dir", "search"),
	} {
		for _, n := range sub.Names() {
			if n == "browser" {
				t.Fatalf("the %s registry has the browser", name)
			}
		}
	}
}

func TestShellAfterUntrustedWebAlwaysAsks(t *testing.T) {
	var log askLog
	reg, err := NewRegistry(t.TempDir(), log.approver(false))
	if err != nil {
		t.Fatal(err)
	}
	reg.ShellAllow = []string{"echo*"}
	sh := reg.byName["shell"]
	reg.MarkUntrustedWeb()
	res := sh.Run(context.Background(), map[string]any{"command": "echo hi"})
	if log.count() != 1 || log.actions[0] != "shell_after_web" {
		t.Fatalf("asks %v", log.actions)
	}
	if !res.IsError || !strings.Contains(res.Content, "shell is not auto-approved after reading an untrusted web page in this request") {
		t.Fatalf("result %q", res.Content)
	}
	reg.ClearUntrustedWeb()
	if res := sh.Run(context.Background(), map[string]any{"command": "echo hi"}); res.IsError || log.count() != 1 {
		t.Fatalf("after Clear the allow list must apply again: %q, %d asks", res.Content, log.count())
	}
}

func TestShellAfterUntrustedWebRunsWhenApproved(t *testing.T) {
	var log askLog
	reg, _ := NewRegistry(t.TempDir(), log.approver(true))
	reg.MarkUntrustedWeb()
	res := reg.byName["shell"].Run(context.Background(), map[string]any{"command": "echo approved"})
	if res.IsError || !strings.Contains(res.Content, "approved") {
		t.Fatalf("result %q", res.Content)
	}
}

func TestShellAfterUntrustedWebStillHonoursDeny(t *testing.T) {
	var log askLog
	reg, _ := NewRegistry(t.TempDir(), log.approver(true))
	reg.ShellDeny = []string{"rm*"}
	reg.MarkUntrustedWeb()
	res := reg.byName["shell"].Run(context.Background(), map[string]any{"command": "rm -rf x"})
	if !res.IsError || !strings.Contains(res.Content, "deny list") || log.count() != 0 {
		t.Fatalf("result %q, %d asks", res.Content, log.count())
	}
}
