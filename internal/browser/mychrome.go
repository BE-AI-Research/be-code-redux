package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The person's own Chrome ("my-Chrome mode"): Chrome 144 and later can have
// remote debugging turned on from chrome://inspect/#remote-debugging, after
// which it writes DevToolsActivePort into its user-data dir and asks "Allow
// remote debugging?" for each connection. In this mode BE-Code connects to
// that port over the WebSocket alone (no /json endpoints are relied on),
// never launches anything, works only in tabs it opened itself (or a person
// handed over with /browser tab), and closes only those.

// ChromeToggle is where the person turns remote debugging on.
const ChromeToggle = "chrome://inspect/#remote-debugging"

// ChromeConsentNotice is said, once per connection attempt, while Chrome is
// holding the connection for the person's answer.
const ChromeConsentNotice = "Chrome is asking to allow remote debugging — click Allow in Chrome"

// Swappable for tests.
var (
	userHomeDir = os.UserHomeDir
	// consentNoticeAfter is how long a connection may take before it is
	// taken to be waiting on Chrome's prompt: a plain local connection
	// answers in milliseconds.
	consentNoticeAfter = time.Second
)

// defaultConsentWait bounds the wait for the person to answer Chrome's
// prompt.
const defaultConsentWait = 60 * time.Second

// ParseDevToolsActivePort reads the file Chrome writes into its user-data
// dir: the port on the first line, the browser target path
// (/devtools/browser/<id>) on the second. Lines are trimmed and empty ones
// dropped; both are required and the port must be 1–65535.
func ParseDevToolsActivePort(data []byte) (port int, path string, err error) {
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return 0, "", errors.New("it does not name both a port and a browser path")
	}
	n, err := strconv.Atoi(lines[0])
	if err != nil || n <= 0 || n > 65535 {
		return 0, "", fmt.Errorf("%q is not a port", lines[0])
	}
	if !strings.HasPrefix(lines[1], "/") {
		return 0, "", fmt.Errorf("%q is not a browser path", lines[1])
	}
	return n, lines[1], nil
}

// chromeChannels names the channels chrome_channel accepts.
var chromeChannels = map[string]bool{"stable": true, "beta": true, "dev": true, "canary": true}

// ResolveChromeDir is the user-data dir my-Chrome mode reads
// DevToolsActivePort from, and the label /browser shows for it: an explicit
// dir wins (labelled by itself); otherwise the channel's default location,
// an unknown channel warning and falling back to stable. dir is "" when no
// home directory can be found.
func ResolveChromeDir(channel, explicitDir string) (dir, label, warn string) {
	if d := strings.TrimSpace(explicitDir); d != "" {
		return d, d, ""
	}
	ch := strings.ToLower(strings.TrimSpace(channel))
	if ch == "" {
		ch = "stable"
	}
	if !chromeChannels[ch] {
		warn = fmt.Sprintf("browser.chrome_channel %q is not one of stable|beta|dev|canary; using stable", strings.TrimSpace(channel))
		ch = "stable"
	}
	home, err := userHomeDir()
	if err != nil || home == "" {
		return "", ch, warn
	}
	return chromeUserDataDir(goos, ch, home, getenv), ch, warn
}

// chromeUserDataDir is a channel's default user-data dir on goos.
func chromeUserDataDir(goos, channel, home string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		name := map[string]string{"stable": "Chrome", "beta": "Chrome Beta", "dev": "Chrome Dev", "canary": "Chrome Canary"}[channel]
		return filepath.Join(home, "Library", "Application Support", "Google", name)
	case "windows":
		base := getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		name := map[string]string{"stable": "Chrome", "beta": "Chrome Beta", "dev": "Chrome Dev", "canary": "Chrome SxS"}[channel]
		return filepath.Join(base, "Google", name, "User Data")
	}
	base := getenv("CHROME_CONFIG_HOME")
	if base == "" {
		base = getenv("XDG_CONFIG_HOME")
	}
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	name := map[string]string{"stable": "google-chrome", "beta": "google-chrome-beta", "dev": "google-chrome-unstable", "canary": "google-chrome-canary"}[channel]
	return filepath.Join(base, name)
}

// ReadDevToolsActivePort reads and parses dir's DevToolsActivePort.
func ReadDevToolsActivePort(dir string) (port int, path string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
	if err != nil {
		return 0, "", err
	}
	return ParseDevToolsActivePort(b)
}

