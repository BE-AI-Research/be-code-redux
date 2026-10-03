package browser

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func TestParseDevToolsActivePort(t *testing.T) {
	for _, c := range []struct {
		name, in string
		port     int
		path     string
		ok       bool
	}{
		{"good", "9222\n/devtools/browser/abc-123\n", 9222, "/devtools/browser/abc-123", true},
		{"crlf", "54321\r\n/devtools/browser/x\r\n", 54321, "/devtools/browser/x", true},
		{"blank lines and spaces", "\n  1234 \n\n /devtools/browser/y \n", 1234, "/devtools/browser/y", true},
		{"missing path line", "9222\n", 0, "", false},
		{"empty", "", 0, "", false},
		{"port zero", "0\n/devtools/browser/x\n", 0, "", false},
		{"port too large", "65536\n/devtools/browser/x\n", 0, "", false},
		{"port max", "65535\n/devtools/browser/x\n", 65535, "/devtools/browser/x", true},
		{"garbage", "hello\nworld\n", 0, "", false},
		{"negative", "-5\n/devtools/browser/x\n", 0, "", false},
		{"path not a path", "9222\nws://evil/\n", 0, "", false},
	} {
		port, path, err := ParseDevToolsActivePort([]byte(c.in))
		if c.ok != (err == nil) || port != c.port || path != c.path {
			t.Errorf("%s: got %d %q %v", c.name, port, path, err)
		}
	}
}

func TestChromeUserDataDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := "/home/p"
	for _, c := range []struct {
		goos, channel string
		env           map[string]string
		want          string
	}{
		{"linux", "stable", nil, filepath.Join(home, ".config", "google-chrome")},
		{"linux", "beta", nil, filepath.Join(home, ".config", "google-chrome-beta")},
		{"linux", "dev", nil, filepath.Join(home, ".config", "google-chrome-unstable")},
		{"linux", "canary", nil, filepath.Join(home, ".config", "google-chrome-canary")},
		{"linux", "stable", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, filepath.Join("/xdg", "google-chrome")},
		{"linux", "stable", map[string]string{"XDG_CONFIG_HOME": "/xdg", "CHROME_CONFIG_HOME": "/cch"}, filepath.Join("/cch", "google-chrome")},
		{"darwin", "stable", nil, filepath.Join(home, "Library", "Application Support", "Google", "Chrome")},
		{"darwin", "beta", nil, filepath.Join(home, "Library", "Application Support", "Google", "Chrome Beta")},
		{"darwin", "dev", nil, filepath.Join(home, "Library", "Application Support", "Google", "Chrome Dev")},
		{"darwin", "canary", nil, filepath.Join(home, "Library", "Application Support", "Google", "Chrome Canary")},
		{"windows", "stable", map[string]string{"LOCALAPPDATA": `C:\L`}, filepath.Join(`C:\L`, "Google", "Chrome", "User Data")},
		{"windows", "beta", map[string]string{"LOCALAPPDATA": `C:\L`}, filepath.Join(`C:\L`, "Google", "Chrome Beta", "User Data")},
		{"windows", "dev", map[string]string{"LOCALAPPDATA": `C:\L`}, filepath.Join(`C:\L`, "Google", "Chrome Dev", "User Data")},
		{"windows", "canary", map[string]string{"LOCALAPPDATA": `C:\L`}, filepath.Join(`C:\L`, "Google", "Chrome SxS", "User Data")},
		{"windows", "stable", nil, filepath.Join(home, "AppData", "Local", "Google", "Chrome", "User Data")},
	} {
		if got := chromeUserDataDir(c.goos, c.channel, home, env(c.env)); got != c.want {
			t.Errorf("%s/%s %v: got %q, want %q", c.goos, c.channel, c.env, got, c.want)
		}
	}
}

