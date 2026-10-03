package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId"`
}

type createdTarget struct {
	id, opener string
	at         time.Time
}

// closeWaitTimeout bounds how long Close waits for a launched browser to
// exit on its own, after Browser.close has replied, before killing it. A
// var so a test can shorten it.
var closeWaitTimeout = 5 * time.Second

// errEnded is what every operation returns once Close has run: the session
// is final and must never reconnect or launch again.
var errEnded = errors.New("the browser session has ended")

// jsonListEntry is one entry of GET /json/list — real Chromium's own
// endpoint, which (unlike Target.getTargets) lists tabs most-recently-active
// first.
type jsonListEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// Tab is one open tab, for the tabs action.
type Tab struct {
	Index      int
	Title, URL string
	Current    bool
	// Agent marks, in the person's own list (PersonTabs), a tab the agent
	// may work in: one it opened, or one handed over with /browser tab.
	Agent bool
	// Handle is the person's stable name for the tab ("t3"), for
	// /browser tab and /browser untab; my-Chrome mode only.
	Handle string
	id     string
}

// Status is what /browser shows. Reading it never waits on the browser.
type Status struct {
	Connected, Launched   bool
	Product, Address, Exe string
	Title, URL            string
	// MyChrome: attached (or, not yet connected, set to attach) to the
	// person's own Chrome; Label is its channel or user-data dir.
	MyChrome bool
	Label    string
}

// Session owns the connection to the browser for one BE-Code session. It
// attaches or launches lazily on first use (spec §1.3), reconnects when the
// browser goes away, follows a tab an action opens, and closes a browser
// only if it launched it.
type Session struct {
	opts Options

	mu       sync.Mutex // one operation at a time
	conn     *Conn
	proc     *Process
	page     *Page
	unsub    func()
	ended    bool   // Close has run; never reconnect or launch again
	listAddr string // where GET /json/list is asked: opts.Address when attached, the launched browser's own host:port otherwise
	connMine bool   // the live connection is to the person's own Chrome

	// myChrome is the mode the next connection uses: Options.MyChrome, or
	// /browser attach (AttachMyChromeAsync). Read without waiting.
	myChrome atomic.Bool

	evMu      sync.Mutex // written from the connection's reader goroutine
	created   []createdTarget
	destroyed map[string]bool
	// The tab cache and ownership, kept current from target events so the
	// person's /browser tabs and /browser tab never wait on the browser.
	pages  []string              // page targets, in the order first seen
	info   map[string]targetInfo // their last known title and URL
	opened map[string]bool       // tabs this connection opened (closed on disconnect)
	handed map[string]bool       // tabs the person handed over (never closed)
	handTo string                // a handed-over tab to drive from the next Page
	curID  string                // the tab being driven, as last recorded
	// handles are the person's stable names for tabs ("t3"), assigned when
	// a tab is first seen and never reused within a connection; listed is
	// the order of the person's last /browser tabs, so a number typed
	// after it still means the tab that was listed under it.
	handles map[string]string
	nextHdl int
	listed  []string

	// attachCancel ends a /browser attach still waiting (Chrome's prompt):
	// Close, CloseAsync and a second attach call it before taking mu.
	attachMu     sync.Mutex
	attachCancel context.CancelFunc
	attachGen    int

	stMu sync.Mutex // Status reads only this, never mu
	st   Status
}

// NewSession prepares a session; nothing connects until Page.
func NewSession(opts Options) *Session {
	if opts.Address == "" {
		opts.Address = "127.0.0.1:9222"
	}
	s := &Session{opts: opts}
	s.myChrome.Store(opts.MyChrome)
	s.resetTargetsLocked()
	s.st = s.idleStatus()
	return s
}

// MyChrome reports whether the session attaches to the person's own
// Chrome. It never waits.
func (s *Session) MyChrome() bool { return s.myChrome.Load() }

// idleStatus is Status with nothing connected.
func (s *Session) idleStatus() Status {
	return Status{Address: s.opts.Address, MyChrome: s.myChrome.Load(), Label: s.opts.ChromeLabel}
}

