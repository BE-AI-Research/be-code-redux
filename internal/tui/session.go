package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/commands"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/ui"
)

// Session is the shared core of a BE-Code UI: one per host, whatever the
// number of terminals watching it. It owns everything the terminals have in
// common — the agent and its event wiring, the transcript as raw entries,
// the run state, the client roster, the input history, the review
// coordinator — while each View owns what is genuinely local to one
// terminal: its size, its styles, its viewport, its drafts and its popups.
//
// Locking. mu guards every field below except viewsMu/views and the test
// seams. View.Update and View.View take it for their whole body, so Session
// methods come in two flavours: the plain ones lock (they are what the agent
// goroutine calls), and the ...Locked variants assume the caller already
// holds mu (they are what Update calls). broadcast is the exception that
// makes this work: it takes only viewsMu and never blocks, so it is safe
// from either side — nothing under mu may block on a channel that the Update
// goroutine is the one draining.
type Session struct {
	mu sync.Mutex // guards every field below except viewsMu/views and the test seams

	cfg      *config.Config
	ag       *agent.Agent
	prov     provider.Provider
	rootCtx  context.Context
	cancelFn context.CancelFunc
	// consultCancel stops a /consult asked while a run was in progress: it
	// runs on the root context rather than the turn's, so Esc and /quit
	// reach it here instead of through cancelFn.
	consultCancel context.CancelFunc

	entries   []entry         // the transcript, as raw entries; see entry.go
	streaming strings.Builder // current assistant text
	lastReply string          // last assistant answer, plain text
	lastTool  string          // last tool output, full

	// room is the session's chat room (chat.go): every attached terminal's
	// view of it, mirrored into the session file beside the transcript.
	room []store.ChatLine

	running    bool // a run is in progress (a view's mode may be a popup)
	statusNote string
	// reasoningChars is the reasoning received for the reply in progress
	// (reset at OnModelStart); reasoningShown is the count last put on the
	// status line. statusHook sees every status for tests.
	reasoningChars, reasoningShown int
	statusHook                     func(string)
	usage                          usageMsg // cached usage for the wheel and menu (see usageMsg)
	// toast is the transient notice shown in yellow on the last transcript
	// row until toastUntil; now is swappable for tests.
	toast      string
	toastUntil time.Time
	now        func() time.Time

	// review decides where a file change is reviewed (editor diff, a
	// terminal, or both with the first answer winning). Set by cmd after
	// NewSession; nil in tests that do not exercise it.
	review   *review.Coordinator
	custom   map[string]commands.Command
	histFile *inputHistory

	clients []live.ClientInfo
	served  bool
	// quitting latches the end of the session: Quit sets it, broadcasts a
	// quitMsg every view answers with tea.Quit, and calls onQuit — the
	// runner's hook, which is what lets RunServed return even when there is
	// no program left to exit (an idle quit with nobody attached).
	quitting bool
	onQuit   func()
	// quitCh is closed once, by QuitLocked, and is how anything parked off
	// the update loop learns that the session has ended. Ask selects on it:
	// an approval left open when the last terminal goes away would otherwise
	// keep the agent goroutine blocked for ever.
	quitCh chan struct{}
	// holders is the set of terminals with the queue popup open. The agent's
	// delivery hold is refcounted over it (see holdQueueLocked): two
	// terminals may have the popup open at once, and a terminal that goes
	// away with it open must not leave delivery held for good.
	holders map[int]bool
	// joined is the set of terminals (by client id) that have posted their
	// own "X joined" line to the room. SetClients uses it to know which
	// departing terminals owe the room a "left" line — a terminal that never
	// opened /chat has nothing to say goodbye from.
	joined map[int]bool
	// ids is who each attached terminal is (identity.go): filled by
	// resolveClientLocked as terminals attach, keyed by client id. usersPath
	// is ~/.be-code/users.json ("" in a test session that wants no file on
	// disk at all — resolution then happens in memory only, through
	// resolveFn/bindFn). resolveFn/bindFn are test seams for inbox.Resolve/
	// inbox.Bind; nil means the real thing.
	ids       map[int]identity
	usersPath string
	resolveFn func(inbox.Terminal) (inbox.Resolution, error)
	bindFn    func(id string, tm inbox.Terminal) error
	// inboxDir is ~/.be-code/inbox (dm.go): "" disables /inbox and /dm outright
	// (a test session that wants no mailbox on disk at all — newTestSession
	// clears it the same way it clears usersPath, and a DM test sets its own
	// temp dir). onlineFn is the test seam for online (nil means the real
	// live-registry scan).
	inboxDir string
	onlineFn func(id string) bool
	// switchPending is set between asking the host to switch a terminal and
	// the roster that shows whether anyone is left (see SetClients).
	switchPending bool
	idleSince     time.Time // last moment the session had a client or a run
	// Joining a live session instead of forking it (see resumeFrom):
	// liveCodes reports which session codes have a host running somewhere,
	// loadSession reads a saved session, and switchClient (served only,
	// host.Switch) hands one terminal over to another session's host. All
	// are fields so tests can stand in for the filesystem and host.
	liveCodes   func() map[string]bool
	liveRecords func() []live.Record // the advertised hosts, for sessions the store has no file for
	loadSession func(id string) (*store.Session, error)
	// switchClient, detachClient and dropKeyClient are the host's own entry
	// points, nil in-process. None of them may be called from inside Update:
	// the host notifies its callbacks, which reach a program through p.Send —
	// the very channel that program's Update goroutine receives from (see the
	// /detach command, and live.Host.recompute).
	switchClient  func(id int, code string)
	detachClient  func(id int)
	dropKeyClient func(id int)

	// ideAnnounced keeps the editor-bridge line to one appearance;
	// initHinted keeps the "no BECODE.md" nudge to one per session, and
	// initChecked latches the *check* too, since a WindowSizeMsg arrives on
	// every resize and NeedsInitHint stats the workspace each time.
	ideAnnounced, initChecked, initHinted bool

	// ask is the question every attached terminal is being shown right now
	// — a tool approval, a plan, a shared picker — and askGen numbers the
	// asks so an answer or a withdrawal can be matched to the one it means
	// (see ask.go). Both are guarded by mu like everything else here.
	ask *ask
	// askFree is closed when the open question resolves; a second question
	// queues on it rather than being refused as a denial.
	askFree chan struct{}
	askGen  int

	viewsMu sync.Mutex
	views   map[int]*View // each View carries its mailbox (v.mb)

	// startTurnHook is a test seam consulted at the top of startTurnLocked:
	// when set it *replaces* the agent run, so a test can exercise
	// everything around a turn without a model goroutine mutating the
	// session under its assertions. nil in production, set before anything
	// else runs.
	startTurnHook func(string)
}