func TestResolveChromeDir(t *testing.T) {
	oldOS, oldEnv, oldHome := goos, getenv, userHomeDir
	defer func() { goos, getenv, userHomeDir = oldOS, oldEnv, oldHome }()
	goos = "linux"
	getenv = func(string) string { return "" }
	userHomeDir = func() (string, error) { return "/home/p", nil }

	dir, label, warn := ResolveChromeDir("", "")
	if dir != "/home/p/.config/google-chrome" || label != "stable" || warn != "" {
		t.Fatalf("default: %q %q %q", dir, label, warn)
	}
	dir, label, warn = ResolveChromeDir(" Beta ", "")
	if dir != "/home/p/.config/google-chrome-beta" || label != "beta" || warn != "" {
		t.Fatalf("beta: %q %q %q", dir, label, warn)
	}
	dir, label, warn = ResolveChromeDir("nightly", "")
	if dir != "/home/p/.config/google-chrome" || label != "stable" ||
		warn != `browser.chrome_channel "nightly" is not one of stable|beta|dev|canary; using stable` {
		t.Fatalf("unknown: %q %q %q", dir, label, warn)
	}
	dir, label, warn = ResolveChromeDir("nightly", "/srv/chromium")
	if dir != "/srv/chromium" || label != "/srv/chromium" || warn != "" {
		t.Fatalf("explicit dir wins, no channel warning: %q %q %q", dir, label, warn)
	}
	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	if dir, _, _ = ResolveChromeDir("stable", ""); dir != "" {
		t.Fatalf("no home: %q", dir)
	}
}

// myChromeFixture is the person's own Chrome: WebSocket only, one tab of
// their own (T1, a bank they are signed in to), and its DevToolsActivePort
// in a temporary user-data dir.
func myChromeFixture(t *testing.T) (*Session, *browsertest.Browser, *browsertest.PageScript, string) {
	t.Helper()
	fb := browsertest.New(t)
	fb.SetWSOnly(true)
	ps := browsertest.NewPage(fb, "https://bank.test/accounts", "My accounts — Bank", browsertest.FormTree)
	dir := t.TempDir()
	fb.WritePortFile(dir)
	o := testOptions()
	o.MyChrome, o.ChromeDir, o.ChromeLabel = true, dir, "stable"
	// Launching is configured on and an executable named: my-Chrome mode
	// must never use either.
	o.Launch, o.Executable = true, filepath.Join(dir, "no-such-chrome")
	s := NewSession(o)
	t.Cleanup(s.Close)
	return s, fb, ps, dir
}

func TestMyChromeAttachesOverTheWebSocketAloneInANewTab(t *testing.T) {
	s, fb, _, _ := myChromeFixture(t)
	p, notes, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fb.HTTPHits() != 0 {
		t.Fatalf("asked HTTP %d times; my-Chrome mode is WebSocket only", fb.HTTPHits())
	}
	if len(notes) != 1 || notes[0] != "attached to your Chrome (stable); working in a new tab — your own tabs stay private" {
		t.Fatalf("notes %q", notes)
	}
	if p.targetID == "T1" {
		t.Fatal("drove the person's own tab")
	}
	if len(fb.Calls("Target.createTarget")) != 1 {
		t.Fatal("no tab of its own was opened")
	}
	for _, c := range fb.Calls("Target.attachToTarget") {
		if strings.Contains(string(c.Params), `"T1"`) {
			t.Fatal("attached to the person's own tab")
		}
	}
	if len(fb.Calls("Browser.setDownloadBehavior")) != 0 {
		t.Fatal("changed the download behaviour of the person's whole browser")
	}
	if len(fb.Calls("Page.setDownloadBehavior")) != 1 {
		t.Fatal("downloads were not refused in the agent's own tab")
	}
	st := s.Status()
	if !st.Connected || !st.MyChrome || st.Label != "stable" || st.Launched {
		t.Fatalf("status %+v", st)
	}
}

func TestMyChromeTabsListOnlyTheAgentsOwn(t *testing.T) {
	s, _, ps, _ := myChromeFixture(t)
	ps.AddTarget("T2", "https://mail.test/inbox", "Inbox (3) — private", browsertest.EmptyTree, "")
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	tabs, err := s.Tabs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 1 || tabs[0].URL != "about:blank" || !tabs[0].Current {
		t.Fatalf("model's tab list %+v", tabs)
	}
	for _, n := range []int{2, 3} {
		if err := s.SwitchTab(context.Background(), n); err == nil {
			t.Fatalf("switched to tab %d, which is the person's", n)
		}
	}
}