// resetTargetsLocked forgets every target, tab and ownership record: each
// connection starts afresh (the agent's tabs are the ones it opened in
// this connection).
func (s *Session) resetTargetsLocked() {
	s.evMu.Lock()
	s.created, s.destroyed = nil, map[string]bool{}
	s.pages, s.info = nil, map[string]targetInfo{}
	s.opened, s.handed = map[string]bool{}, map[string]bool{}
	s.handTo, s.curID = "", ""
	// handles and nextHdl are kept for the life of the session: target ids
	// hold for a Chrome run, so t3 names one tab across a reconnect, and a
	// number is never handed out twice.
	if s.handles == nil {
		s.handles = map[string]string{}
	}
	s.listed = nil
	s.evMu.Unlock()
}

// Page returns the page being driven, connecting first — or reconnecting,
// when the browser went away. The notes say what happened on the way.
func (s *Session) Page(ctx context.Context) (*Page, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, nil, errEnded
	}
	var notes []string
	reconnect := false
	var why error
	var orphans []string // the lost connection's own tabs, in the person's Chrome
	if s.conn != nil {
		select {
		case <-s.conn.Done():
			why = s.conn.Err()
			if s.connMine {
				orphans = s.ownTabsLocked()
			}
			s.dropLocked()
			reconnect = true
		default:
		}
	}
	if s.conn == nil {
		note, err := s.connectLocked(ctx)
		if err != nil {
			return nil, nil, err
		}
		if s.connMine && len(orphans) > 0 {
			// Target ids hold for the life of a Chrome: the tabs the lost
			// connection opened are closed now, if they are still there.
			s.closeTabsLocked(ctx, orphans)
		}
		if reconnect && s.connMine {
			notes = append(notes, "the connection to your Chrome was lost; reconnected")
		} else if reconnect && errors.Is(why, errReaderPanicked) {
			// Spec §5: the connection is marked dead with a notice.
			notes = append(notes, "the browser connection was marked dead ("+panicCause(why)+"); reconnected")
		} else if reconnect {
			notes = append(notes, "the browser was closed; reconnected")
		} else if note != "" {
			notes = append(notes, note)
		}
	}
	if s.connMine {
		if s.page == nil || s.isDestroyed(s.page.targetID) || s.handOverPending() || !s.agentMayUse(s.page.targetID) {
			note, err := s.pickMineLocked(ctx)
			if err != nil {
				return nil, nil, err
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
	} else if s.page == nil || s.isDestroyed(s.page.targetID) {
		note, err := s.pickPageLocked(ctx)
		if err != nil {
			return nil, nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	s.recordLocked()
	return s.page, notes, nil
}

func (s *Session) connectLocked(ctx context.Context) (string, error) {
	if s.myChrome.Load() {
		return s.connectMineLocked(ctx)
	}
	addr := s.opts.Address
	if !s.opts.AllowRemote && !IsLoopbackAddr(addr) {
		return "", fmt.Errorf("browser.address %s is not on this machine; set browser.allow_remote to use it", addr)
	}
	var wsURL, note, listAddr string
	if v, err := FetchVersion(ctx, addr); err == nil {
		wsURL, note, listAddr = v.WebSocketURL, "attached to "+v.Browser+" at "+addr, addr
	} else {
		if !s.opts.Launch {
			return "", s.noBrowser(addr)
		}
		exe := s.opts.Executable
		if exe == "" {
			exe = FindExecutable()
		}
		if exe == "" {
			return "", s.noBrowser(addr)
		}
		headless := s.opts.ForceHeadless || !HasDisplay()
		proc, err := Launch(ctx, exe, s.opts.Profile, headless)
		if err != nil {
			return "", err
		}
		s.proc, wsURL = proc, proc.WSURL
		listAddr = wsHostPort(proc.WSURL)
		mode := "visible"
		if headless {
			mode = "headless"
		}
		note = fmt.Sprintf("launched %s (%s, profile %s)", filepath.Base(exe), mode, s.opts.Profile)
	}
	conn, err := Dial(ctx, wsURL)
	if err != nil {
		s.killLocked()
		return "", fmt.Errorf("connecting to the browser: %w", err)
	}
	var ver struct {
		Product string `json:"product"`
	}
	conn.Call(ctx, "", "Browser.getVersion", nil, &ver)
	// Downloads are refused at the browser level in 1.1.5 (spec §5); a
	// browser that refuses the request itself is noticed, never silently
	// left downloading.
	if err := conn.Call(ctx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}, nil); err != nil {
		note += "; downloads could not be refused by this browser"
	}
	s.listAddr = listAddr
	s.resetTargetsLocked()
	unsub := conn.Subscribe(s.onEvent)
	if err := conn.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		unsub()
		conn.Close()
		s.killLocked()
		return "", err
	}
	s.conn, s.unsub = conn, unsub
	s.stMu.Lock()
	s.st = Status{Connected: true, Launched: s.proc != nil, Product: ver.Product, Address: addr}
	if s.proc != nil {
		s.st.Exe = s.proc.Exe
	}
	s.stMu.Unlock()
	return note, nil
}

func (s *Session) noBrowser(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "9222"
	}
	if !s.opts.Launch {
		return fmt.Errorf("no browser at %s (browser.launch is off) — start one with --remote-debugging-port=%s", addr, port)
	}
	return fmt.Errorf("no browser at %s and none installed to launch — start one with --remote-debugging-port=%s, or set browser.executable", addr, port)
}