// PortFileError is what a missing or unreadable DevToolsActivePort is
// reported as, naming the dir looked in and the toggle that writes it.
func PortFileError(dir string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("your Chrome has no remote debugging port open (no DevToolsActivePort in %s) — in Chrome, open %s and turn remote debugging on, then try again", dir, ChromeToggle)
	}
	return fmt.Errorf("could not read DevToolsActivePort in %s (%v) — in Chrome, open %s and turn remote debugging on again, then try again", dir, err, ChromeToggle)
}

func (s *Session) consentWait() time.Duration {
	if s.opts.ConsentWait > 0 {
		return s.opts.ConsentWait
	}
	return defaultConsentWait
}

func (s *Session) notify(msg string) {
	if s.opts.Notify != nil {
		s.opts.Notify(msg)
	}
}

// dialMyChrome connects to the person's Chrome from its DevToolsActivePort
// and waits — bounded, with one notice — for the person to allow it.
func (s *Session) dialMyChrome(ctx context.Context) (conn *Conn, product, addr string, err error) {
	dir := s.opts.ChromeDir
	if dir == "" {
		return nil, "", "", fmt.Errorf("could not find your Chrome's user-data dir — set browser.chrome_user_data_dir, and turn remote debugging on at %s", ChromeToggle)
	}
	port, path, err := ReadDevToolsActivePort(dir)
	if err != nil {
		return nil, "", "", PortFileError(dir, err)
	}
	addr = "127.0.0.1:" + strconv.Itoa(port)
	wait := s.consentWait()
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	fired := make(chan struct{})
	timer := time.AfterFunc(consentNoticeAfter, func() {
		defer close(fired)
		s.notify(ChromeConsentNotice)
	})
	conn, err = Dial(cctx, "ws://"+addr+path)
	var ver struct {
		Product string `json:"product"`
	}
	if err == nil {
		// The first command is where a held connection shows: Chrome may
		// complete the upgrade and answer nothing until Allow is clicked.
		if err = conn.Call(cctx, "", "Browser.getVersion", nil, &ver); err != nil {
			conn.Close()
		}
	}
	if !timer.Stop() {
		<-fired
		s.notify("")
	}
	if err == nil {
		return conn, ver.Product, addr, nil
	}
	switch {
	case ctx.Err() != nil:
		return nil, "", "", ctx.Err()
	case cctx.Err() != nil:
		return nil, "", "", fmt.Errorf("Chrome did not allow remote debugging within %s — click Allow in Chrome's \"Allow remote debugging?\" prompt, then try again", wait)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return nil, "", "", fmt.Errorf("nothing is listening on port %d named in %s (a stale file left by a Chrome that has exited?) — open Chrome and turn remote debugging on at %s, then try again",
			port, filepath.Join(dir, "DevToolsActivePort"), ChromeToggle)
	}
	return nil, "", "", fmt.Errorf("Chrome closed the connection (%v) — was remote debugging declined? Click Allow when Chrome asks, or check %s, then try again", err, ChromeToggle)
}

// connectMineLocked attaches to the person's Chrome. A fresh connection
// owns no tab yet, so the agent's first page is always a new tab.
func (s *Session) connectMineLocked(ctx context.Context) (string, error) {
	conn, product, addr, err := s.dialMyChrome(ctx)
	if err != nil {
		return "", err
	}
	s.resetTargetsLocked()
	unsub := conn.Subscribe(s.onEvent)
	if err := conn.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		unsub()
		conn.Close()
		return "", err
	}
	// Seed the tab cache the person's /browser tabs reads; events keep it
	// current from here.
	var ts struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if conn.Call(ctx, "", "Target.getTargets", nil, &ts) == nil {
		s.evMu.Lock()
		for _, t := range ts.TargetInfos {
			if t.Type == "page" {
				s.seeLocked(t)
			}
		}
		s.evMu.Unlock()
	}
	s.conn, s.unsub, s.connMine, s.listAddr = conn, unsub, true, ""
	s.stMu.Lock()
	s.st = Status{Connected: true, Product: product, Address: addr, MyChrome: true, Label: s.opts.ChromeLabel}
	s.stMu.Unlock()
	return "attached to your Chrome (" + s.opts.ChromeLabel + "); working in a new tab — your own tabs stay private", nil
}

// attachLocked attaches to one tab. In my-Chrome mode downloads are
// refused in that tab alone: the browser-wide setting would refuse the
// person's own downloads too.
func (s *Session) attachLocked(ctx context.Context, id string) (*Page, error) {
	p, err := attachPage(ctx, s.conn, id, s.opts)
	if err != nil || !s.connMine {
		return p, err
	}
	if p.call(ctx, "Page.setDownloadBehavior", map[string]any{"behavior": "deny"}, nil) != nil {
		p.mu.Lock()
		p.addNoteLocked("downloads could not be refused in this tab")
		p.mu.Unlock()
	}
	return p, nil
}