func TestMyChromeHandOverIsThePersonsOnly(t *testing.T) {
	s, fb, _, _ := myChromeFixture(t)
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := s.ListForPerson()
	if len(all) != 2 {
		t.Fatalf("the person's own list %+v", all)
	}
	n := 0
	for _, tab := range all {
		if tab.URL == "https://bank.test/accounts" {
			n = tab.Index
			if tab.Agent {
				t.Fatal("the person's tab is marked as the agent's")
			}
		}
	}
	if n == 0 {
		t.Fatalf("the person's tab is missing from their own list %+v", all)
	}
	got, err := s.HandOver(strconv.Itoa(n))
	if err != nil || got.Title != "My accounts — Bank" {
		t.Fatalf("hand over: %+v %v", got, err)
	}
	p, notes, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.targetID != "T1" || len(notes) != 1 || notes[0] != "now driving the tab you handed over: bank.test" {
		t.Fatalf("after hand-over: %s %q", p.targetID, notes)
	}
	tabs, _ := s.Tabs(context.Background())
	if len(tabs) != 2 {
		t.Fatalf("model's tab list after the hand-over %+v", tabs)
	}
	if _, err := s.HandOver("9"); err == nil {
		t.Fatal("handed over a tab that does not exist")
	}
	// Closing leaves the handed-over tab open: only what the agent opened
	// is closed.
	s.Close()
	closed := fb.Calls("Target.closeTarget")
	if len(closed) != 1 || strings.Contains(string(closed[0].Params), `"T1"`) {
		t.Fatalf("closed %+v", closed)
	}
}

func TestMyChromeCloseClosesOnlyItsOwnTabsAndNeverTheBrowser(t *testing.T) {
	s, fb, ps, _ := myChromeFixture(t)
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ours := p.targetID
	// A popup the agent's tab opened is the agent's too.
	since := time.Now()
	ps.AddTarget("T9", "https://shop.test/", "Shop", browsertest.EmptyTree, ours)
	fb.Emit("", "Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "T9", "type": "page", "url": "https://shop.test/", "openerId": ours}})
	// One the person's own tab opened is not.
	ps.AddTarget("T8", "https://bank.test/statement", "Statement", browsertest.EmptyTree, "T1")
	fb.Emit("", "Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "T8", "type": "page", "url": "https://bank.test/statement", "openerId": "T1"}})
	waitFor(t, func() bool { return len(s.PersonTabs()) == 4 })
	if note, err := s.AfterAction(context.Background(), since); err != nil || note != "switched to the new tab: shop.test" {
		t.Fatalf("after action: %q %v", note, err)
	}
	s.Close()
	var closed []string
	for _, c := range fb.Calls("Target.closeTarget") {
		closed = append(closed, string(c.Params))
	}
	if len(closed) != 2 || !strings.Contains(strings.Join(closed, " "), `"`+ours+`"`) || !strings.Contains(strings.Join(closed, " "), `"T9"`) {
		t.Fatalf("closed %q", closed)
	}
	if len(fb.Calls("Browser.close")) != 0 {
		t.Fatal("closed the person's browser")
	}
}

func TestMyChromeMissingPortFileNamesTheToggleAndNeverLaunches(t *testing.T) {
	s, fb, _, dir := myChromeFixture(t)
	os.Remove(filepath.Join(dir, "DevToolsActivePort"))
	_, _, err := s.Page(context.Background())
	if err == nil || !strings.Contains(err.Error(), "chrome://inspect/#remote-debugging") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "launch") || len(fb.Calls("")) != 0 {
		t.Fatalf("fell back to something else: %v", err)
	}
}

func TestMyChromeUnreadablePortFile(t *testing.T) {
	s, _, _, dir := myChromeFixture(t)
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("garbage"), 0o600)
	_, _, err := s.Page(context.Background())
	if err == nil || !strings.Contains(err.Error(), "chrome://inspect/#remote-debugging") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("got %v", err)
	}
}

func TestMyChromeStalePortFile(t *testing.T) {
	s, _, _, dir := myChromeFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens there now: Chrome has exited
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(strconv.Itoa(port)+"\n/devtools/browser/gone\n"), 0o600)
	_, _, err = s.Page(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "chrome://inspect/#remote-debugging") ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("got %v", err)
	}
}

type noticeLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *noticeLog) add(m string) {
	l.mu.Lock()
	l.msgs = append(l.msgs, m)
	l.mu.Unlock()
}

