package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func testSession(t *testing.T) (*Session, *browsertest.Browser, *browsertest.PageScript) {
	t.Helper()
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree)
	o := testOptions()
	o.Address = fb.Addr()
	s := NewSession(o)
	t.Cleanup(s.Close)
	return s, fb, ps
}

func TestSessionAttachesLazily(t *testing.T) {
	s, fb, _ := testSession(t)
	if len(fb.Calls("")) != 0 {
		t.Fatal("the session spoke to the browser before anything asked it to")
	}
	if st := s.Status(); st.Connected {
		t.Fatal("connected before first use")
	}
	_, notes, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "attached to FakeChrome/1.0 at "+fb.Addr() {
		t.Fatalf("notes %q", notes)
	}
	if c := fb.Calls("Browser.setDownloadBehavior"); len(c) != 1 || !strings.Contains(string(c[0].Params), `"deny"`) {
		t.Fatalf("downloads were not refused: %+v", c)
	}
	if len(fb.Calls("Target.setDiscoverTargets")) != 1 {
		t.Fatal("target discovery not enabled")
	}
	st := s.Status()
	if !st.Connected || st.Launched || st.Product != "FakeChrome/1.0" || st.Address != fb.Addr() {
		t.Fatalf("status %+v", st)
	}
}

func TestSessionRefusesARemoteAddress(t *testing.T) {
	s := NewSession(Options{Address: "10.0.0.5:9222", Launch: true})
	_, _, err := s.Page(context.Background())
	if err == nil || err.Error() != "browser.address 10.0.0.5:9222 is not on this machine; set browser.allow_remote to use it" {
		t.Fatalf("got %v", err)
	}
}

func TestSessionNoBrowserMessages(t *testing.T) {
	s := NewSession(Options{Address: "127.0.0.1:1", Launch: false})
	if _, _, err := s.Page(context.Background()); err == nil || !strings.Contains(err.Error(), "browser.launch is off") {
		t.Fatalf("launch off: %v", err)
	}
	oldLook, oldOS := lookPath, goos
	defer func() { lookPath, goos = oldLook, oldOS }()
	goos = "linux"
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	s = NewSession(Options{Address: "127.0.0.1:1", Launch: true})
	_, _, err := s.Page(context.Background())
	want := "no browser at 127.0.0.1:1 and none installed to launch — start one with --remote-debugging-port=1, or set browser.executable"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
}

func TestSessionLaunchesAndClosesWhatItLaunched(t *testing.T) {
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "about:blank", "", browsertest.EmptyTree)
	_, port, _ := strings.Cut(fb.Addr(), ":")
	t.Setenv("BE_CODE_FAKE_BROWSER", "serve")
	t.Setenv("BE_CODE_FAKE_PORT", port)
	t.Setenv("BE_CODE_FAKE_PATH", "/devtools/browser/fake")
	o := testOptions()
	o.Address, o.Launch, o.Executable, o.Profile, o.ForceHeadless = "127.0.0.1:1", true, os.Args[0], t.TempDir(), true
	s := NewSession(o)
	_, notes, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "launched ") || !strings.Contains(notes[0], "headless") {
		t.Fatalf("notes %q", notes)
	}
	if st := s.Status(); !st.Launched || st.Exe != os.Args[0] {
		t.Fatalf("status %+v", st)
	}
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	s.Close()
	if len(fb.Calls("Browser.close")) != 1 {
		t.Fatal("a launched browser was not asked to close")
	}
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the launched browser is still running after Close")
	}
}

func TestSessionCloseLeavesAnAttachedBrowserRunning(t *testing.T) {
	s, fb, _ := testSession(t)
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if len(fb.Calls("Browser.close")) != 0 {
		t.Fatal("Close shut down a browser BE-Code only attached to")
	}
	if s.Status().Connected {
		t.Fatal("still connected after Close")
	}
}

func TestSessionReconnectsAfterTheBrowserDrops(t *testing.T) {
	s, fb, _ := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	fb.Drop()
	waitFor(t, func() bool {
		select {
		case <-conn.Done():
			return true
		default:
			return false
		}
	})
	_, notes, err := s.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "the browser was closed; reconnected" {
		t.Fatalf("notes %q", notes)
	}
}

func TestSessionReplacesADestroyedTab(t *testing.T) {
	s, fb, ps := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	ps.RemoveTarget("T1")
	ps.AddTarget("T2", "https://acme.test/two", "Two", browsertest.FormTree, "")
	fb.Emit("", "Target.targetDestroyed", map[string]any{"targetId": "T1"})
	waitFor(t, func() bool { return s.isDestroyed("T1") })
	p, notes, err := s.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetID() != "T2" || len(notes) != 1 || notes[0] != "the tab being driven was closed; now driving: Two" {
		t.Fatalf("page %s, notes %q", p.TargetID(), notes)
	}
}