// NewSession builds the shared core and wires the agent's callbacks to it.
// Every callback runs on the agent goroutine and does nothing but broadcast,
// so none of them can block the loop on a terminal.
func NewSession(cfg *config.Config, ag *agent.Agent, prov provider.Provider) *Session {
	s := &Session{
		cfg: cfg, ag: ag, prov: prov,
		now:      time.Now,
		histFile: loadInputHistory(ui.HistoryFile()),
		custom:   commands.Load(ag.Tools.Root),

		liveCodes:   liveSessionCodes,
		liveRecords: liveSessionRecords,
		loadSession: store.Load,

		views:   map[int]*View{},
		quitCh:  make(chan struct{}),
		holders: map[int]bool{},
		joined:  map[int]bool{},
		ids:     map[int]identity{},
	}
	if p, err := inbox.UsersPath(); err == nil {
		s.usersPath = p
	}
	if d, err := inbox.Dir(); err == nil {
		s.inboxDir = d
	}
	wireEvents(s)
	s.usage = s.usageSnapshot() // pre-run, single-threaded: safe
	if ag.Session != nil {
		// The room comes back whether or not the transcript does — a session
		// resumed the day after a long chat should not lose it — and ahead of
		// seedResumeLocked, whose divider marks where the transcript picks up.
		s.restoreRoom(ag.Session.Chat)
		if len(ag.Session.Messages) > 0 {
			s.seedResumeLocked(ag.Session) // no views yet: single-threaded
		}
	}
	return s
}

