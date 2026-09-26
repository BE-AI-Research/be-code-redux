package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func testOptions() Options {
	return Options{SnapshotChars: 12000, SettleTimeout: 2 * time.Second, QuietWindow: 20 * time.Millisecond}
}

func testPageOpts(t *testing.T, url, title, tree string, opts Options) (*Page, *browsertest.PageScript, *browsertest.Browser) {
	t.Helper()
	fb := browsertest.New(t)
	ps := browsertest.NewPage(fb, url, title, tree)
	conn := dialFake(t, fb)
	p, err := attachPage(context.Background(), conn, "T1", opts)
	if err != nil {
		t.Fatal(err)
	}
	return p, ps, fb
}

// formPage is the sign-in fixture with its password and card fields found
// by the sensitive-field query.
func formPage(t *testing.T) (*Page, *browsertest.PageScript, *browsertest.Browser) {
	p, ps, fb := testPageOpts(t, "https://acme.test/login", "Sign in — Acme", browsertest.FormTree, testOptions())
	ps.Lock()
	ps.Sensitive = map[int][]string{60: {"type", "password"}, 130: {"autocomplete", "cc-number"}}
	ps.Unlock()
	return p, ps, fb
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition never held")
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// inOrder reports whether want appears in list as a subsequence.
func inOrder(list, want []string) bool {
	i := 0
	for _, s := range list {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestPageSnapshotHidesSensitiveFields(t *testing.T) {
	p, _, _ := formPage(t)
	snap, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`textbox "Email" [e1] = "ann@example.com"`, `textbox "Password" (password) [e2]`, `textbox "Card number" (card) [e6]`} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, snap)
		}
	}
	if strings.Contains(snap, "hunter2") || strings.Contains(snap, "4111") {
		t.Fatalf("snapshot shows a secret:\n%s", snap)
	}
}

func TestPageSnapshotFailsClosedWhenTheSensitiveQueryFails(t *testing.T) {
	p, ps, _ := formPage(t)
	ps.Lock()
	ps.FailSensitiveQuery = true
	ps.Unlock()
	snap, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap, "ann@example.com") || strings.Contains(snap, "hunter2") {
		t.Fatalf("a failed query still showed field values:\n%s", snap)
	}
}

func TestPageIsSensitive(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if yes, err := p.IsSensitive(ctx, "e2"); err != nil || !yes {
		t.Fatalf("password field: %v, %v", yes, err)
	}
	if yes, err := p.IsSensitive(ctx, "e1"); err != nil || yes {
		t.Fatalf("email field: %v, %v", yes, err)
	}
	ps.Lock()
	ps.FailSensitiveQuery = true
	ps.Unlock()
	if yes, _ := p.IsSensitive(ctx, "e1"); !yes {
		t.Fatal("a failed check must answer yes (fail closed)")
	}
	var unknown *UnknownRefError
	if _, err := p.IsSensitive(ctx, "e99"); !errors.As(err, &unknown) {
		t.Fatalf("unknown ref gave %v", err)
	}
}

func TestPageClickDispatchesRealMouseEventsAtTheCentre(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if err := p.Click(ctx, "e4"); err != nil {
		t.Fatal(err)
	}
	want := []string{"mouse mouseMoved 50,20", "mouse mousePressed 50,20", "mouse mouseReleased 50,20"}
	if !inOrder(ps.Inputs(), want) {
		t.Fatalf("inputs %v", ps.Inputs())
	}
}

func TestPageStaleRef(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	ps.Lock()
	ps.Disconnected[80] = true
	ps.Unlock()
	err := p.Click(ctx, "e4")
	var stale *StaleRefError
	if !errors.As(err, &stale) || err.Error() != "e4 is no longer on the page" {
		t.Fatalf("got %v", err)
	}
	for _, in := range ps.Inputs() {
		if strings.HasPrefix(in, "mouse") {
			t.Fatal("a stale ref was clicked anyway")
		}
	}
}

func TestPageUnknownRef(t *testing.T) {
	p, _, _ := formPage(t)
	err := p.Click(context.Background(), "e99")
	var unknown *UnknownRefError
	if !errors.As(err, &unknown) || !strings.Contains(err.Error(), "not an element on this page") {
		t.Fatalf("got %v", err)
	}
}