// onEvent runs on the reader goroutine: it records, never calls.
func (s *Session) onEvent(ev Event) {
	if ev.SessionID != "" {
		return
	}
	switch ev.Method {
	case "Target.targetCreated":
		var e struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &e) == nil && e.TargetInfo.Type == "page" {
			s.evMu.Lock()
			s.created = append(s.created, createdTarget{id: e.TargetInfo.TargetID, opener: e.TargetInfo.OpenerID, at: time.Now()})
			s.seeLocked(e.TargetInfo)
			// A tab one of the agent's own tabs opened is the agent's too.
			if s.opened[e.TargetInfo.OpenerID] {
				s.opened[e.TargetInfo.TargetID] = true
			}
			s.evMu.Unlock()
		}
	case "Target.targetInfoChanged":
		var e struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &e) == nil && e.TargetInfo.Type == "page" {
			s.evMu.Lock()
			s.seeLocked(e.TargetInfo)
			s.evMu.Unlock()
		}
	case "Target.targetDestroyed":
		var e struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(ev.Params, &e) == nil {
			s.evMu.Lock()
			s.destroyed[e.TargetID] = true
			s.evMu.Unlock()
		}
	}
}

// seeLocked records a page target's title and URL (under evMu).
func (s *Session) seeLocked(t targetInfo) {
	if _, ok := s.info[t.TargetID]; !ok {
		s.pages = append(s.pages, t.TargetID)
		if _, ok := s.handles[t.TargetID]; !ok {
			s.nextHdl++
			s.handles[t.TargetID] = "t" + strconv.Itoa(s.nextHdl)
		}
	}
	s.info[t.TargetID] = t
}

func (s *Session) isDestroyed(id string) bool {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.destroyed[id]
}

// wsHostPort is the host:port a browser-level WebSocket URL (Process.WSURL)
// answers GET requests on too — the launched browser's own DevTools HTTP
// endpoint, as opposed to the configured Options.Address of one attached to.
func wsHostPort(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// fetchTargetList asks addr for GET /json/list, bounded to 2s: real
// Chromium's own endpoint for the tab list, ordered most-recently-active
// first — unlike Target.getTargets, which documents no order at all.
func fetchTargetList(ctx context.Context, addr string) ([]jsonListEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/json/list: %s", addr, resp.Status)
	}
	var list []jsonListEntry
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// mostRecentPageLocked picks the tab a real user would consider "current":
// the first page entry of GET /json/list when that succeeds and names one
// still open (fromList true), else the last live page target of
// Target.getTargets (fromList false — the order this package used before
// /json/list was consulted, and still the fallback when a browser has no
// such endpoint or lists nothing usable).
func (s *Session) mostRecentPageLocked(ctx context.Context) (id string, fromList bool, err error) {
	if list, lerr := fetchTargetList(ctx, s.listAddr); lerr == nil {
		for _, e := range list {
			if e.Type == "page" && !s.isDestroyed(e.ID) {
				return e.ID, true, nil
			}
		}
	}
	id, err = s.getTargetsPageLocked(ctx)
	return id, false, err
}

// getTargetsPageLocked is the pre-/json/list rule on its own: the last live
// page target of Target.getTargets, or "" when there is none.
func (s *Session) getTargetsPageLocked(ctx context.Context) (string, error) {
	var ts struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := s.conn.Call(ctx, "", "Target.getTargets", nil, &ts); err != nil {
		return "", err
	}
	for i := len(ts.TargetInfos) - 1; i >= 0; i-- {
		if t := ts.TargetInfos[i]; t.Type == "page" && !s.isDestroyed(t.TargetID) {
			return t.TargetID, nil
		}
	}
	return "", nil
}

// createBlankTabLocked opens a fresh about:blank tab.
func (s *Session) createBlankTabLocked(ctx context.Context) (string, error) {
	var c struct {
		TargetID string `json:"targetId"`
	}
	if err := s.conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &c); err != nil {
		return "", err
	}
	return c.TargetID, nil
}