// seedResumeLocked puts a resumed session's saved transcript on screen —
// what the person saw before they left, then a divider, then the resume
// line — so continuity is visible, not just restored in the model's
// history. The caller holds mu (or, in NewSession, is alone).
func (s *Session) seedResumeLocked(sess *store.Session) {
	if s.cfg.ResumeReplay {
		for _, l := range ui.Replay(sess.Messages, s.cfg.ResumeReplayTurns) {
			switch l.Kind {
			case ui.ReplayUser:
				s.appendEntryLocked(entry{Kind: entryUser, Label: "you> ", Text: l.Text})
			case ui.ReplayAssistant:
				s.appendEntryLocked(entry{Kind: entryAssistant, Text: l.Text})
			case ui.ReplayTool:
				s.appendEntryLocked(entry{Kind: entryTool, Label: l.Label, Text: l.Text})
			case ui.ReplayToolOK:
				s.appendEntryLocked(entry{Kind: entryToolOK, Text: l.Text})
			case ui.ReplayToolErr:
				s.appendEntryLocked(entry{Kind: entryToolErr, Text: l.Text})
			case ui.ReplaySummary:
				s.appendEntryLocked(entry{Kind: entryDim, Text: "earlier turns compacted:\n" + l.Text})
			}
		}
		s.appendEntryLocked(entry{Kind: entryDim, Text: ui.ReplayDivider})
	}
	s.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("resumed %s — %s (%d messages)", sess.ResumeCode(), sess.Title, len(sess.Messages))})
	if sess.Handoff != "" {
		s.appendEntryLocked(entry{Kind: entryDim, Text: "handoff briefing loaded into the system prompt; /handoff shows it"})
	}
}

// wireEvents points s.ag's callbacks and its registry's approval seam at the
// session. It is separate from NewSession because the agent under a session
// can be replaced — a test standing in its own, anything that rebuilds the
// agent for a new provider — and a replacement with no wiring is an agent
// whose tool calls and consultations reach no terminal at all.
//
// Every callback records what happened on the session and broadcasts; none
// of them touches a view, so N terminals never put N copies of one tool call
// on the transcript.
func wireEvents(s *Session) {
	ag := s.ag
	ag.Tools.Approve = s.approveFromAgent
	ag.Tools.ApproveCtx = s.approveFromAgentCtx
	ag.Events = agent.Events{
		OnDelta:     s.onDelta,
		OnToolStart: s.onToolStart,
		OnToolEnd:   s.onToolEnd,
		OnNotice:    s.notice,
		OnTransient: s.transient,
		OnReasoning: func(t string) {
			// This reply's reasoning only: the count starts over at
			// OnModelStart, or a session that had thought for 40k chars
			// showed 40k on a reply that had thought for 4k.
			s.reasoningChars += len(t)
			if s.reasoningChars-s.reasoningShown >= 200 { // throttle status updates
				s.reasoningShown = s.reasoningChars
				s.setStatus(fmt.Sprintf("thinking (%dk chars of reasoning)", s.reasoningChars/1000))
			}
		},
		OnModelStart:      func() { s.reasoningChars, s.reasoningShown = 0, 0 },
		OnConsultStart:    s.onConsultStart,
		OnConsultProgress: s.onConsultProgress,
		OnConsultEnd:      s.onConsultEnd,
	}
	ag.Tools.OnStatus = s.setStatus
}

// themeFor resolves the theme for a client label: a remembered per-device
// theme (client_themes, keyed by the label with its pid stripped) first,
// else the config default, else the built-in default. A name that is not a
// known theme at either step is skipped rather than returned, so a stale or
// hand-edited config never leaves a view with no styles at all.
func (s *Session) themeFor(label string) (name, origin string) {
	key := live.LabelKey(label)
	if remembered, ok := s.cfg.ClientThemes[key]; ok {
		if _, ok := lookupTheme(remembered); ok {
			return remembered, fmt.Sprintf("remembered for %q", key)
		}
	}
	if _, ok := lookupTheme(s.cfg.Theme); ok {
		return s.cfg.Theme, "config default"
	}
	return "dark", "built-in default"
}

// themeWarnFor reports the "theme X is not known; using Y" note this label
// should see once, or "" when nothing at either step (remembered, config
// default) needed to fall through to resolved.
func (s *Session) themeWarnFor(label, resolved string) string {
	key := live.LabelKey(label)
	if remembered, ok := s.cfg.ClientThemes[key]; ok {
		if _, ok := lookupTheme(remembered); !ok {
			return fmt.Sprintf("theme %q is not known; using %s", remembered, resolved)
		}
		return ""
	}
	if s.cfg.Theme != "" {
		if _, ok := lookupTheme(s.cfg.Theme); !ok {
			return fmt.Sprintf("theme %q is not known; using %s", s.cfg.Theme, resolved)
		}
	}
	return ""
}

