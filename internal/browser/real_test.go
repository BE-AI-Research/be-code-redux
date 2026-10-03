package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestRealBrowser drives an installed Chromium headless against a local
// page: the renderer against real accessibility output, a real click, real
// typing, and a password that must never appear. It also covers two cases
// found in review of earlier tasks — a one-time-code field reachable only
// through a shadow root, and a button whose click opens a confirm() dialog
// — against a real browser rather than the scripted fake. Skipped with
// -short and where no browser is installed (the VM, CI).
func TestRealBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real browser")
	}
	exe := FindExecutable()
	if exe == "" {
		t.Skip("no Chrome, Edge, Brave or Chromium installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Counter</title>
<main><h1>Counter</h1>
<p id="n">count: 0</p>
<button onclick="window.k=(window.k||0)+1;document.getElementById('n').textContent='count: '+window.k">Increment</button>
<label>Name <input id="name"></label>
<label>Password <input type="password" value="hunter2"></label>
<div id="otp-host"></div>
<button onclick="window.confirmed=confirm('Really delete?')">Delete</button>
<script>
  const otpRoot = document.getElementById('otp-host').attachShadow({mode: 'open'});
  otpRoot.innerHTML = '<label>Code <input id="otp" autocomplete="one-time-code"></label>';
  otpRoot.getElementById('otp').value = '654321';
</script>
</main>`)
	}))
	defer srv.Close()
	s := NewSession(Options{Address: "127.0.0.1:1", Launch: true, Executable: exe, Profile: t.TempDir(),
		ForceHeadless: true, SnapshotChars: 12000, SettleTimeout: 10 * time.Second})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, _, err := s.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, ctx, p)
	if err := p.Click(ctx, refFor(t, snap, `button "Increment"`)); err != nil {
		t.Fatal(err)
	}
	snap = mustSnapshot(t, ctx, p)
	if !strings.Contains(snap, "count: 1") {
		t.Fatalf("the click did not land:\n%s", snap)
	}
	if err := p.Type(ctx, refFor(t, snap, `textbox "Name"`), "Ann", false); err != nil {
		t.Fatal(err)
	}
	snap = mustSnapshot(t, ctx, p)
	if !regexp.MustCompile(`textbox "Name" \[e\d+\] = "Ann"`).MatchString(snap) {
		t.Fatalf("the typing did not land:\n%s", snap)
	}
	text, err := p.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{snap, text} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("the password's value was shown:\n%s", out)
		}
	}
	if !strings.Contains(snap, "(password)") {
		t.Fatalf("the password field was not marked:\n%s", snap)
	}

	// A one-time-code field that exists only inside an open shadow root
	// (attachShadow), its value set by script before the first snapshot.
	// It must show as sensitive, its value must never appear anywhere in
	// the snapshot (or the page's read text), and typing into it must be
	// refused.
	otpRef := refFor(t, snap, `textbox "Code"`)
	if !strings.Contains(snap, "(one-time code)") {
		t.Fatalf("the shadow-root one-time-code field was not marked sensitive:\n%s", snap)
	}
	for _, out := range []string{snap, text} {
		if strings.Contains(out, "654321") {
			t.Fatalf("the shadow-root one-time-code field's value was shown:\n%s", out)
		}
	}
	var sfe *SensitiveFieldError
	if err := p.Type(ctx, otpRef, "000000", false); err == nil || !errors.As(err, &sfe) {
		t.Fatalf("typing into a shadow-root one-time-code field must be refused with *SensitiveFieldError, got %v", err)
	}

	// A button whose click opens confirm(): the snapshot must report the
	// open dialog (the package's own representation — a "dialog" line plus
	// accept/dismiss refs), accepting it must clear the dialog, and none of
	// this may hang (the whole test runs inside a bounded ctx).
	if err := p.Click(ctx, refFor(t, snap, `button "Delete"`)); err != nil {
		t.Fatal(err)
	}
	snap = mustSnapshot(t, ctx, p)
	if !strings.Contains(snap, `dialog confirm "Really delete?"`) {
		t.Fatalf("the confirm dialog was not reported in the snapshot:\n%s", snap)
	}
	acceptRef := refFor(t, snap, "accept [")
	if err := p.Click(ctx, acceptRef); err != nil {
		t.Fatal(err)
	}
	snap = mustSnapshot(t, ctx, p)
	if strings.Contains(snap, "dialog confirm") {
		t.Fatalf("the dialog was still showing after it was accepted:\n%s", snap)
	}
}

func mustSnapshot(t *testing.T, ctx context.Context, p *Page) string {
	t.Helper()
	snap, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// refFor finds the ref on the snapshot line that starts with prefix.
func refFor(t *testing.T, snap, prefix string) string {
	t.Helper()
	re := regexp.MustCompile(`\[(e\d+)\]`)
	for _, l := range strings.Split(snap, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			if m := re.FindStringSubmatch(l); m != nil {
				return m[1]
			}
		}
	}
	t.Fatalf("no %s in:\n%s", prefix, snap)
	return ""
}

// TestRealBrowserMyChromeMode attaches in my-Chrome mode to a real
// Chromium. The chrome://inspect toggle (and its "Allow remote debugging?"
// prompt) cannot be switched on headlessly, so the browser is launched here
// with --remote-debugging-port=0 on a throwaway profile instead: that writes
// the same DevToolsActivePort file into the profile, which is all this mode
// reads. What it proves against a real browser: the WebSocket-only attach
// from the port file, a new tab of the agent's own, the existing tab kept
// out of the model's list, and only the agent's tab closed on Close. The
// toggle and the prompt themselves are on the live checklist. Skipped with
// -short and where no browser is installed.
func TestRealBrowserMyChromeMode(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real browser")
	}
	exe := FindExecutable()
	if exe == "" {
		t.Skip("no Chrome, Edge, Brave or Chromium installed")
	}
	profile := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	proc, err := Launch(ctx, exe, profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Kill()
	s := NewSession(Options{MyChrome: true, ChromeDir: profile, ChromeLabel: "test", SnapshotChars: 12000, SettleTimeout: 10 * time.Second})
	defer s.Close()
	p, notes, err := s.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "attached to your Chrome (test); working in a new tab") {
		t.Fatalf("notes %q", notes)
	}
	tabs, err := s.Tabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 1 || !tabs[0].Current {
		t.Fatalf("the model's tabs %+v", tabs)
	}
	waitFor(t, func() bool { return len(s.PersonTabs()) >= 2 })
	ours := p.targetID
	s.Close()
	// The launched browser's own first tab is still open; the agent's is not.
	conn, err := Dial(ctx, proc.WSURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Target.closeTarget answers before the tab is gone: wait for it.
	ourOpen, pages := true, 0
	for deadline := time.Now().Add(5 * time.Second); ourOpen && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var ts struct {
			TargetInfos []targetInfo `json:"targetInfos"`
		}
		if err := conn.Call(ctx, "", "Target.getTargets", nil, &ts); err != nil {
			t.Fatal(err)
		}
		ourOpen, pages = false, 0
		for _, ti := range ts.TargetInfos {
			if ti.Type == "page" {
				pages++
				ourOpen = ourOpen || ti.TargetID == ours
			}
		}
	}
	if ourOpen {
		t.Fatal("the agent's tab was left open")
	}
	if pages == 0 {
		t.Fatal("closed a tab that was not the agent's")
	}
}