// pickPageLocked attaches to the most recent open tab, or opens one. A
// /json/list pick can name a tab that has just closed — its
// Target.targetDestroyed not yet processed, so isDestroyed does not yet
// know it — in which case the attach fails; that gets one retry against
// the getTargets-only pick (the same live call the attach was always
// paired with before /json/list existed), and only a failure of that
// retry propagates.
func (s *Session) pickPageLocked(ctx context.Context) (string, error) {
	lost := s.page != nil
	if s.page != nil {
		s.page.release()
		s.page = nil
	}
	id, fromList, err := s.mostRecentPageLocked(ctx)
	if err != nil {
		return "", err
	}
	opened := false
	if id == "" {
		if id, err = s.createBlankTabLocked(ctx); err != nil {
			return "", err
		}
		opened = true
	}
	p, attachErr := s.attachLocked(ctx, id)
	if attachErr != nil && fromList {
		if gid, gerr := s.getTargetsPageLocked(ctx); gerr == nil {
			id, opened = gid, false
			if id == "" {
				if id, gerr = s.createBlankTabLocked(ctx); gerr == nil {
					opened = true
				}
			}
			if gerr == nil {
				p, attachErr = s.attachLocked(ctx, id)
			}
		}
	}
	if attachErr != nil {
		return "", attachErr
	}
	s.page = p
	if !lost {
		return "", nil
	}
	if opened {
		return "the tab being driven was closed; opened a new one", nil
	}
	return "the tab being driven was closed; now driving: " + s.tabName(ctx, p), nil
}

// AfterAction follows a tab the last action opened (a target=_blank link,
// window.open): it becomes the tab driven, settled, with a note.
func (s *Session) AfterAction(ctx context.Context, since time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return "", errEnded
	}
	if s.page == nil || s.conn == nil {
		return "", nil
	}
	s.evMu.Lock()
	pick := ""
	for _, c := range s.created {
		if c.opener == s.page.targetID && !c.at.Before(since) && !s.destroyed[c.id] {
			pick = c.id
		}
	}
	s.created = nil
	if pick != "" && s.connMine {
		// The agent's own action opened it: it is the agent's to drive
		// and to close.
		s.opened[pick] = true
	}
	s.evMu.Unlock()
	if pick == "" {
		return "", nil
	}
	p, err := s.attachLocked(ctx, pick)
	if err != nil {
		return "", err
	}
	s.page.release()
	s.page = p
	s.conn.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": pick}, nil)
	p.settle(ctx)
	name := s.tabName(ctx, p)
	s.recordLocked()
	return "switched to the new tab: " + name, nil
}

// Tabs lists the open tabs, marking the one being driven.
func (s *Session) Tabs(ctx context.Context) ([]Tab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, errEnded
	}
	return s.tabsLocked(ctx)
}

func (s *Session) tabsLocked(ctx context.Context) ([]Tab, error) {
	if s.conn == nil {
		return nil, ErrClosed
	}
	var ts struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := s.conn.Call(ctx, "", "Target.getTargets", nil, &ts); err != nil {
		return nil, err
	}
	var out []Tab
	for _, t := range ts.TargetInfos {
		if t.Type != "page" || s.isDestroyed(t.TargetID) {
			continue
		}
		if s.connMine {
			// The person's own tabs are never listed to the model, not
			// even by title: only the agent's own and handed-over ones.
			s.evMu.Lock()
			s.seeLocked(t)
			mine := s.opened[t.TargetID] || s.handed[t.TargetID]
			s.evMu.Unlock()
			if !mine {
				continue
			}
		}
		out = append(out, Tab{Index: len(out) + 1, Title: t.Title, URL: t.URL, id: t.TargetID,
			Current: s.page != nil && t.TargetID == s.page.targetID})
	}
	return out, nil
}