// NewView opens one terminal's view of this session. id is the live client
// id the view renders for (0 is the local terminal), label how that terminal
// names itself.
func (s *Session) NewView(id int, label string) *View {
	s.mu.Lock()
	name, origin := s.themeFor(label)
	warn := s.themeWarnFor(label, name)
	st := stylesOr(name)
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = st.Accent
	v := &View{Session: s, id: id, label: label, st: st, richText: name != "mono", spin: sp,
		theme: name, themeOrigin: origin, themeWarn: warn,
		clipboardWrite: writeClipboard, clipboardRead: readClipboard, termWrite: writeTerminal,
		mb: newMailbox()}
	v.input = v.newInputArea() // this terminal's one input line; sized by the first layout()
	// A terminal that attaches mid-session starts with the room as it already
	// is, the same reason v.streaming is seeded below: a chatMsg broadcast
	// only carries what changes from here.
	v.room = append([]store.ChatLine(nil), s.room...)
	// A terminal that already knows who it is starts with its DM unread
	// count populated too, so the bottom-line badge is accurate from the
	// first frame it draws rather than only after /inbox has been opened
	// once. Production always resolves identity in SetClients before this
	// runs (see runner.onClients); a test that sets s.ids directly gets the
	// same treatment.
	if s.inboxDir != "" && s.ids[id].ID != "" {
		v.reloadThreads()
	}
	// A terminal that attaches in the middle of a reply starts from what has
	// streamed so far, not from the next delta — and is attached before mu
	// is released, or a delta broadcast in that gap would reach neither the
	// seed nor the view.
	v.streaming = s.streaming.String()
	// A terminal that attaches in the middle of a run starts busy, exactly
	// where runStateMsg{running:true} would have left it. mode's zero value
	// is modeInput, and a view that started there would take Enter to
	// Submit — a *second* concurrent RunFull on the one agent, clobbering
	// cancelFn — instead of queueing, and would not cancel on Esc.
	if s.running {
		v.mode = modeBusy
		v.input.Placeholder = busyPlaceholder
	}
	// And a terminal that attaches while a question is on every other screen
	// is shown it too. Otherwise a detach and reattach during a file-write
	// approval leaves the agent goroutine parked in Ask with nothing left
	// that can answer it. There is no size yet, so the modal viewport built
	// here is a placeholder: the first WindowSizeMsg builds it for real (see
	// View.update).
	if s.ask != nil {
		v.showAsk(s.ask)
	}
	s.attachView(v)
	s.mu.Unlock()
	return v
}

// sendTo hands msg to one view's mailbox, if that view is still attached.
// Like broadcast it takes only viewsMu and never blocks, so it is safe from
// the agent goroutine and from inside Update alike — and going through the
// roster is what keeps it off a mailbox that retireView has already closed.
func (s *Session) sendTo(id int, msg tea.Msg) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	if v := s.views[id]; v != nil {
		v.mb.send(msg)
	}
}

// holdQueueLocked records that one terminal has (or no longer has) its queue
// popup open, and holds the agent's delivery while any of them does. The
// caller holds mu.
//
// A refcount rather than a flag: two terminals may have the popup open at
// once, and either one's close would otherwise resume delivery under the
// other's cursor.
func (s *Session) holdQueueLocked(id int, on bool) {
	if s.holders == nil {
		s.holders = map[int]bool{}
	}
	if on {
		s.holders[id] = true
	} else {
		delete(s.holders, id)
	}
	s.ag.Hold(len(s.holders) > 0)
}

// attachView registers a view so broadcasts reach its mailbox.
func (s *Session) attachView(v *View) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	if s.views == nil {
		s.views = map[int]*View{}
	}
	s.views[v.id] = v
}

// detachView stops broadcasting to a view. It must run before that view's
// mailbox is closed, or a broadcast racing the close would send on a closed
// channel.
func (s *Session) detachView(id int) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	delete(s.views, id)
}

func (s *Session) viewByID(id int) *View {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	return s.views[id]
}

// broadcast hands msg to every attached view. It takes only viewsMu — never
// mu — and every hand-off is non-blocking, so it is safe to call from the
// agent goroutine and from inside Update alike.
func (s *Session) broadcast(msg tea.Msg) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()
	for _, v := range s.views {
		v.mb.send(msg)
	}
}

