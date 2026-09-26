package browser

import (
	"context"
	"errors"
	"os"
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