func (s *Session) handOverPending() bool {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.handTo != ""
}

// pickMineLocked chooses the tab to drive in the person's Chrome: a tab
// just handed over, else the newest of the agent's own, else a new one.
// It never looks at — never mind attaches to — any other tab.
func (s *Session) pickMineLocked(ctx context.Context) (string, error) {
	lost := s.page != nil && s.isDestroyed(s.page.targetID)
	if s.page != nil {
		s.page.release()
		s.page = nil
	}
	s.evMu.Lock()
	hand := s.handTo
	s.handTo = ""
	s.evMu.Unlock()
	if hand != "" && !s.isDestroyed(hand) {
		if p, err := s.attachLocked(ctx, hand); err == nil {
			s.page = p
			s.conn.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": hand}, nil)
			title, _ := p.Info(ctx)
			return "now driving the tab you handed over: " + title, nil
		}
	}
	s.evMu.Lock()
	id := ""
	for i := len(s.pages) - 1; i >= 0; i-- {
		if t := s.pages[i]; (s.opened[t] || s.handed[t]) && !s.destroyed[t] {
			id = t
			break
		}
	}
	s.evMu.Unlock()
	created := false
	if id == "" {
		var err error
		if id, err = s.createBlankTabLocked(ctx); err != nil {
			return "", err
		}
		s.evMu.Lock()
		s.opened[id] = true
		s.seeLocked(targetInfo{TargetID: id, Type: "page", URL: "about:blank"})
		s.evMu.Unlock()
		created = true
	}
	p, err := s.attachLocked(ctx, id)
	if err != nil {
		return "", err
	}
	s.page = p
	switch {
	case !lost:
		return "", nil
	case created:
		return "the tab being driven was closed; opened a new one", nil
	}
	title, _ := p.Info(ctx)
	return "the tab being driven was closed; now driving: " + title, nil
}

// closeOwnTabsLocked closes, best effort and bounded, every tab this
// connection opened — never a tab of the person's, never the browser.
func (s *Session) closeOwnTabsLocked() {
	s.closeTabsLocked(context.Background(), s.ownTabsLocked())
}

// ownTabsLocked lists the tabs this connection opened that are still open.
func (s *Session) ownTabsLocked() []string {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	var ids []string
	for _, id := range s.pages {
		if s.opened[id] && !s.destroyed[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

// closeTabsLocked closes ids on the live connection, bounded to 2 s.
func (s *Session) closeTabsLocked(ctx context.Context, ids []string) {
	if len(ids) == 0 || s.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	for _, id := range ids {
		s.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id}, nil)
	}
}

// agentMayUse reports whether the agent may drive tab id: one it opened,
// or one handed over and not taken back. Always true outside my-Chrome mode.
func (s *Session) agentMayUse(id string) bool {
	if !s.connMine {
		return true
	}
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.opened[id] || s.handed[id]
}

// PersonTabs is every open tab of the person's Chrome, for the person's
// own /browser tabs — never for the model. It reads the cache the target
// events keep, so it never waits on the browser; empty when not attached
// to the person's Chrome.
func (s *Session) PersonTabs() []Tab {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.personTabsLocked()
}

func (s *Session) personTabsLocked() []Tab {
	var out []Tab
	for _, id := range s.pages {
		if s.destroyed[id] {
			continue
		}
		t := s.info[id]
		out = append(out, Tab{Index: len(out) + 1, Title: t.Title, URL: t.URL, id: id, Handle: s.handles[id],
			Current: id == s.curID, Agent: s.opened[id] || s.handed[id]})
	}
	return out
}

// ListForPerson is PersonTabs for /browser tabs itself: it also remembers
// the listing, so a number typed after it means the tab listed under it.
func (s *Session) ListForPerson() []Tab {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	tabs := s.personTabsLocked()
	s.listed = s.listed[:0]
	for _, t := range tabs {
		s.listed = append(s.listed, t.id)
	}
	return tabs
}

// errTabsChanged refuses a number whose tab is no longer the one listed.
var errTabsChanged = errors.New("the tab list changed; run /browser tabs again")

// resolveTabLocked finds the tab a person named: a handle ("t3"), or a
// number from their last /browser tabs that still names the same open tab.
func (s *Session) resolveTabLocked(ref string) (Tab, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	tabs := s.personTabsLocked()
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(s.listed) {
			if len(s.listed) == 0 {
				return Tab{}, errors.New("run /browser tabs first, then name a tab by its number or id")
			}
			return Tab{}, fmt.Errorf("there is no tab %d; /browser tabs listed %d", n, len(s.listed))
		}
		id := s.listed[n-1]
		for _, t := range tabs {
			if t.id == id {
				return t, nil
			}
		}
		return Tab{}, errTabsChanged
	}
	for _, t := range tabs {
		if t.Handle == ref {
			return t, nil
		}
	}
	return Tab{}, fmt.Errorf("no open tab is %s; /browser tabs lists them", ref)
}