// SwitchTab drives tab n (1-based, as Tabs numbers them).
func (s *Session) SwitchTab(ctx context.Context, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errEnded
	}
	tabs, err := s.tabsLocked(ctx)
	if err != nil {
		return err
	}
	if n < 1 || n > len(tabs) {
		return fmt.Errorf("there is no tab %d; there are %d", n, len(tabs))
	}
	if tabs[n-1].Current {
		return nil
	}
	p, err := s.attachLocked(ctx, tabs[n-1].id)
	if err != nil {
		return err
	}
	if s.page != nil {
		s.page.release()
	}
	s.page = p
	s.conn.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": tabs[n-1].id}, nil)
	p.settle(ctx)
	s.recordLocked()
	return nil
}

// Record copies the page's title and URL into Status; the tool calls it
// after each action, once the snapshot has refreshed them.
func (s *Session) Record() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked()
}

func (s *Session) recordLocked() {
	if s.page == nil {
		return
	}
	s.evMu.Lock()
	s.curID = s.page.targetID
	s.evMu.Unlock()
	t, u := s.page.Title(), s.page.URL()
	s.stMu.Lock()
	s.st.Title, s.st.URL = t, u
	s.stMu.Unlock()
}

// Status is a copy of the session's state; it never waits on an operation.
func (s *Session) Status() Status {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.st
}

// Close disconnects, finally: once it has run, the session never
// reconnects or launches again (Page, AfterAction, Tabs and SwitchTab all
// return errEnded). A browser BE-Code launched is closed (asked first, then
// killed only if it does not exit on its own); one it attached to is left
// running.
func (s *Session) Close() {
	s.cancelAttach()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnectLocked()
	s.ended = true
}

// CloseAsync disconnects on a goroutine of its own, for a caller that must
// not wait (the TUI's Update goroutine, answering /browser close) — a
// restartable disconnect, unlike Close: Page reconnects (or relaunches)
// after it, exactly as after the browser dropping on its own.
func (s *Session) CloseAsync() { go s.disconnect() }

func (s *Session) disconnect() {
	s.cancelAttach()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnectLocked()
}

func (s *Session) disconnectLocked() {
	if s.conn != nil {
		if s.page != nil {
			s.page.release()
		}
		if s.connMine {
			s.closeOwnTabsLocked()
		}
		if s.proc != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := s.conn.Call(ctx, "", "Browser.close", nil, nil)
			cancel()
			if err == nil {
				// Give it a chance to shut down cleanly — flush the
				// persistent profile's cookies and session state — before
				// resorting to a kill (spec §1.3, amended).
				select {
				case <-s.proc.Exited():
				case <-time.After(closeWaitTimeout):
				}
			}
		}
		if s.unsub != nil {
			s.unsub()
			s.unsub = nil
		}
		s.conn.Close()
		s.conn = nil
	}
	s.page = nil
	s.connMine = false
	s.resetTargetsLocked()
	s.killLocked()
	s.stMu.Lock()
	s.st = s.idleStatus()
	s.stMu.Unlock()
}

// dropLocked forgets a connection that has already died.
func (s *Session) dropLocked() {
	if s.unsub != nil {
		s.unsub()
		s.unsub = nil
	}
	s.conn, s.page = nil, nil
	s.connMine = false
	s.resetTargetsLocked()
	s.killLocked()
	s.stMu.Lock()
	s.st = s.idleStatus()
	s.stMu.Unlock()
}

func (s *Session) killLocked() {
	if s.proc != nil {
		s.proc.Kill()
		s.proc = nil
	}
}

// panicCause is the "its reader panicked: <value>" tail of a panicked
// connection's error, clipped.
func panicCause(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, errReaderPanicked.Error()); i >= 0 {
		msg = msg[i:]
	}
	return clip(msg, 200)
}
