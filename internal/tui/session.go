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

	entries   []entry         // the transcript, as raw entries; see entry.go
	streaming strings.Builder // current assistant text
	lastReply string          // last assistant answer, plain text
	lastTool  string          // last tool output, full

	running    bool // a run is in progress (a view's mode may be a popup)
	statusNote string
	usage      usageMsg // cached usage for the wheel and menu (see usageMsg)
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
	host    *live.Host
	// quitting latches the end of the session: Quit sets it, broadcasts a
	// quitMsg every view answers with tea.Quit, and calls onQuit — the
	// runner's hook, which is what lets RunServed return even when there is
	// no program left to exit (an idle quit with nobody attached).
	quitting bool
	onQuit   func()
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
	ask    *ask
	askGen int

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

		views: map[int]*View{},
	}
	ag.Tools.Approve = s.approveFromAgent
	// Every callback records what happened on the session and broadcasts;
	// none of them touches a view, so N terminals never put N copies of one
	// tool call on the transcript.
	ag.Events = agent.Events{
		OnDelta:     s.onDelta,
		OnToolStart: s.onToolStart,
		OnToolEnd:   s.onToolEnd,
		OnNotice:    s.notice,
		OnTransient: s.transient,
		OnReasoning: func() func(string) {
			n, last := 0, 0
			return func(t string) {
				n += len(t)
				if n-last >= 200 { // throttle status updates
					last = n
					s.setStatus(fmt.Sprintf("thinking (%dk chars of reasoning)", n/1000))
				}
			}
		}(),
	}
	ag.Tools.OnStatus = s.setStatus
	s.usage = s.usageSnapshot() // pre-run, single-threaded: safe
	return s
}

// NewView opens one terminal's view of this session. id is the live client
// id the view renders for (0 is the local terminal), label how that terminal
// names itself.
func (s *Session) NewView(id int, label string) *View {
	s.mu.Lock()
	st := stylesOr(s.cfg.Theme)
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = st.Accent
	v := &View{Session: s, id: id, label: label, st: st, richText: s.cfg.Theme != "mono", spin: sp,
		clipboardWrite: writeClipboard, clipboardRead: readClipboard, termWrite: writeTerminal,
		mb: newMailbox()}
	v.input = v.newInputArea() // this terminal's one input line; sized by the first layout()
	// A terminal that attaches in the middle of a reply starts from what has
	// streamed so far, not from the next delta — and is attached before mu
	// is released, or a delta broadcast in that gap would reach neither the
	// seed nor the view.
	v.streaming = s.streaming.String()
	s.attachView(v)
	s.mu.Unlock()
	return v
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
	return usageMsg{
		ctxTokens: s.ag.History.Tokens(),
		budget:    s.ag.History.Limit(),
		total:     s.ag.Stats.PromptTokens + s.ag.Stats.CompletionTokens,
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
// clientsMsg for each view to do its own local cleanup (see
// View.updateClients).
func (s *Session) SetClients(infos []live.ClientInfo) {
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
		s.quitting = true
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