// send is broadcast under the name the agent-side goroutines use.
func (s *Session) send(msg tea.Msg) { s.broadcast(msg) }

// appendEntry records one transcript entry and tells every view to render
// it. This is the variant for callers that do not hold mu (tests, and
// anything on an agent goroutine); code reached from Update calls
// appendEntryLocked instead.
func (s *Session) appendEntry(e entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEntryLocked(e)
}

// appendEntryLocked is appendEntry for a caller that already holds mu.
// The entry lands on the shared transcript here; each view renders it with
// its own styles and width when its mailbox delivers the entryMsg.
func (s *Session) appendEntryLocked(e entry) {
	s.entries = append(s.entries, e)
	s.broadcast(entryMsg{n: len(s.entries) - 1, e: e})
}

// Entries is a snapshot of the transcript, taken under the lock.
func (s *Session) Entries() []entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]entry(nil), s.entries...)
}

// usageSnapshot must be called only where the agent loop is quiescent:
// before the program starts, inside agent event callbacks, or after a run
// returns on its goroutine.
func (s *Session) usageSnapshot() usageMsg {
	usage := s.ag.Usage()
	return usageMsg{
		ctxTokens: s.ag.History.Tokens(),
		budget:    s.ag.History.Limit(),
		total:     usage.PromptTokens + usage.CompletionTokens,
	}
}

// clientLabel names a client for transcript prefixes and the bottom line.
// It reads the roster without locking: every caller is either inside Update
// (which holds mu for its whole body) or a single-goroutine test.
func (s *Session) clientLabel(client int) string {
	for _, c := range s.clients {
		if c.ID == client {
			return c.Label
		}
	}
	return "you"
}

// userPrefix is the transcript prefix for a user line: "you> " with a
// single terminal, "<label>> " for every sender once several are attached.
func (s *Session) userPrefix(client int) string {
	if len(s.clients) > 1 {
		return s.clientLabel(client) + "> "
	}
	return "you> "
}

// clientLabels joins the attached terminals' labels for the bottom line,
// truncated with an ellipsis to fit room cells. With no usable room it
// returns "": an untruncated list would overrun the row and wrap the
// status line on every attached terminal.
func (s *Session) clientLabels(room int) string {
	if room <= 1 {
		return ""
	}
	labels := make([]string, 0, len(s.clients))
	for _, c := range s.clients {
		labels = append(labels, c.Label)
	}
	joined := strings.Join(labels, ", ")
	if r := []rune(joined); len(r) > room {
		joined = string(r[:room-1]) + "…"
	}
	return joined
}

// SetClients records a new attached-terminal roster. It is the host's
// OnClients callback, so it runs on the host's goroutine, not the program's:
// everything shared happens here under mu — the attach/detach transcript
// lines, the pending-switch flag, and retiring a departed terminal's history
// cursor and escape-sequence parser — and the roster then goes out as a
// clientsMsg, which is each view's cue to redraw its bottom line (the
// programs themselves are the runner's: see runner.onClients).
func (s *Session) SetClients(infos []live.ClientInfo) {
	// Identity first, off the lock: resolving a new terminal can read a file
	// and, on Windows and macOS, run `arp -a` with a 3 s timeout. Under mu
	// that would hold every terminal's Update and the agent's own events for
	// the duration.
	resolved := s.resolveNew(infos)
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.clients
	s.clients = infos
	if len(infos) > 0 {
		// Someone is still watching, so the switch that set this flag did
		// not empty the session. Clearing it here, not only on an attach,
		// keeps a later ordinary detach from quitting a host whose client
		// was told it is still running.
		s.switchPending = false
	}
	for _, c := range infos {
		if !hasClient(prev, c.ID) {
			s.appendEntryLocked(entry{Kind: entryDim, Text: "attached: " + c.Label})
		}
		if r, ok := resolved[c.ID]; ok {
			s.recordIdentityLocked(c.ID, r)
		}
	}
	for _, c := range prev {
		if hasClient(infos, c.ID) {
			continue
		}
		s.appendEntryLocked(entry{Kind: entryDim, Text: "detached: " + c.Label})
		// A terminal that has gone leaves no place in the shared input
		// history and no half-typed escape sequence behind it.
		s.histFile.drop(c.ID)
		if s.dropKeyClient != nil {
			s.dropKeyClient(c.ID)
		}
		// Only a terminal that actually opened the room owes it a goodbye —
		// one that never typed /chat never said hello either.
		if s.joined[c.ID] {
			s.PostLocked("", s.chatNameOf(c)+" left", "leave")
			delete(s.joined, c.ID)
		}
	}
	s.broadcast(clientsMsg(infos))
}