func TestSessionFollowsATabTheActionOpened(t *testing.T) {
	s, fb, ps := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	ps.AddTarget("T2", "https://acme.test/popup", "Popup", browsertest.FormTree, "T1")
	fb.Emit("", "Target.targetCreated", map[string]any{"targetInfo": map[string]any{"targetId": "T2", "type": "page", "openerId": "T1"}})
	waitFor(t, func() bool {
		s.evMu.Lock()
		defer s.evMu.Unlock()
		return len(s.created) > 0
	})
	note, err := s.AfterAction(ctx, since)
	if err != nil || note != "switched to the new tab: Popup" {
		t.Fatalf("note %q, %v", note, err)
	}
	p, _, _ := s.Page(ctx)
	if p.TargetID() != "T2" {
		t.Fatalf("still driving %s", p.TargetID())
	}
	if len(fb.Calls("Target.activateTarget")) != 1 {
		t.Fatal("the new tab was not brought to the front")
	}
}

func TestSessionTabsAndSwitch(t *testing.T) {
	s, _, ps := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	ps.AddTarget("T2", "https://acme.test/two", "Two", browsertest.FormTree, "")
	tabs, err := s.Tabs(ctx)
	if err != nil || len(tabs) != 2 || !tabs[0].Current || tabs[1].Current || tabs[1].Title != "Two" || tabs[1].Index != 2 {
		t.Fatalf("tabs %+v, %v", tabs, err)
	}
	if err := s.SwitchTab(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Page(ctx); p.TargetID() != "T2" {
		t.Fatal("did not switch")
	}
	if err := s.SwitchTab(ctx, 5); err == nil || err.Error() != "there is no tab 5; there are 2" {
		t.Fatalf("got %v", err)
	}
}

func TestSessionStatusNeverWaitsOnAnOperation(t *testing.T) {
	s, _, _ := testSession(t)
	s.mu.Lock() // an operation in progress (a slow launch, say)
	defer s.mu.Unlock()
	done := make(chan Status, 1)
	go func() { done <- s.Status() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Status waited on the session lock")
	}
}

// --- Fix round 1 ---

// TestSessionCloseWaitsForAVoluntaryExit is item 1: Close must give a
// launched browser a chance to shut down cleanly after Browser.close
// replies, rather than SIGKILLing it at once. The fake browser process
// (BE_CODE_FAKE_CLOSE_DELAY) exits *on its own* — os.Exit(0) — 150ms after
// it comes up, writing a marker file just before it does. A process killed
// by SIGKILL never reaches that write, so the marker's presence (and Close
// taking at least that long) proves Close waited for the exit rather than
// killing on the spot.
func TestSessionCloseWaitsForAVoluntaryExit(t *testing.T) {
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "about:blank", "", browsertest.EmptyTree)
	_, port, _ := strings.Cut(fb.Addr(), ":")
	marker := filepath.Join(t.TempDir(), "closed")
	t.Setenv("BE_CODE_FAKE_BROWSER", "serve")
	t.Setenv("BE_CODE_FAKE_PORT", port)
	t.Setenv("BE_CODE_FAKE_PATH", "/devtools/browser/fake")
	t.Setenv("BE_CODE_FAKE_CLOSE_DELAY", "150ms")
	t.Setenv("BE_CODE_FAKE_CLOSE_MARKER", marker)
	// closeWaitTimeout is left at its package default (5s): under a loaded
	// -race run, the exec scheduling delay before the child process even
	// reaches its own 150ms sleep can itself take a while, and a tight
	// override here would trade one flake for another.
	o := testOptions()
	o.Address, o.Launch, o.Executable, o.Profile, o.ForceHeadless = "127.0.0.1:1", true, os.Args[0], t.TempDir(), true
	s := NewSession(o)
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Close()
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Close returned in %s, before the fake browser could exit on its own — it must have killed the process instead of waiting for it", elapsed)
	}
	// No upper bound here beyond closeWaitTimeout itself: under load (a
	// full -race run spawns many of these fake-browser subprocesses) the
	// exec scheduling delay before the child even reaches its own 150ms
	// sleep can stretch well past it — the marker file below is what
	// actually proves the exit was voluntary, not a tight elapsed bound.
	if elapsed >= closeWaitTimeout {
		t.Fatalf("Close took %s — it should have noticed the voluntary exit before closeWaitTimeout (%s) elapsed", elapsed, closeWaitTimeout)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the launched browser was killed before it could exit on its own (no marker written)")
	}
}