func (l *noticeLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

func withNoticeAfter(t *testing.T, d time.Duration) {
	old := consentNoticeAfter
	consentNoticeAfter = d
	t.Cleanup(func() { consentNoticeAfter = old })
}

func TestMyChromeNoticesTheConsentPromptOnce(t *testing.T) {
	withNoticeAfter(t, 20*time.Millisecond)
	for _, c := range []struct {
		name                 string
		handshake, firstCall time.Duration
	}{{"handshake held", 300 * time.Millisecond, 0}, {"first reply held", 0, 300 * time.Millisecond}} {
		t.Run(c.name, func(t *testing.T) {
			fb := browsertest.New(t)
			fb.SetWSOnly(true)
			browsertest.NewPage(fb, "https://bank.test/", "Bank", browsertest.EmptyTree)
			fb.SetConsentDelay(c.handshake, c.firstCall)
			dir := t.TempDir()
			fb.WritePortFile(dir)
			var log noticeLog
			o := testOptions()
			o.MyChrome, o.ChromeDir, o.ChromeLabel, o.Notify = true, dir, "stable", log.add
			s := NewSession(o)
			defer s.Close()
			if _, _, err := s.Page(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := log.all()
			if len(got) != 2 || got[0] != ChromeConsentNotice || got[1] != "" {
				t.Fatalf("notices %q", got)
			}
		})
	}
}

func TestMyChromeNoNoticeWhenChromeAnswersAtOnce(t *testing.T) {
	withNoticeAfter(t, 500*time.Millisecond)
	s, _, _, _ := myChromeFixture(t)
	var log noticeLog
	s.opts.Notify = log.add
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("notices %q", got)
	}
}

func TestMyChromeGivesUpWaitingForConsent(t *testing.T) {
	withNoticeAfter(t, 10*time.Millisecond)
	fb := browsertest.New(t)
	fb.SetWSOnly(true)
	browsertest.NewPage(fb, "https://bank.test/", "Bank", browsertest.EmptyTree)
	fb.SetConsentDelay(0, 5*time.Second)
	dir := t.TempDir()
	fb.WritePortFile(dir)
	o := testOptions()
	o.MyChrome, o.ChromeDir, o.ChromeLabel, o.ConsentWait = true, dir, "stable", 150*time.Millisecond
	s := NewSession(o)
	defer s.Close()
	start := time.Now()
	_, _, err := s.Page(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not allow remote debugging within 150ms") || !strings.Contains(err.Error(), "Allow") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited %s", time.Since(start))
	}
}

func TestMyChromeDeclinedConnection(t *testing.T) {
	s, fb, _, _ := myChromeFixture(t)
	fb.SetRefuse(true)
	_, _, err := s.Page(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Chrome closed the connection") {
		t.Fatalf("got %v", err)
	}
}

func TestMyChromeWaitDefaultsToSixtySeconds(t *testing.T) {
	if got := (&Session{}).consentWait(); got != 60*time.Second {
		t.Fatalf("default consent wait %s", got)
	}
}

func TestAttachMyChromeSwitchesAnOrdinarySession(t *testing.T) {
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "https://bank.test/", "Bank", browsertest.EmptyTree)
	dir := t.TempDir()
	fb.WritePortFile(dir)
	var log noticeLog
	o := testOptions()
	o.Address, o.ChromeDir, o.ChromeLabel, o.Notify = "127.0.0.1:1", dir, "stable", log.add
	s := NewSession(o)
	defer s.Close()
	if s.MyChrome() {
		t.Fatal("my-Chrome mode on before it was asked for")
	}
	s.AttachMyChromeAsync(log.add)
	if !s.MyChrome() {
		t.Fatal("the mode did not switch at once")
	}
	waitFor(t, func() bool { return s.Status().Connected })
	if st := s.Status(); !st.MyChrome || st.Label != "stable" {
		t.Fatalf("status %+v", st)
	}
	waitFor(t, func() bool {
		for _, m := range log.all() {
			if strings.HasPrefix(m, "browser: attached to your Chrome (stable)") {
				return true
			}
		}
		return false
	})
}

func TestHandOverOutsideMyChromeMode(t *testing.T) {
	s, _, _ := testSession(t)
	if _, err := s.HandOver("1"); err == nil {
		t.Fatal("handed over a tab outside my-Chrome mode")
	}
}