// ---- turns -------------------------------------------------------------------

// busyPlaceholder is what every input line advertises while the agent works.
const busyPlaceholder = "type to queue a message for the agent…  (Enter queues · Esc cancels)"

// flushLocked turns whatever the model has streamed so far into a
// transcript entry. The caller holds mu.
//
// Order matters: streamEndMsg goes out *before* the entry, so that a view
// clears its own copy of the streaming text and then renders the finished
// entry — the other way round would paint the reply twice for as long as it
// took the second message to arrive.
func (s *Session) flushLocked() {
	if s.streaming.Len() == 0 {
		return
	}
	text := strings.TrimRight(s.streaming.String(), "\n")
	s.lastReply = text
	s.streaming.Reset()
	s.broadcast(streamEndMsg{})
	s.appendEntryLocked(entry{Kind: entryAssistant, Text: text})
}

// ---- agent events ------------------------------------------------------------
//
// Everything below runs on the agent goroutine. Each one records what
// happened on the shared session and broadcasts; none of them touches a
// view, because with one program per terminal a per-view append would put
// one tool call on the transcript once per attached terminal.

// onDelta is Events.OnDelta: the streamed text accrues on the session (so a
// terminal attaching mid-reply can be seeded with it, see NewView) and the
// fragment goes out for every view to append to its own copy.
func (s *Session) onDelta(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streaming.WriteString(t)
	s.broadcast(deltaMsg(t))
}

func (s *Session) onToolStart(name, args string) {
	s.mu.Lock()
	s.flushLocked()
	s.appendEntryLocked(entry{Kind: entryTool, Label: name, Text: args})
	s.statusNote = "running " + name
	s.broadcast(statusMsg(s.statusNote))
	s.mu.Unlock()
	s.broadcast(s.usageSnapshot()) // the loop is quiescent inside a callback
}

func (s *Session) onToolEnd(name string, res tools.Result) {
	s.mu.Lock()
	s.flushLocked()
	s.lastTool = res.Content
	first := strings.SplitN(res.Content, "\n", 2)[0]
	if res.IsError {
		s.appendEntryLocked(entry{Kind: entryToolErr, Text: first})
	} else {
		if len(first) > 100 {
			first = first[:100] + "…"
		}
		s.appendEntryLocked(entry{Kind: entryToolOK, Text: first})
	}
	s.statusNote = "thinking"
	s.broadcast(statusMsg(s.statusNote))
	s.mu.Unlock()
	s.broadcast(s.usageSnapshot())
}

// ---- co-working ---------------------------------------------------------
//
// A consultation is a second voice in the one transcript every terminal
// shares: the question goes up before the wait it explains, the files the
// co-worker reads are a status line rather than entries, and the answer
// closes it with what it cost. Like every other event these record on the
// session and broadcast — a per-view append would print the co-worker once
// per attached terminal.

// onConsultStart is Events.OnConsultStart: the question, under the
// co-worker's name, and a status line naming who is being waited on.
func (s *Session) onConsultStart(name, question, origin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	s.appendEntryLocked(entry{Kind: entryCoworkAsk, Label: name, Text: question})
	s.statusNote = "consulting " + name
	s.broadcast(statusMsg(s.statusNote))
}

// onConsultProgress is Events.OnConsultProgress: how far the co-worker has
// got. It is a status line and never an entry — a consultation that reads
// twenty files would otherwise bury the question it is answering.
func (s *Session) onConsultProgress(name string, read int) {
	s.setStatus(fmt.Sprintf("consulting %s · %d files read", name, read))
}

// onConsultEnd is Events.OnConsultEnd: the answer, or a dimmed note saying
// why there is none. A declined or capped consultation is an ordinary
// outcome the harness carries on from, so it is dim rather than an error.
func (s *Session) onConsultEnd(res agent.ConsultResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	if err != nil {
		s.appendEntryLocked(entry{Kind: entryDim, Text: res.Coworker + ": " + err.Error()})
	} else {
		s.appendEntryLocked(entry{Kind: entryCowork, Label: res.Coworker, Text: res.Answer})
		if res.Partial {
			s.appendEntryLocked(entry{Kind: entryDim, Text: "(partial)"})
		}
		s.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("%s read %d files in %s",
			res.Coworker, res.Read, res.Elapsed.Round(time.Second))})
	}
	s.statusNote = "thinking"
	s.broadcast(statusMsg(s.statusNote))
}