func TestPageTypeReplacesAndSubmits(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if err := p.Type(ctx, "e1", "bob@example.com", true); err != nil {
		t.Fatal(err)
	}
	want := []string{"focus 40", "select-all", "text bob@example.com", "key keyDown Enter", "key keyUp Enter"}
	if !inOrder(ps.Inputs(), want) {
		t.Fatalf("inputs %v", ps.Inputs())
	}
}

func TestPageTypeEmptyClears(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if err := p.Type(ctx, "e1", "", false); err != nil {
		t.Fatal(err)
	}
	if !has(ps.Inputs(), "key keyDown Delete") {
		t.Fatalf("clearing did not press Delete: %v", ps.Inputs())
	}
}

func TestPageSelect(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if err := p.Select(ctx, "e1", "Canada"); err != nil {
		t.Fatal(err)
	}
	if !has(ps.Inputs(), `select "Canada"`) {
		t.Fatalf("inputs %v", ps.Inputs())
	}
	ps.Lock()
	ps.SelectResult = `no option "Mars"; the options are: Canada`
	ps.Unlock()
	if err := p.Select(ctx, "e1", "Mars"); err == nil || err.Error() != `no option "Mars"; the options are: Canada` {
		t.Fatalf("got %v", err)
	}
}

func TestPagePress(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	if err := p.Press(ctx, "esc"); err != nil {
		t.Fatal(err)
	}
	if !has(ps.Inputs(), "key keyDown Escape") {
		t.Fatalf("inputs %v", ps.Inputs())
	}
	if err := p.Press(ctx, "F13"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("got %v", err)
	}
}

func TestPageScroll(t *testing.T) {
	p, ps, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if err := p.Scroll(ctx, "down", ""); err != nil {
		t.Fatal(err)
	}
	if !has(ps.Inputs(), "wheel 480") {
		t.Fatalf("inputs %v", ps.Inputs())
	}
	if err := p.Scroll(ctx, "", "e1"); err != nil || !has(ps.Inputs(), "scroll-into-view 40") {
		t.Fatalf("scroll to a ref: %v, %v", err, ps.Inputs())
	}
	if err := p.Scroll(ctx, "sideways", ""); err == nil {
		t.Fatal("an unknown direction was accepted")
	}
}

func TestPageNavigateWaitsForTheLoadEvent(t *testing.T) {
	p, ps, fb := formPage(t)
	ps.Lock()
	ps.LoadDelay = 150 * time.Millisecond
	ps.Unlock()
	start := time.Now()
	if err := p.Navigate(context.Background(), "acme.test/next"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("returned after %s, before the load event", time.Since(start))
	}
	calls := fb.Calls("Page.navigate")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Params), `"https://acme.test/next"`) {
		t.Fatalf("navigate calls %+v", calls)
	}
	if snap, _ := p.Snapshot(context.Background()); strings.Contains(snap, loadingNote) {
		t.Fatal("a loaded page is marked loading")
	}
}

func TestPageNavigateTimesOutAsLoading(t *testing.T) {
	opts := testOptions()
	opts.SettleTimeout = 200 * time.Millisecond
	p, ps, _ := testPageOpts(t, "https://acme.test/login", "Sign in", browsertest.FormTree, opts)
	ps.Lock()
	ps.NoLoadEvent = true
	ps.Unlock()
	start := time.Now()
	if err := p.Navigate(context.Background(), "https://acme.test/slow"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("settle ran %s past a 200ms timeout", d)
	}
	snap, _ := p.Snapshot(context.Background())
	if !strings.Contains(snap, loadingNote) {
		t.Fatalf("a page that never loaded is not marked loading:\n%s", snap)
	}
}

func TestPageNavigateReportsNetworkErrors(t *testing.T) {
	p, _, _ := formPage(t)
	err := p.Navigate(context.Background(), "https://unreachable.test/")
	if err == nil || err.Error() != "could not open https://unreachable.test/: net::ERR_NAME_NOT_RESOLVED" {
		t.Fatalf("got %v", err)
	}
}

func TestPageNavigationResetsRefs(t *testing.T) {
	p, _, _ := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	if _, ok := p.refs.Lookup("e1"); !ok {
		t.Fatal("no ref after the first snapshot")
	}
	if err := p.Navigate(ctx, "https://acme.test/other"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, ok := p.refs.Lookup("e1"); return !ok })
}

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"example.com":             "https://example.com",
		"example.com:8080/a":      "https://example.com:8080/a",
		"localhost:3000/x":        "http://localhost:3000/x",
		"127.0.0.1:8080":          "http://127.0.0.1:8080",
		"https://acme.test/login": "https://acme.test/login",
		"about:blank":             "about:blank",
	}
	for in, want := range ok {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "data:text/html,hi",
		"mailto:a@b.c", "chrome://settings", "ftp://example.com/x"} {
		if got, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) accepted it as %q", in, got)
		}
	}
}