// The person lists 1 bank, 2 news, 3 mail; bank closes; "/browser tab 2"
// must not hand over mail.
func TestMyChromeHandOverAfterATabCloses(t *testing.T) {
	s, fb, ps, _ := myChromeFixture(t)
	ps.AddTarget("T2", "https://news.test/", "News", browsertest.EmptyTree, "")
	ps.AddTarget("T3", "https://mail.test/", "Mail", browsertest.EmptyTree, "")
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	listed := s.ListForPerson()
	if len(listed) != 4 || listed[0].Handle != "t1" || listed[1].Handle != "t2" || listed[2].Handle != "t3" {
		t.Fatalf("listing %+v", listed)
	}
	fb.Emit("", "Target.targetDestroyed", map[string]any{"targetId": "T1"})
	waitFor(t, func() bool { return len(s.PersonTabs()) == 3 })
	if _, err := s.HandOver("1"); err == nil || err.Error() != "the tab list changed; run /browser tabs again" {
		t.Fatalf("number of a closed tab: %v", err)
	}
	got, err := s.HandOver("2")
	if err != nil || got.URL != "https://news.test/" {
		t.Fatalf("number 2 still means news: %+v %v", got, err)
	}
	got, err = s.HandOver("T3")
	if err != nil || got.URL != "https://mail.test/" || got.Handle != "t3" {
		t.Fatalf("by id: %+v %v", got, err)
	}
	if _, err := s.HandOver("t1"); err == nil {
		t.Fatal("handed over a closed tab by id")
	}
	// Ids are never reused: a new tab gets a new one.
	ps.AddTarget("T4", "https://new.test/", "New", browsertest.EmptyTree, "")
	fb.Emit("", "Target.targetCreated", map[string]any{"targetInfo": map[string]any{"targetId": "T4", "type": "page", "url": "https://new.test/"}})
	waitFor(t, func() bool { return len(s.PersonTabs()) == 4 })
	for _, tab := range s.PersonTabs() {
		if tab.URL == "https://new.test/" && tab.Handle != "t5" {
			t.Fatalf("new tab handle %q", tab.Handle)
		}
	}
}

func TestMyChromeUntabTakesAHandOverBack(t *testing.T) {
	s, _, _, _ := myChromeFixture(t)
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOver("t1"); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.Page(context.Background())
	if p.targetID != "T1" {
		t.Fatalf("driving %s", p.targetID)
	}
	if _, err := s.Unhand("t2"); err == nil {
		t.Fatal("took back a tab that was never handed over")
	}
	back, err := s.Unhand("t1")
	if err != nil || len(back) != 1 || back[0].Handle != "t1" {
		t.Fatalf("untab: %+v %v", back, err)
	}
	p, _, _ = s.Page(context.Background())
	if p.targetID == "T1" {
		t.Fatal("still driving a tab taken back")
	}
	if tabs, _ := s.Tabs(context.Background()); len(tabs) != 1 {
		t.Fatalf("model's tabs %+v", tabs)
	}
	s.HandOver("t1")
	if back, err := s.Unhand("all"); err != nil || len(back) != 1 {
		t.Fatalf("untab all: %+v %v", back, err)
	}
}

func TestMyChromeCloseCancelsAnAttachWaitingForConsent(t *testing.T) {
	withNoticeAfter(t, 10*time.Millisecond)
	fb := browsertest.New(t)
	fb.SetWSOnly(true)
	browsertest.NewPage(fb, "https://bank.test/", "Bank", browsertest.EmptyTree)
	fb.SetConsentDelay(0, 30*time.Second)
	dir := t.TempDir()
	fb.WritePortFile(dir)
	var log noticeLog
	o := testOptions()
	o.ChromeDir, o.ChromeLabel, o.Notify = dir, "stable", log.add
	s := NewSession(o)
	var reports noticeLog
	s.AttachMyChromeAsync(reports.add)
	waitFor(t, func() bool { return len(log.all()) > 0 }) // waiting on the prompt
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close waited %s for the attach", d)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(fb.Calls("Target.createTarget")); n != 0 {
		t.Fatalf("opened %d tabs after Close", n)
	}
	if got := reports.all(); len(got) != 0 {
		t.Fatalf("a cancelled attach reported %q", got)
	}
}