func (s *Session) personGate() error {
	if !s.myChrome.Load() {
		return errors.New("/browser tab hands over a tab of your own Chrome (/browser attach); otherwise the model already sees every tab")
	}
	return nil
}

// HandOver gives the agent the tab ref names (a handle like "t3", or a
// number from the person's last /browser tabs): it may work in it from its
// next browser call (and it is driven then), but it is never closed on the
// person's behalf. Only a person's command calls it; it never waits.
func (s *Session) HandOver(ref string) (Tab, error) {
	if err := s.personGate(); err != nil {
		return Tab{}, err
	}
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if len(s.pages) == 0 {
		return Tab{}, errors.New("not attached to your Chrome yet — its tabs are listed once it is (the model's first browser call, or /browser attach)")
	}
	t, err := s.resolveTabLocked(ref)
	if err != nil {
		return Tab{}, err
	}
	s.handed[t.id] = true
	s.handTo = t.id
	t.Agent = true
	return t, nil
}

// Unhand takes hand-overs back: the tab ref names, or every one for "all".
// The agent stops driving a tab taken back from its next browser call. It
// never waits; it reports the tabs taken back.
func (s *Session) Unhand(ref string) ([]Tab, error) {
	if err := s.personGate(); err != nil {
		return nil, err
	}
	s.evMu.Lock()
	defer s.evMu.Unlock()
	var out []Tab
	if strings.EqualFold(strings.TrimSpace(ref), "all") {
		for _, t := range s.personTabsLocked() {
			if s.handed[t.id] {
				out = append(out, t)
			}
		}
		s.handed, s.handTo = map[string]bool{}, ""
		return out, nil
	}
	t, err := s.resolveTabLocked(ref)
	if err != nil {
		return nil, err
	}
	if !s.handed[t.id] {
		return nil, fmt.Errorf("%s was not handed over", t.Handle)
	}
	delete(s.handed, t.id)
	if s.handTo == t.id {
		s.handTo = ""
	}
	return []Tab{t}, nil
}

// AttachMyChromeAsync is /browser attach: the session switches to the
// person's own Chrome for the rest of its life (the config is unchanged),
// dropping any other connection — closing a browser it launched — and
// connects on a goroutine of its own, handing report the outcome (nothing
// when it was cancelled). It never waits. Close, CloseAsync, CancelAttach
// and a second attach cancel one still waiting on Chrome's prompt.
func (s *Session) AttachMyChromeAsync(report func(string)) {
	s.myChrome.Store(true)
	s.stMu.Lock()
	if !s.st.Connected {
		s.st.MyChrome, s.st.Label = true, s.opts.ChromeLabel
	}
	s.stMu.Unlock()
	s.cancelAttach()
	ctx, cancel := context.WithCancel(context.Background())
	s.attachMu.Lock()
	s.attachCancel = cancel
	s.attachMu.Unlock()
	go func() {
		defer cancel()
		s.mu.Lock()
		if s.conn != nil && !s.connMine && ctx.Err() == nil {
			s.disconnectLocked()
		}
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		_, notes, err := s.Page(ctx)
		if ctx.Err() != nil || report == nil {
			return
		}
		switch {
		case err != nil:
			report("browser: " + err.Error())
		case len(notes) > 0:
			report("browser: " + strings.Join(notes, "; "))
		default:
			report("browser: attached to your Chrome (" + s.opts.ChromeLabel + ")")
		}
	}()
}

// CancelAttach ends a /browser attach still waiting on Chrome's prompt,
// reporting whether there was one. It never waits.
func (s *Session) CancelAttach() bool { return s.cancelAttach() }

func (s *Session) cancelAttach() bool {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.attachCancel == nil {
		return false
	}
	s.attachCancel()
	s.attachCancel = nil
	return true
}