// notice puts one note on the shared transcript. It is Events.OnNotice and
// the way a command's own goroutine reports its outcome; a noticeMsg that
// reaches a view instead is that terminal's own (its backend ping), and is
// rendered locally.
func (s *Session) notice(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	s.appendEntryLocked(noticeEntry(text))
}

// transient records a short-lived notice. It is not a transcript entry: it
// lives on the session until toastUntil, and each view schedules its own
// expiry tick when the broadcast reaches it.
func (s *Session) transient(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toastLocked(text)
}

// toastLocked is transient for a caller that already holds mu (deliverDM,
// raised from the inbox watcher's own goroutine while mu is held for the
// whole callback).
func (s *Session) toastLocked(text string) {
	s.toast, s.toastUntil = text, s.now().Add(toastFor)
	s.broadcast(transientMsg(text))
}

// setStatus changes the bottom-line note without adding a transcript line:
// hidden-reasoning progress, and the editor's own review progress
// (Registry.OnStatus).
func (s *Session) setStatus(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if note == "" && s.running {
		note = "thinking"
	}
	s.statusNote = note
	if s.statusHook != nil {
		s.statusHook(note)
	}
	s.broadcast(statusMsg(note))
}

// setRunStateLocked records that the session started or finished working and
// tells every view, which is what moves each terminal between its input and
// busy modes. The caller holds mu.
func (s *Session) setRunStateLocked(running bool, note string) {
	s.running = running
	s.statusNote = note
	s.broadcast(runStateMsg{running: running, note: note})
}

// setRunState is setRunStateLocked for a caller that does not hold mu (the
// plan goroutine, once the plan has been approved).
func (s *Session) setRunState(running bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRunStateLocked(running, note)
}

// runContextLocked makes the cancellable context a turn runs under and parks
// its cancel in cancelFn, where Esc and /quit find it. The caller holds mu.
func (s *Session) runContextLocked() context.Context {
	root := s.rootCtx
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	s.cancelFn = cancel
	return ctx
}

// Submit is a request typed at a terminal: it echoes the text under that
// terminal's label and starts the turn.
//
// The caller holds mu — it is a view's Update, the only thing that can have
// a keystroke to submit. (mu is not reentrant, so there is no unlocked
// variant to call by mistake.)
func (s *Session) Submit(text string, from int) {
	s.appendEntryLocked(entry{Kind: entryUser, Label: s.userPrefix(from), Text: text})
	s.startTurnLocked(text)
}

// Quit ends the session for every attached terminal at once: a run in
// flight is cancelled, and each view answers the broadcast quitMsg with
// tea.Quit. It is called by the host's quit hook, by the runner's idle
// rules, and by /quit (through QuitLocked).
func (s *Session) Quit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.QuitLocked()
}

// QuitLocked is Quit for a caller that already holds mu (a view's Update).
func (s *Session) QuitLocked() {
	if !s.quitting {
		if s.cancelFn != nil {
			s.cancelFn() // leaving mid-turn: stop the run, then write the briefing
		}
		if s.consultCancel != nil {
			s.consultCancel()
		}
		s.quitting = true
		if s.quitCh == nil {
			s.quitCh = make(chan struct{})
		}
		// Anything parked off the update loop — an Ask nobody is left to
		// answer — learns the session is over from this, once.
		close(s.quitCh)
		s.broadcast(quitMsg{})
	}
	if s.onQuit != nil {
		// On a goroutine of its own: the hook reads session state under mu,
		// which this caller is holding, and it asks each program to stop,
		// which blocks until that program's update loop takes the message —
		// and this may be running *on* that loop.
		go s.onQuit()
	}
}

// isQuitting reports whether Quit has run.
func (s *Session) isQuitting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quitting
}