// TestSessionCloseIsFinal is item 2: once Close has run, the session must
// never reconnect or launch again.
func TestSessionCloseIsFinal(t *testing.T) {
	s, fb, _ := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := len(fb.Calls(""))
	_, _, err := s.Page(ctx)
	if err == nil || err.Error() != "the browser session has ended" {
		t.Fatalf("got %v", err)
	}
	if len(fb.Calls("")) != before {
		t.Fatal("Page talked to the browser after Close")
	}
	if _, err := s.AfterAction(ctx, time.Now()); err == nil || err.Error() != "the browser session has ended" {
		t.Fatalf("AfterAction: got %v", err)
	}
	if _, err := s.Tabs(ctx); err == nil || err.Error() != "the browser session has ended" {
		t.Fatalf("Tabs: got %v", err)
	}
	if err := s.SwitchTab(ctx, 1); err == nil || err.Error() != "the browser session has ended" {
		t.Fatalf("SwitchTab: got %v", err)
	}
}

// TestSessionCloseAsyncStillReconnects is item 2's other half: CloseAsync
// is a restartable disconnect (for /browser close), not the final word.
func TestSessionCloseAsyncStillReconnects(t *testing.T) {
	s, _, _ := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	s.CloseAsync()
	waitFor(t, func() bool { return !s.Status().Connected })
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Connected {
		t.Fatal("did not reconnect after CloseAsync")
	}
}

// TestSessionAttachesToTheMostRecentTab is item 3: the initial pick goes
// through GET /json/list, which real Chromium orders most-recently-active
// first, rather than trusting Target.getTargets' undocumented order.
func TestSessionAttachesToTheMostRecentTab(t *testing.T) {
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree)
	ps.AddTarget("T2", "https://acme.test/two", "Two", browsertest.FormTree, "")
	// T1 is listed first — the opposite of Target.getTargets' own order
	// (T1 then T2, appended as created), and of what the pre-fix "last page
	// of getTargets" rule would pick (T2). Only reading /json/list gets T1.
	fb.SetList([]browsertest.ListEntry{
		{ID: "T1", Type: "page", Title: "Sign in — Acme", URL: "https://acme.test/login"},
		{ID: "T2", Type: "page", Title: "Two", URL: "https://acme.test/two"},
	})
	o := testOptions()
	o.Address = fb.Addr()
	s := NewSession(o)
	t.Cleanup(s.Close)
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetID() != "T1" {
		t.Fatalf("drove %s, want T1 (the tab /json/list lists first)", p.TargetID())
	}
}

// TestSessionFallsBackWhenJSONListFails is item 3's fallback: a 404 (no
// /json/list scripted) falls back to the last page target of
// Target.getTargets, exactly as before this fix.
func TestSessionFallsBackWhenJSONListFails(t *testing.T) {
	s, _, ps := testSession(t)
	ps.AddTarget("T2", "https://acme.test/two", "Two", browsertest.FormTree, "")
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetID() != "T2" {
		t.Fatalf("drove %s, want T2 (getTargets' last page, /json/list having 404ed)", p.TargetID())
	}
}

// TestSessionFallsBackWhenTheJSONListPickIsGone is fix round 2's item: the
// tab GET /json/list names can have just closed — its Target.targetDestroyed
// not yet processed, so isDestroyed does not yet know it — and attaching to
// it fails. That must fall back to the getTargets pick (the same live call
// the attach was always paired with before /json/list existed) rather than
// surfacing the raw attach error out of Page.
func TestSessionFallsBackWhenTheJSONListPickIsGone(t *testing.T) {
	s, fb, _ := testSession(t)
	fb.SetList([]browsertest.ListEntry{{ID: "T-ghost", Type: "page", Title: "Ghost", URL: "https://acme.test/ghost"}})
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetID() != "T1" {
		t.Fatalf("drove %s, want T1 (getTargets, after the /json/list pick turned out to be gone)", p.TargetID())
	}
}

// TestSessionNotesWhenDownloadsCannotBeRefused is item 4: a failed
// Browser.setDownloadBehavior must not be silently ignored.
func TestSessionNotesWhenDownloadsCannotBeRefused(t *testing.T) {
	s, fb, _ := testSession(t)
	fb.Handle("Browser.setDownloadBehavior", func(string, json.RawMessage) (any, error) {
		return nil, errors.New("not supported")
	})
	_, notes, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.HasSuffix(notes[0], "; downloads could not be refused by this browser") {
		t.Fatalf("notes %q", notes)
	}
}

// Final review M8 (spec §5): a panicking reader marks the connection dead
// with a notice saying so, not merely that the browser was closed; the next
// call reconnects.
func TestSessionReconnectsAfterTheReaderPanics(t *testing.T) {
	s, fb, _ := testSession(t)
	ctx := context.Background()
	if _, _, err := s.Page(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	conn.Subscribe(func(Event) { panic("boom") })
	fb.Emit("", "Target.targetInfoChanged", map[string]any{})
	waitFor(t, func() bool {
		select {
		case <-conn.Done():
			return true
		default:
			return false
		}
	})
	_, notes, err := s.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "the browser connection was marked dead (its reader panicked: boom); reconnected" {
		t.Fatalf("notes %q", notes)
	}
}