func TestPageAlertIsAcceptedAndNoted(t *testing.T) {
	p, ps, fb := formPage(t)
	p.Snapshot(context.Background())
	fb.Emit("S-T1", "Page.javascriptDialogOpening", map[string]any{"type": "alert", "message": "Saved"})
	waitFor(t, func() bool { return has(ps.Inputs(), "dialog accept=true") })
	if notes := p.TakeNotes(); len(notes) != 1 || notes[0] != `alert: "Saved"` {
		t.Fatalf("notes %q", notes)
	}
	if len(p.TakeNotes()) != 0 {
		t.Fatal("TakeNotes did not clear them")
	}
}

func TestPageConfirmIsShownAndAnsweredByItsRef(t *testing.T) {
	p, ps, fb := formPage(t)
	ctx := context.Background()
	p.Snapshot(ctx)
	fb.Emit("S-T1", "Page.javascriptDialogOpening", map[string]any{"type": "confirm", "message": "Delete this repository?"})
	var snap string
	waitFor(t, func() bool {
		snap, _ = p.Snapshot(ctx)
		return strings.Contains(snap, `dialog confirm "Delete this repository?"`)
	})
	before := len(fb.Calls("Accessibility.getFullAXTree"))
	p.Snapshot(ctx)
	if after := len(fb.Calls("Accessibility.getFullAXTree")); after != before {
		t.Fatal("the page was read while a dialog blocked it")
	}
	accept, _ := p.refs.DialogRefs()
	if !strings.Contains(snap, "accept ["+accept+"]") {
		t.Fatalf("no accept ref:\n%s", snap)
	}
	if p.Describe(accept) != `accept dialog "Delete this repository?" [`+accept+`]` {
		t.Fatalf("describe %q", p.Describe(accept))
	}
	if err := p.Click(ctx, accept); err != nil {
		t.Fatal(err)
	}
	if !has(ps.Inputs(), "dialog accept=true") {
		t.Fatalf("inputs %v", ps.Inputs())
	}
	// Wait for the fake's Page.javascriptDialogClosed (sent a moment after
	// the reply, as the real browser orders it) to actually arrive: the
	// dialog block must clear from a live snapshot before the test ends,
	// or that event can land after this test's fake browser has already
	// torn down, crashing the whole run (browsertest.Browser.Emit fails
	// the test — correctly, for a real dropped event — but the failure
	// then reaches a *different*, already-completed test).
	waitFor(t, func() bool {
		s, _ := p.Snapshot(ctx)
		return !strings.Contains(s, "dialog confirm")
	})
}

func TestPageBack(t *testing.T) {
	p, _, fb := formPage(t)
	ctx := context.Background()
	if err := p.Back(ctx); err == nil || !strings.Contains(err.Error(), "no earlier page") {
		t.Fatalf("back at the start: %v", err)
	}
	p.Navigate(ctx, "https://acme.test/next")
	if err := p.Back(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := fb.Calls("Page.navigateToHistoryEntry"); len(calls) != 1 || !strings.Contains(string(calls[0].Params), `"entryId":1`) {
		t.Fatalf("history calls %+v", calls)
	}
}

func TestPageReadUsesAnIsolatedWorld(t *testing.T) {
	p, ps, fb := formPage(t)
	ps.Lock()
	ps.ReadText = "Title\n\n\n\n   Body text  \n"
	ps.Unlock()
	got, err := p.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "Title\n\nBody text" {
		t.Fatalf("read %q", got)
	}
	calls := fb.Calls("Runtime.evaluate")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Params), `"contextId":7`) {
		t.Fatalf("evaluate did not run in the isolated world: %+v", calls)
	}
}

func TestPageSnapshotOverBudgetFetchesTheViewport(t *testing.T) {
	opts := testOptions()
	opts.SnapshotChars = 40
	p, _, fb := testPageOpts(t, "https://acme.test/login", "Sign in", browsertest.FormTree, opts)
	if _, err := p.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fb.Calls("DOMSnapshot.captureSnapshot")) != 1 {
		t.Fatal("an over-budget snapshot did not ask where the viewport is")
	}
}