// emptyAfterSwitch reports the one case where a host with nobody attached
// should end itself rather than wait to be attached to again: a terminal
// switched to another session, leaving behind a session nobody ever typed
// into. A session with turns in it is worth coming back to with `be-code
// attach`, and so is one with a run in flight whose first turn has not
// reached the session file yet.
func (s *Session) emptyAfterSwitch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchPending && !s.running &&
		s.ag.Session != nil && len(s.ag.Session.Messages) == 0
}

// idleExpired reports whether a served session has sat with no terminal
// attached and no run in progress for live_idle_limit minutes. A client or
// a run resets the clock instead. The runner's idleLoop polls this.
func (s *Session) idleExpired(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.served || s.cfg.LiveIdleLimit <= 0 {
		return false
	}
	if len(s.clients) > 0 || s.running {
		s.idleSince = now
		return false
	}
	return now.Sub(s.idleSince) >= time.Duration(s.cfg.LiveIdleLimit)*time.Minute
}

// startTurnLocked launches the agent on a goroutine. The caller holds mu and
// has already echoed the request into the transcript.
func (s *Session) startTurnLocked(text string) {
	s.setRunStateLocked(true, "thinking")
	if s.startTurnHook != nil {
		// A test is standing in for the run: record it and start nothing,
		// so no model goroutine mutates the session under its assertions.
		s.startTurnHook(text)
		return
	}
	ctx := s.runContextLocked()
	go func() {
		_, rep, err := s.ag.RunFull(ctx, text)
		s.send(s.usageSnapshot()) // run finished; agent quiescent
		s.finishTurn(rep, err)
	}()
}

// finishTurn ends a run: it is called directly by the goroutine that ran it
// (never from inside Update), records everything the turn produced on the
// shared transcript, and returns the session to idle.
func (s *Session) finishTurn(rep *agent.ReviewedReport, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishTurnLocked(rep, err)
}

// finishTurnLocked is finishTurn for a caller that already holds mu.
func (s *Session) finishTurnLocked(rep *agent.ReviewedReport, err error) {
	s.flushLocked()
	if err != nil {
		if err != context.Canceled && !strings.Contains(err.Error(), "context canceled") {
			s.appendEntryLocked(entry{Kind: entryError, Label: "error ", Text: err.Error()})
		} else {
			s.appendEntryLocked(entry{Kind: entryWarn, Text: "cancelled"})
		}
	}
	if rep != nil && rep.Verify != nil {
		for _, line := range strings.Split(rep.Verify.Human(), "\n") {
			s.appendEntryLocked(entry{Kind: entryDim, Text: line})
		}
		if rep.Verify.Passed() {
			s.appendEntryLocked(entry{Kind: entryOK, Text: "✓ verified"})
		} else {
			s.appendEntryLocked(entry{Kind: entryErr, Text: "✗ verification failed after repairs"})
		}
	}
	if rep != nil && rep.Reviewed {
		if rep.ReviewIssues == "" {
			s.appendEntryLocked(entry{Kind: entryOK, Text: "✓ reviewer approved"})
		} else {
			s.appendEntryLocked(entry{Kind: entryWarn, Text: "reviewer raised issues (repair attempted)"})
		}
	}
	s.appendEntryLocked(entry{Kind: entryPlain})
	s.setRunStateLocked(false, "")
	// Anything queued during the run that the model never got to see becomes
	// the next turn — as one request, but echoed line by line under the
	// terminal each message came from.
	if left := s.ag.DrainItems(); len(left) > 0 {
		texts := make([]string, 0, len(left))
		for _, it := range left {
			s.appendEntryLocked(entry{Kind: entryUser, Label: s.userPrefix(it.From), Text: it.Text})
			texts = append(texts, it.Text)
		}
		s.startTurnLocked(strings.Join(texts, "\n"))
	}
}

// finishInit ends the /init flow on its own goroutine, the way finishTurn
// ends a run: the outcome goes on the shared transcript, the init context is
// released so a stale cancel cannot reach the next turn's Esc, and the
// session returns to idle.
func (s *Session) finishInit(path string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	if s.cancelFn != nil {
		s.cancelFn()
		s.cancelFn = nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
			s.appendEntryLocked(entry{Kind: entryWarn, Text: "cancelled"})
		} else {
			s.appendEntryLocked(entry{Kind: entryError, Label: "init failed: ", Text: err.Error()})
		}
	} else {
		s.appendEntryLocked(entry{Kind: entryOK, Text: "wrote " + path})
	}
	s.finishTurnLocked(nil, nil)
}