func TestMyChromeCancelAttachThenAttachAgain(t *testing.T) {
	withNoticeAfter(t, 10*time.Millisecond)
	fb := browsertest.New(t)
	browsertest.NewPage(fb, "https://bank.test/", "Bank", browsertest.EmptyTree)
	fb.SetConsentDelay(0, 30*time.Second)
	dir := t.TempDir()
	fb.WritePortFile(dir)
	var log noticeLog
	o := testOptions()
	o.ChromeDir, o.ChromeLabel, o.Notify = dir, "stable", log.add
	s := NewSession(o)
	defer s.Close()
	s.AttachMyChromeAsync(nil)
	waitFor(t, func() bool { return len(log.all()) > 0 })
	if !s.CancelAttach() || s.CancelAttach() {
		t.Fatal("CancelAttach")
	}
	fb.SetConsentDelay(0, 0)
	var reports noticeLog
	s.AttachMyChromeAsync(reports.add)
	waitFor(t, func() bool { return len(reports.all()) == 1 })
	if r := reports.all()[0]; !strings.HasPrefix(r, "browser: attached to your Chrome (stable)") {
		t.Fatalf("report %q", r)
	}
}

func TestMyChromeReconnectClosesTheLostConnectionsTabs(t *testing.T) {
	s, fb, _, _ := myChromeFixture(t)
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := p.targetID
	fb.Drop()
	var notes []string
	waitFor(t, func() bool {
		_, n, err := s.Page(context.Background())
		notes = n
		return err == nil && len(n) > 0
	})
	if len(notes) != 1 || notes[0] != "the connection to your Chrome was lost; reconnected" {
		t.Fatalf("notes %q", notes)
	}
	closed := fb.Calls("Target.closeTarget")
	if len(closed) != 1 || !strings.Contains(string(closed[0].Params), `"`+old+`"`) {
		t.Fatalf("closed %+v", closed)
	}
}

func TestMyChromeHandlesSurviveAReconnect(t *testing.T) {
	s, fb, ps, _ := myChromeFixture(t)
	ps.AddTarget("T2", "https://mail.test/", "Mail", browsertest.EmptyTree, "")
	p, _, err := s.Page(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ours := p.targetID
	before := map[string]string{}
	for _, tab := range s.ListForPerson() {
		before[tab.id] = tab.Handle
	}
	fb.Drop()
	waitFor(t, func() bool {
		_, n, err := s.Page(context.Background())
		return err == nil && len(n) > 0
	})
	after := s.ListForPerson()
	seen := map[string]bool{}
	for _, tab := range after {
		if h, ok := before[tab.id]; ok && h != tab.Handle {
			t.Fatalf("%s was %s, now %s", tab.id, h, tab.Handle)
		}
		if seen[tab.Handle] {
			t.Fatalf("handle %s reused: %+v", tab.Handle, after)
		}
		seen[tab.Handle] = true
		if tab.id == ours {
			t.Fatal("the lost connection's tab is still listed")
		}
	}
	got, err := s.HandOver(before["T2"])
	if err != nil || got.URL != "https://mail.test/" {
		t.Fatalf("hand over by an old id: %+v %v", got, err)
	}
	if _, err := s.HandOver(before[ours]); err == nil || err.Error() != "no open tab is "+before[ours]+"; /browser tabs lists them" {
		t.Fatalf("a closed tab's old id: %v", err)
	}
}

func TestMyChromeNotesNameTabsByHostUnlessAllowed(t *testing.T) {
	s, _, _, _ := myChromeFixture(t)
	s.opts.TitleOK = func(h string) bool { return h == "bank.test" }
	if _, _, err := s.Page(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.ListForPerson()
	s.HandOver("t1")
	if _, notes, _ := s.Page(context.Background()); len(notes) != 1 || notes[0] != "now driving the tab you handed over: My accounts — Bank" {
		t.Fatalf("allow-tier title: %q", notes)
	}
}

func TestShortURL(t *testing.T) {
	for in, want := range map[string]string{
		"about:blank":                         "about:blank",
		"data:text/html,<h1>secret</h1>":      "data:text/html…",
		"data:text/html;base64,PGgxPg==":      "data:text/html…",
		"blob:https://evil.test/0b6a-11ee-be": "blob:https://evil.test/0…",
		"":                                    "",
	} {
		if got := ShortURL(in); got != want {
			t.Errorf("ShortURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttachFinishedLeavesNothingToCancel(t *testing.T) {
	s, _, _, _ := myChromeFixture(t)
	var reports noticeLog
	s.AttachMyChromeAsync(reports.add)
	waitFor(t, func() bool { return len(reports.all()) == 1 })
	waitFor(t, func() bool {
		s.attachMu.Lock()
		defer s.attachMu.Unlock()
		return s.attachCancel == nil
	})
	if s.CancelAttach() {
		t.Fatal("a finished attach was still cancellable")
	}
}
