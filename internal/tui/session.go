package tui

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
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
	// switchClient, detachClient, dropKeyClient and setOverlay are the host's
	// own entry points, nil in-process. None of them may be called from
	// inside Update: the host notifies its callbacks, which reach the program
	// through p.Send — the very channel the Update goroutine receives from
	// (see the /detach command, and live.Host.recompute).
	switchClient  func(id int, code string)
	detachClient  func(id int)
	dropKeyClient func(id int)
	setOverlay    func(id int, s string)

	// ideAnnounced keeps the editor-bridge line to one appearance;
	// initHinted keeps the "no BECODE.md" nudge to one per session, and
	// initChecked latches the *check* too, since a WindowSizeMsg arrives on
	// every resize and NeedsInitHint stats the workspace each time.
	ideAnnounced, initChecked, initHinted bool

	// askGen numbers shared-review prompts (approvalMsg.gen). Written from
	// the agent goroutine (reviewTerminal.Ask) and read by Withdraw on the
	// same goroutine, so it is atomic rather than guarded by mu.
	askGen atomic.Int64

	viewsMu sync.Mutex
	views   map[int]*View // each View carries its mailbox (v.mb)

	// startTurnHook is a test seam consulted at the top of startTurn; nil in
	// production. sendHook is a test seam consulted by broadcast: with no
	// tea.Program running there is nothing to deliver a message from another
	// goroutine to, so a test routes them into a channel it drains into
	// Update itself (see review_integration_test.go). Both nil in production,
	// and both set before anything else runs.
	startTurnHook func(string)
	sendHook      func(tea.Msg)
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
	ag.Events = agent.Events{
		OnDelta: func(t string) { s.send(deltaMsg(t)) },
		OnToolStart: func(n, a string) {
			s.send(toolStartMsg{n, a})
			s.send(s.usageSnapshot())
		},
		OnToolEnd: func(n string, r tools.Result) {
			s.send(toolEndMsg{n, r})
			s.send(s.usageSnapshot())
		},
		OnNotice:    func(t string) { s.send(noticeMsg(t)) },
		OnTransient: func(t string) { s.send(transientMsg(t)) },
		OnReasoning: func() func(string) {
			n, last := 0, 0
			return func(t string) {
				n += len(t)
				if n-last >= 200 { // throttle status updates
					last = n
					s.send(thinkingMsg(n))
				}
			}
		}(),
	}
	ag.Tools.OnStatus = func(t string) { s.send(statusMsg(t)) }
	s.usage = s.usageSnapshot() // pre-run, single-threaded: safe
	return s
}

// NewView opens one terminal's view of this session. id is the live client
// id the view renders for (0 is the local terminal), label how that terminal
// names itself.
func (s *Session) NewView(id int, label string) *View {
	st := stylesOr(s.cfg.Theme)
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = st.Accent
	v := &View{Session: s, id: id, label: label, st: st, richText: s.cfg.Theme != "mono", spin: sp,
		clipboardWrite: writeClipboard, clipboardRead: readClipboard, termWrite: writeTerminal,
		mb: newMailbox()}
	v.inputFor(0) // the local terminal's input line; sized by the first layout()
	s.attachView(v)
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
	if s.sendHook != nil {
		s.sendHook(msg)
		return
	}
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
	s.broadcast(entryMsg{e})
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

// approveFromAgent bridges the agent goroutine into the UI event loop.
func (s *Session) approveFromAgent(action, detail string) bool {
	if action == "shell" && s.cfg.AutoApproveShell {
		return true
	}
	if action == "file_write" && !s.cfg.ApproveFileWrites {
		return true
	}
	resp := make(chan bool, 1)
	s.send(approvalMsg{action: action, detail: detail, resp: resp})
	return <-resp
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

// SetReview hands the session the review coordinator built in cmd, so
// /review can report and change where file changes are reviewed.
func (s *Session) SetReview(c *review.Coordinator) { s.review = c }

// ReviewTerminal is this UI as the coordinator's terminal-side reviewer:
// the shared approval modal, which any attached terminal can answer.
func (s *Session) ReviewTerminal() review.Terminal { return reviewTerminal{s} }

// ---- running -----------------------------------------------------------------

// RunLocal runs the session in this process's own terminal (alt screen) and
// blocks until exit. One view, one program: the in-process TUI is client 0.
func (s *Session) RunLocal(ctx context.Context) error {
	s.rootCtx = ctx
	v := s.NewView(0, "local")
	if s.cfg.ThemeTerminalColors {
		v.termWrite(terminalColorSeq(s.cfg.Theme))
		defer v.termWrite(terminalColorReset())
	}
	p := tea.NewProgram(v, tea.WithAltScreen(), tea.WithMouseCellMotion())
	v.program = p
	defer s.retireView(v)
	go v.mb.run(p.Send)
	_, err := p.Run()
	s.histFile.save()
	return err
}

// retireView takes a view out of the broadcast set and closes its mailbox,
// in that order: once no broadcast can reach the mailbox, closing it is safe
// and the delivery goroutine ends.
func (s *Session) retireView(v *View) {
	s.detachView(v.id)
	v.mb.close()
}
