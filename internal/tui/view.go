// Package tui implements BE-Code's full-screen terminal UI on the
// Charmbracelet stack: streaming transcript, multi-line input, status bar,
// approval modals with diffs, and interactive pickers.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/commands"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/ui"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// ---- messages from the agent goroutine ------------------------------------

// entryMsg is one transcript entry the session has already recorded, on its
// way to a view that has yet to render it. The entry itself lives on
// Session.entries; this only says "render this, in your styles, at your
// width" (see Session.appendEntryLocked and View.renderEntryLocal).
//
// n is the entry's index in Session.entries, and it is what keeps the stream
// and rebuild() honest: a view renders the message only when n is at or past
// its own render cursor (View.renderedN). A rebuild that overtook this entry
// leaves n behind the cursor, so the message is dropped instead of painting
// the entry twice; an entry lost to a full mailbox leaves a gap, and the next
// message that arrives is still rendered in its own place rather than in the
// lost one's. A bare counter cannot do either, because it cannot tell which
// entry a queued message is for.
type entryMsg struct {
	n int
	e entry
}

// deltaMsg is one fragment of the model's reply. The text accrues on the
// session too (so a terminal attaching mid-reply is seeded with it), but
// each view keeps its own copy to render under its transcript — and clears
// it on streamEndMsg, which the session broadcasts just before the finished
// reply arrives as an entry.
type deltaMsg string
type streamEndMsg struct{}

// noticeMsg is a note meant for one terminal only — its own backend ping.
// Notes from the agent are transcript entries instead (Session.notice), or
// N terminals would put N copies of each on the shared transcript.
type noticeMsg string

// quitMsg ends the session on every attached terminal at once: each view
// answers it with tea.Quit. It is broadcast by Session.Quit, which is also
// what a client's quit frame, /quit and the idle limit go through.
type quitMsg struct{}

// transientMsg is a short-lived status notice (waiting for the backend,
// context budgeting): shown on the notice row for toastFor, never kept.
type transientMsg string

// toastTickMsg asks the view to expire the notice row if its time is up.
type toastTickMsg time.Time

const toastFor = 20 * time.Second

// statusMsg sets the bottom-line status note directly, without adding a
// transcript line (hidden-reasoning progress, editor-side review progress).
type statusMsg string

// runStateMsg tells every terminal that the session started or finished
// working. The run state itself lives on the Session (Session.running,
// Session.statusNote); this is each view's cue to move between its input and
// busy modes, swap the placeholder and close a queue popup. It is broadcast
// by setRunStateLocked, which is the only thing that changes that state.
type runStateMsg struct {
	running bool
	note    string
}

// usageMsg carries a usage snapshot taken ON THE AGENT GOROUTINE at a
// moment the agent is quiescent (inside an event callback, or after a run
// returns). The UI renders only these cached numbers, so the status bar
// never reads agent state concurrently with the loop mutating it.
type usageMsg struct {
	ctxTokens, budget, total int
}

// mode is the input routing state.
type mode int

const (
	modeInput mode = iota
	modeBusy
	// modeAsk is a shared question on screen — an approval, a plan, or a
	// shared picker. Every attached terminal is in it at once and any of
	// them may answer (see ask.go).
	modeAsk
	modePicker      // a view-local list overlay (the theme picker)
	modePalette     // "/" command popup above the input
	modeMenu        // full-screen grouped menu (/menu)
	modeContextMenu // right-click copy/paste popup
	modeQueue       // queued-messages popup (edit/drop while a run is in progress)
)

// View is one terminal's view of a Session: the bubbletea model a single
// program renders. It embeds the shared core, so every Session field and
// method is reachable through promotion (m.ag, m.clients, m.appendEntry…)
// while everything declared here is local to this terminal — its size, its
// styles, its viewport, its drafts and the popups it has open.
type View struct {
	*Session

	id    int    // the live client id this view renders for; 0 is the local terminal
	label string // how that terminal names itself

	st       styles // this terminal's theme; see theme.go
	richText bool   // markdown/syntax rendering enabled
	// theme/themeOrigin track the name and provenance of this view's current
	// theme — one of `remembered for "<key>"`, "config default" or "built-in
	// default" — for /theme's bare report and themeItems' "(current)" mark.
	// themeWarn is a pending "theme X is not known; using Y" note (set at
	// construction when the value that would apply is not a known theme),
	// printed once as a local note on this view's first WindowSizeMsg and
	// cleared immediately after.
	theme, themeOrigin, themeWarn string

	vp      viewport.Model
	modalVP viewport.Model
	spin    spinner.Model
	input   textarea.Model // this terminal's own draft

	mode     mode
	prevMode mode
	picker   *picker
	// shownAsk is the shared question this terminal is displaying and
	// askShown its generation. The ask itself belongs to the Session; the
	// copy here is what View() renders from, so a frame drawn between
	// another terminal's answer and this one's askResolvedMsg still has
	// something to draw (and its picker cursor is this terminal's own).
	shownAsk *ask
	askShown int
	// answeredGen is the generation this terminal answered itself. It is
	// what keeps "answered by …" off the screen of the person who just
	// pressed the key, in the case where one view renders for a whole
	// roster and the answering client is not this view's own id.
	answeredGen int

	width, height int
	ready         bool

	// streaming is this terminal's copy of the reply as it arrives. It
	// deliberately shadows Session.streaming (the strings.Builder the agent
	// goroutine writes): the session accrues the text so a terminal
	// attaching mid-reply can be seeded from it, and each view renders from
	// its own copy without reading a field another goroutine is writing.
	streaming string

	rendered strings.Builder // the session's entries rendered with this view's styles at its width
	// renderedN is the index of the next Session.entries entry this buffer
	// expects. It is what lets rebuild() and the entryMsg stream coexist: a
	// rebuild renders every entry recorded so far and moves the cursor past
	// them, so the entryMsgs still queued for those entries are dropped on
	// arrival rather than painting them a second time (see entryMsg.n).
	renderedN int
	wrapped   string // the wrapped transcript the viewport shows
	// Selection and clipboard (see selection.go, clipboard.go).
	sel        *selection
	wheelFrame int // rotation frame while busy

	// quitHint remembers that this terminal's last key was a Ctrl+C on an
	// empty input: the second one ends the session. It is per view because
	// the confirmation belongs to the person who pressed it — otherwise A's
	// Ctrl+C plus B's unrelated Ctrl+C would end a session neither of them
	// asked to end.
	quitHint bool

	queueCursor int // highlighted row in the queue popup

	clipboardWrite func(string) error
	clipboardRead  func() (string, error)
	termWrite      func(string) // raw escape writer (terminal window colours); swappable for tests

	ascii bool // some attached client cannot show UTF-8 glyphs

	mb *mailbox // broadcasts from the session, waiting to be rendered here

	// quitSeen records that this view handled a quitMsg. Test-only: in
	// production the tea.Quit it returns is the observable effect.
	quitSeen bool

	// panicOnNextUpdate makes the next update panic with testPanicValue.
	// Test-only, and the only way to exercise the runner's recover path for
	// real: a view that dies mid-frame must take its own terminal down
	// ("view error") and leave the session and every other terminal
	// running. Never set in production.
	panicOnNextUpdate bool
}

// mailboxForTest exposes this view's mailbox so a test can deliver what the
// session broadcast without running the delivery goroutine (see flush).
func (m *View) mailboxForTest() *mailbox { return m.mb }

// ---- tea.Model -------------------------------------------------------------

func (m *View) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spin.Tick, m.pingCmd())
}

func (m *View) pingCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.prov.Ping(m.rootCtx)
		if err != nil {
			return noticeMsg(fmt.Sprintf("backend %s unreachable: %v", m.prov.Name(), err))
		}
		return noticeMsg(fmt.Sprintf("backend %s: %s", m.prov.Name(), status))
	}
}

// Update runs this terminal's program. It holds the session lock for its
// whole body, so everything it reaches — the transcript, the run state, the
// queue, the roster — is read and written under the one lock the agent
// goroutine takes too. Nothing under that lock may block on a program: the
// broadcasts it raises go into mailboxes, never down a tea.Program channel.
func (m *View) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	model, cmd := m.update(msg)
	// Render this view's own broadcasts before the frame Bubble Tea draws
	// from this return: everything update just appended went out as an
	// entryMsg, and waiting for the mailbox goroutine to bring it back round
	// through p.Send would leave the transcript one message behind its own
	// keystroke. The mailbox goroutine still matters — it is how broadcasts
	// from *other* goroutines wake this program — it simply finds the queue
	// already empty for the ones raised here.
	if drained := m.mb.drainInto(m); drained != nil {
		cmd = tea.Batch(cmd, drained)
	}
	return model, cmd
}

// testPanicValue is what panicOnNextUpdate panics with, so the line the
// runner logs is recognisably a test's doing and not a real fault.
const testPanicValue = "tui: deliberate test panic in View.update"

func (m *View) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.panicOnNextUpdate {
		m.panicOnNextUpdate = false
		panic(testPanicValue)
	}
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Also the repaint request (see runner.onClients). It reaches the
		// program through its own queue rather than the session mailbox
		// (see program.ctrl), so Bubble Tea's renderer sees it and repaints
		// in full; all that is left for the model is the relayout — which a
		// size that has not changed does not need, and which must not throw
		// away this terminal's selection or re-render every entry.
		resized := msg.Width != m.width || msg.Height != m.height
		m.width, m.height = msg.Width, msg.Height
		if resized {
			m.sel = nil // columns no longer line up after a rewrap
			m.layout()
		}
		m.ready = true
		// Announce the editor bridge here, not on stderr: the alt screen
		// wipes anything printed before it opened. Once only.
		if !m.ideAnnounced && m.ag.IDEName != "" {
			m.ideAnnounced = true
			m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("VS Code connected: %d tools", m.ag.IDETools)})
		}
		// Nudge once toward /init for a project with no notes file yet. The
		// check itself is latched, not just the hint: this runs on every
		// resize, and NeedsInitHint stats four paths in the workspace.
		if !m.initChecked {
			m.initChecked = true
			if ui.NeedsInitHint(m.ag.Tools.Root) {
				m.initHinted = true
				m.appendEntryLocked(entry{Kind: entryDim, Text: ui.InitHint})
			}
		}
		if resized {
			m.rebuild()
		}
		// A terminal that attached while a question was already open built
		// its modal in NewView, before it had a size of its own. Now that it
		// has one, build it for real.
		if m.mode == modeAsk && m.shownAsk != nil && m.modalVP.Width <= 0 {
			m.showAsk(m.shownAsk)
		}
		// The remembered-theme fallback warning, if any: once, on the first
		// size this terminal reports, after the rebuild above so it is not
		// immediately wiped by it.
		if m.themeWarn != "" {
			m.renderLocalNote(m.themeWarn)
			m.themeWarn = ""
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case wheelTickMsg:
		if m.running {
			m.wheelFrame++
			return m, m.wheelTick()
		}
		return m, nil
	case entryMsg:
		// The session already recorded it; this view renders it — unless a
		// rebuild has overtaken it (see renderedN and entryMsg.n), in which
		// case it is already on screen and this copy is dropped.
		if msg.n >= m.renderedN {
			m.renderEntryLocal(msg.e)
			m.renderedN = msg.n + 1
		}
	case deltaMsg:
		m.streaming += string(msg)
		m.refreshTranscript()
	case streamEndMsg:
		// The session has turned the stream into an entry; the entryMsg for
		// it is right behind this one.
		m.streaming = ""
		m.refreshTranscript()
	case transientMsg:
		// The session already recorded it (so a terminal attaching now still
		// sees it); this is the same write, plus the expiry tick that is
		// each view's own.
		m.toast = string(msg)
		m.toastUntil = m.now().Add(toastFor)
		return m, tea.Tick(toastFor+100*time.Millisecond, func(t time.Time) tea.Msg { return toastTickMsg(t) })
	case toastTickMsg:
		if m.toast != "" && !m.now().Before(m.toastUntil) {
			m.toast = ""
		}
		return m, nil
	case noticeMsg:
		// This terminal's own note (its backend ping): rendered here, not
		// appended to the shared transcript, where one program per terminal
		// would put one copy of it per terminal. Notes from the agent come
		// through Session.notice as real entries instead.
		m.renderEntryLocal(noticeEntry(string(msg)))
	case statusMsg:
		m.statusNote = string(msg)
		if m.statusNote == "" && m.running {
			m.statusNote = "thinking"
		}
	case askMsg:
		m.showAsk(msg.a)
	case askResolvedMsg:
		// Someone answered, or the editor did and the coordinator withdrew
		// it. Whoever answered is not told that somebody answered.
		if m.mode == modeAsk && m.askShown == msg.gen {
			answeredHere := m.answeredGen == msg.gen || msg.from == m.id
			m.closeAsk()
			if note := answeredNote(msg.by); !answeredHere && note != "" {
				m.renderLocalNote(note)
			}
		}
	case runStateMsg:
		// A popup or a shared question this terminal has open keeps the
		// frame; the mode underneath it changes instead, so closing the
		// popup lands in whatever the session is doing by then.
		if msg.running {
			m.input.Placeholder = busyPlaceholder
			m.setIdleMode(modeBusy)
			cmds = append(cmds, m.wheelTick())
		} else {
			if m.mode == modeQueue {
				m.closeQueue()
			}
			m.setIdleMode(modeInput)
			m.input.Focus()
		}
	case usageMsg:
		m.usage = msg
	case clientsMsg:
		// The shared half of a roster change is the session's (SetClients)
		// and the programs are the runner's; all this view has to do is draw
		// the new bottom line, which View() reads from m.clients.
	case quitMsg:
		m.quitSeen = true
		return m, tea.Quit
	case drainMsg:
		// Just "your mailbox has something in it": Update drains it around
		// every message, this one included (see mailbox.run).
		return m, nil
	case pickerItemsMsg:
		m.pickerUpdate(msg)
	case tea.KeyMsg:
		// Every key a program receives is its own terminal's: the runner
		// routes each client's bytes to that client's program (see
		// runner.route), so there is no sender to disambiguate here.
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	}

	if m.mode == modeInput {
		updated, cmd := m.input.Update(msg)
		m.input = updated
		cmds = append(cmds, cmd)
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// handleKey routes one keystroke. It is always this terminal's own: one
// program per attached terminal means the runner has already decided whose
// key this is. Popups (the palette, the menus, the queue) are this view's
// alone; the shared question — an approval, a plan, a shared picker —
// belongs to the whole session and any terminal may answer it.
func (m *View) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeAsk:
		return m.handleAskKey(k)
	case modePicker:
		return m.handlePickerKey(k)
	case modePalette:
		return m.handlePaletteKey(k)
	case modeMenu, modeContextMenu:
		return m.handleMenuKey(k)
	case modeQueue:
		return m.handleQueueKey(k)
	case modeBusy:
		return m.handleBusyKey(k)
	}
	return m.handleInputKey(k)
}

// setIdleMode moves this terminal between input and busy. A popup or a
// shared question this terminal has open keeps the frame: it closes through
// idleMode(), which reads the run state as it is by then, so there is
// nothing to change underneath it.
func (m *View) setIdleMode(to mode) {
	if m.mode == modeInput || m.mode == modeBusy {
		m.mode = to
	}
}

// handleInputKey is modeInput: the key edits, submits or acts on this
// terminal's own draft.
func (m *View) handleInputKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.input
	switch k.Type {
	case tea.KeyCtrlC:
		if m.sel != nil {
			m.copyText(m.selectionText(), "selection")
			m.clearSelection()
			return m, nil
		}
		if in.Value() != "" {
			in.Reset()
			m.quitHint = false
			return m, nil
		}
		if m.quitHint {
			// The whole session ends, not just this terminal: the view exits
			// when its own quitMsg comes back round.
			m.QuitLocked()
			return m, nil
		}
		m.quitHint = true
		m.appendEntryLocked(entry{Kind: entryDim, Text: "press Ctrl+C again to quit"})
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(in.Value())
		if text == "" {
			return m, nil
		}
		m.quitHint = false
		m.histFile.add(text, m.id)
		in.Reset()
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text)
		}
		m.Submit(text, m.id)
		return m, nil
	case tea.KeyTab:
		m.completeSlash()
		return m, nil
	case tea.KeyEsc:
		if m.sel != nil {
			m.clearSelection()
			return m, nil
		}
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == '/' && strings.TrimSpace(in.Value()) == "" {
			return m.openPalette("")
		}
	case tea.KeyUp:
		if in.LineCount() <= 1 {
			if prev, ok := m.histFile.prev(m.id); ok {
				in.SetValue(prev)
				in.CursorEnd()
			}
			return m, nil
		}
	case tea.KeyDown:
		if in.LineCount() <= 1 {
			next, _ := m.histFile.next(m.id)
			in.SetValue(next)
			in.CursorEnd()
			return m, nil
		}
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	}
	updated, cmd := in.Update(k)
	*in = updated
	return m, cmd
}

// padToWidth pads s with spaces to w display columns, or truncates it if it
// is already wider. Display width (not byte or rune count) matters here:
// the line may carry ANSI styling.
func padToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cur := lipgloss.Width(s)
	if cur > w {
		return ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", w-cur)
}

// noticeEntry is the transcript entry one note becomes. The editor context
// note is ambient information, not a warning: it is rendered dimmed and
// unlabelled.
func noticeEntry(text string) entry {
	if strings.HasPrefix(text, "[editor:") {
		return entry{Kind: entryDim, Text: text}
	}
	return entry{Kind: entryNote, Text: text}
}

// showAsk puts the session's shared question on this terminal's screen. The
// detail and the item list are shared; the viewport scroll and the picker
// cursor built here are this terminal's own.
func (m *View) showAsk(a *ask) {
	m.shownAsk = a
	m.askShown = a.Gen
	m.mode = modeAsk
	m.picker = nil
	m.modalVP = viewport.New(m.width-6, m.modalHeight())
	switch a.Kind {
	case askApproval:
		if a.Action == "consult" {
			// Not a diff: a question whose first word happens to be "-" is
			// not a deletion, and colouring it as one would say it was.
			m.modalVP.SetContent(a.Detail)
			break
		}
		m.modalVP.SetContent(ui.ColorizeDiff(a.Detail, true))
	case askPlan:
		m.modalVP.SetContent(a.Detail)
	case askPicker:
		m.picker = &picker{title: a.Title, items: a.Items}
	}
}

// closeAsk takes the shared question off this terminal's screen, whether it
// answered it or another terminal did.
func (m *View) closeAsk() {
	m.shownAsk = nil
	m.picker = nil
	m.mode = m.idleMode()
	m.input.Focus()
}

// answeredNote is the dimmed line a terminal that did *not* answer shows in
// its own buffer. A withdrawal passes the note itself ("answered in VS
// Code"); anything else is a client label. An empty by shows nothing.
func answeredNote(by string) string {
	if by == "" {
		return ""
	}
	if strings.HasPrefix(by, "answered") {
		return by
	}
	return "answered by " + by
}

// renderLocalNote writes one dimmed line into this terminal's own buffer. It
// is not a transcript entry: the other terminals have no business seeing
// that this one was told who answered.
func (m *View) renderLocalNote(text string) {
	m.rendered.WriteString(m.st.Dim.Render(text) + "\n")
	m.refreshTranscript()
}

// renderLocalLines writes already-styled lines into this terminal's own
// buffer, for a listing whose parts carry more than one style (/coworkers).
func (m *View) renderLocalLines(lines []string) {
	for _, l := range lines {
		m.rendered.WriteString(l + "\n")
	}
	m.refreshTranscript()
}

// handleAskKey answers the shared question, or scrolls its body. The answer
// is recorded as this terminal's, and this is the only modal it closes
// directly — the rest close on the askResolvedMsg that Answer broadcasts.
func (m *View) handleAskKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.shownAsk
	if a == nil {
		m.mode = m.idleMode()
		m.picker = nil
		return m, nil
	}
	if a.Kind == askPicker {
		return m.handleAskPickerKey(k)
	}
	var ans askAnswer
	decided := true
	switch strings.ToLower(k.String()) {
	case "y":
		ans = askAnswer{OK: true}
	case "n", "esc":
		ans = askAnswer{OK: false}
	case "a":
		// "always": only an approval knows what to stop asking about.
		if a.Kind != askApproval {
			decided = false
			break
		}
		if a.Action == "shell" {
			m.cfg.AutoApproveShell = true
			ans = askAnswer{OK: true, Note: "shell auto-approve enabled for this session"}
		} else if a.Action == "consult" {
			// Session-wide consent for this one co-worker, recorded on the
			// agent (never in the config file): a standing "yes" to sending
			// code off this machine is not something to persist behind the
			// user's back. A detail with no parsable name approves this one
			// consultation and nothing more.
			if name := agent.ConsentCoworker(a.Detail); name != "" {
				m.ag.AllowCoworker(name)
				ans = askAnswer{OK: true, Note: "co-worker " + name + " allowed for this session"}
			} else {
				ans = askAnswer{OK: true}
			}
		} else {
			// Both switches: cfg stops the terminal prompt, the registry
			// flag stops the editor diff review (see Registry.ApproveWrites).
			m.cfg.ApproveFileWrites = false
			m.ag.Tools.ApproveWrites = false
			ans = askAnswer{OK: true, Note: "file-write previews disabled for this session"}
		}
	default:
		decided = false
	}
	if !decided {
		var cmd tea.Cmd
		m.modalVP, cmd = m.modalVP.Update(k)
		return m, cmd
	}
	return m, m.answerAsk(ans)
}

// answerAsk puts this terminal's verdict to the session and, if it was the
// one that counted, closes this terminal's own modal (the others close on
// the askResolvedMsg it broadcasts). A stale answer leaves the modal alone:
// the resolution for the generation that did win is already on its way.
func (m *View) answerAsk(ans askAnswer) tea.Cmd {
	gen := m.askShown
	cmd, ok := m.Answer(gen, ans, m.id, m)
	if ok {
		m.answeredGen = gen
		m.closeAsk()
	}
	return cmd
}

// handleAskPickerKey drives a shared picker: the cursor and filter are this
// terminal's own, the row it confirms decides for the session.
func (m *View) handleAskPickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		p = &picker{title: m.shownAsk.Title, items: m.shownAsk.Items}
		m.picker = p
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		cmd := m.answerAsk(askAnswer{})
		m.input.Focus()
		return m, cmd
	case tea.KeyEnter:
		items := p.filtered()
		if len(items) == 0 {
			return m, nil
		}
		return m, m.answerAsk(askAnswer{OK: true, Note: items[p.cursor].id})
	}
	pickerNav(p, k)
	return m, nil
}

// handleBusyKey: while the agent works the input stays live. Enter queues
// the text for delivery at the model's next call; Esc/Ctrl-C cancels the
// run and discards the queue; PgUp/PgDn scroll; everything else edits.
func (m *View) handleBusyKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.input
	switch k.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		if k.Type == tea.KeyCtrlC && m.sel != nil {
			m.copyText(m.selectionText(), "selection")
			m.clearSelection()
			return m, nil
		}
		if m.cancelFn != nil {
			m.cancelFn()
		}
		if m.consultCancel != nil {
			m.consultCancel()
		}
		if n := len(m.ag.DrainInbox()); n > 0 {
			m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("discarded %d queued message(s)", n)})
		}
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(in.Value())
		if text == "" {
			return m, nil
		}
		in.Reset()
		if strings.HasPrefix(text, "/") {
			if ui.BusySafeCommand(text) {
				m.histFile.add(text, m.id)
				return m.slashCommand(text)
			}
			m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the agent is done (Esc cancels); plain text is queued"})
			return m, nil
		}
		m.histFile.add(text, m.id)
		m.ag.EnqueueFrom(text, m.id)
		if len(m.clients) > 1 {
			m.appendEntryLocked(entry{Kind: entryQueued, Label: "queued> ", Text: m.userPrefix(m.id) + text})
		} else {
			m.appendEntryLocked(entry{Kind: entryQueued, Label: "queued (delivered at the next step)> ", Text: text})
		}
		return m, nil
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	case tea.KeyUp:
		if strings.TrimSpace(in.Value()) == "" {
			return m.openQueue()
		}
	case tea.KeyCtrlQ:
		return m.openQueue()
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == '/' && strings.TrimSpace(in.Value()) == "" {
			return m.openPalette("")
		}
	}
	updated, cmd := in.Update(k)
	*in = updated
	return m, cmd
}

// ---- transcript helpers ----------------------------------------------------

// renderEntryLocal renders one entry into this view's own buffer. It is the
// second half of Session.appendEntryLocked: the entry is already on the
// shared transcript, and this is the view's private rendering of it, at its
// width and in its theme.
func (m *View) renderEntryLocal(e entry) {
	m.rendered.WriteString(renderEntry(e, m.st, m.width, m.compact(), m.richText))
	m.rendered.WriteString("\n")
	m.refreshTranscript()
}

// rebuild re-renders every entry — after a theme change or a resize, since
// tool-argument truncation and Markdown colouring depend on both.
func (m *View) rebuild() {
	m.rendered.Reset()
	for _, e := range m.entries {
		m.rendered.WriteString(renderEntry(e, m.st, m.width, m.compact(), m.richText))
		m.rendered.WriteString("\n")
	}
	m.renderedN = len(m.entries)
	m.refreshTranscript()
}

func (m *View) refreshTranscript() {
	if !m.ready {
		return
	}
	content := m.rendered.String() + m.streaming
	m.wrapped = lipgloss.NewStyle().Width(m.vp.Width).Render(content)
	// A transcript shorter than the viewport is anchored to its bottom, just
	// above the input line, by padding it from the top: the newest lines are
	// then always in the same place as they are once the viewport is full,
	// and a terminal that shows only the bottom of a frame taller than its
	// screen (a phone whose pty counts the rows under its keyboard) still
	// sees them. The padding is part of m.wrapped so mouse coordinates and
	// selection keep mapping onto the same lines the viewport shows.
	if n := strings.Count(m.wrapped, "\n") + 1; n < m.vp.Height {
		m.wrapped = strings.Repeat("\n", m.vp.Height-n) + m.wrapped
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.highlighted())
	if atBottom {
		m.vp.GotoBottom()
	}
}

// PublicVersion is the user-facing release line shown in the header,
// independent of the internal build version in build.mk.
const PublicVersion = "v1.0"

// headerMinRows is the terminal height from which the branded header is
// drawn; below it the rows go to the transcript.
const headerMinRows = 30

func (m *View) showHeader() bool { return m.height >= headerMinRows && !m.compact() }

func (m *View) headerHeight() int {
	if m.showHeader() {
		return 5 // 3-row title box, attribution line, dashed rule
	}
	return 0
}

func (m *View) layout() {
	bottomH := 1
	vpH := m.height - m.headerHeight() - m.inputRows() - bottomH - 1
	if vpH < 3 {
		vpH = 3
	}
	m.vp.Width = m.width
	m.vp.Height = vpH
	setInputPrompt(&m.input, m.compact())
	m.input.SetWidth(m.inputWidth())
	m.input.SetHeight(m.inputRows())
}

func (m *View) modalHeight() int {
	if m.compact() {
		h := m.height - 3
		if h < 3 {
			h = 3
		}
		return h
	}
	h := m.height - 8
	if h < 5 {
		h = 5
	}
	return h
}

// ---- view ------------------------------------------------------------------

func (m *View) View() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return "loading…"
	}
	switch m.mode {
	case modeAsk:
		// Falls through to the ordinary frame if the ask has just been
		// resolved elsewhere and this view has yet to drain the message.
		if v := m.viewAsk(); v != "" {
			return v
		}
	case modePicker:
		return m.viewPicker()
	case modeMenu:
		return m.viewMenu()
	}

	var b strings.Builder
	if m.showHeader() {
		b.WriteString(m.headerView())
		b.WriteString("\n")
	}
	transcript := m.vp.View()
	if ((m.mode == modePalette || m.mode == modeContextMenu) && m.picker != nil) || m.mode == modeQueue {
		// Popups sit over the bottom of the transcript, right above the
		// input; the newest lines stay visible above them.
		var box string
		switch m.mode {
		case modeQueue:
			box = m.queueBox()
		case modeContextMenu:
			box = m.contextMenuBox()
		default:
			box = m.paletteBox()
		}
		boxH := lipgloss.Height(box)
		lines := strings.Split(transcript, "\n")
		if boxH < len(lines) {
			lines = lines[boxH:]
		} else {
			lines = nil
		}
		transcript = strings.Join(lines, "\n") + "\n" + box
	}
	if m.toast != "" && m.mode != modePalette && m.mode != modeContextMenu && m.mode != modeQueue {
		// The notice row borrows the last transcript row so the input rows
		// never move.
		lines := strings.Split(transcript, "\n")
		if len(lines) > 0 {
			lines[len(lines)-1] = m.st.Warn.Render(padToWidth(" "+m.toast, m.width))
			transcript = strings.Join(lines, "\n")
		}
	}
	b.WriteString(transcript)
	b.WriteString("\n")
	b.WriteString(m.inputRow())
	b.WriteString("\n")
	// Padded, never written raw: the compact line in particular is built
	// from whatever the model is called and how many terminals are
	// attached, and one cell too many wraps the row and scrolls the whole
	// frame up on a phone-sized terminal.
	b.WriteString(padToWidth(m.bottomLine(), m.width))
	return b.String()
}

// headerView draws the branded header: boxed title, logo at the right,
// attribution, dashed rule.
func (m *View) headerView() string {
	box := m.st.Border.Render(m.st.Text.Bold(true).Render("BE-Code Redux"))
	lines := strings.Split(box, "\n")
	code := "—"
	if m.ag.Session != nil {
		code = m.ag.Session.ResumeCode()
	}
	logo := m.st.Accent.Render("session " + code)
	if len(lines) >= 2 {
		gap := m.width - lipgloss.Width(lines[1]) - lipgloss.Width(logo) - 1
		if gap < 1 {
			gap = 1
		}
		lines[1] += strings.Repeat(" ", gap) + logo
	}
	attribution := m.st.Dim.Render("  2026 BE AI Research · https://github.com/BE-AI-Research - " + PublicVersion)
	rule := m.st.Dim.Render(strings.Repeat("- ", m.width/2))
	return strings.Join(lines, "\n") + "\n" + attribution + "\n" + rule
}

// bottomLine is the single row under the input: menu hint, model, state.
func (m *View) bottomLine() string {
	if m.compact() {
		return m.compactBottomLine()
	}
	state := m.st.OK.Render("ready")
	if m.running {
		state = m.spin.View() + " " + m.statusNote + m.st.Dim.Render(" · Enter queues · Esc cancels")
		if n := m.ag.Pending(); n > 0 {
			state += m.st.Accent.Render(fmt.Sprintf(" · %d queued · ↑ edit", n))
		}
	}
	line := " " + m.st.Accent.Render("/menu") + " " + m.st.Accent.Render("/help") +
		m.st.Dim.Render(" · "+shortModel(m.ag.Model)+" · ") + state
	if m.ag.IDEName != "" {
		line += m.st.Accent.Render(" " + m.ideMarker())
	}
	if n := len(m.clients); n > 1 {
		line += m.st.Accent.Render(fmt.Sprintf(" %s %d", m.clientsGlyph(), n))
		if labels := m.clientLabels(m.width - lipgloss.Width(line) - 3); labels != "" {
			line += m.st.Dim.Render(" · " + labels)
		}
	}
	if m.sel != nil {
		line += m.st.Dim.Render(" · selection: Ctrl+C copy · right-click menu · Esc clear")
	}
	return line
}

// shortModel trims registry prefixes and long tags so the model fits the
// bottom line: "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL" → "Qwen3.8-27B-GGUF:UD-Q4_K_XL".
func shortModel(name string) string { return shortModelTo(name, 32) }

// shortModelTo is shortModel with the cap spelled out: the compact layout
// trims harder, because on a 40-column terminal the model name is what
// pushes the clients marker — the only sign the session is shared — off the
// end of the row.
func shortModelTo(name string, max int) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if max < 2 {
		max = 2
	}
	if r := []rune(name); len(r) > max {
		name = string(r[:max-1]) + "…"
	}
	return name
}

// viewAsk renders the shared question: the approval modal, the plan modal or
// the picker list. "" means there is nothing to draw (the ask was resolved
// between the answer and this view's askResolvedMsg).
func (m *View) viewAsk() string {
	a := m.shownAsk
	if a == nil {
		return ""
	}
	switch a.Kind {
	case askPicker:
		return m.viewPicker()
	case askPlan:
		hint := " y execute · n discard · ↑↓ scroll"
		if m.compact() {
			hint = " y/n · ↑↓"
		}
		body := m.st.Border.Width(m.width - 4).Render(
			m.st.ModalTi.Render(a.Title) + "\n\n" + m.modalVP.View())
		return body + "\n" + m.st.Dim.Render(hint)
	}
	title := "Shell command"
	hint := "y approve · n deny · a always-approve shell · ↑↓ scroll"
	switch a.Action {
	case "file_write":
		title = "File change"
		hint = "y approve · n deny · a stop asking for writes · ↑↓ scroll"
	case "consult":
		// Sending this workspace's code to an online model is a decision
		// about a co-worker by name, not a class of action, so "a" says
		// whose it is.
		title = "Co-working model"
		hint = "y allow this · n decline · a allow " + agent.ConsentCoworker(a.Detail) + " for the session · ↑↓ scroll"
	}
	if m.compact() {
		hint = "y/n/a · ↑↓"
	}
	body := m.st.Border.Width(m.width - 4).Render(
		m.st.ModalTi.Render(title+" — approval required") + "\n\n" + m.modalVP.View())
	return body + "\n" + m.st.Dim.Render(" "+hint)
}

// ---- the input line --------------------------------------------------------
//
// One textarea per terminal, because a draft belongs to whoever is typing
// it. The in-process TUI is simply the one view of a session with no host.

// newInputArea builds this terminal's textarea with the prompt, height and
// key bindings every input line shares.
func (m *View) newInputArea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "describe a task…  (Enter sends · Ctrl+J newline · / for commands)"
	ta.SetHeight(m.inputRows())
	setInputPrompt(&ta, m.compact())
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	styleInput(&ta, m.st)
	ta.Focus()
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")
	if m.served {
		// A blinking cursor is a frame every 500 ms down the socket, per
		// terminal, for nothing anyone can see moving.
		ta.Cursor.SetMode(cursor.CursorStatic)
	}
	if w := m.inputWidth(); w > 0 {
		ta.SetWidth(w)
	}
	return ta
}

// styleInput colours a textarea from the theme. The bubbles defaults leave
// the typed text in the terminal's own foreground and paint the cursor line
// on ANSI black — invisible, respectively, on a terminal whose default
// foreground is dark and whose background the theme has recoloured
// (Termux under solarized-dark showed nothing at all), so every part that
// carries text takes a theme colour and the cursor line carries none.
func styleInput(ta *textarea.Model, st styles) {
	plain := lipgloss.NewStyle()
	for _, s := range []*textarea.Style{&ta.FocusedStyle, &ta.BlurredStyle} {
		s.Base = plain
		// The line holding the cursor — every line of a one-row input — is
		// rendered with CursorLine, not Text, so both carry the colour.
		s.CursorLine = st.Text
		s.Text = st.Text
		s.Prompt = st.Accent
		s.Placeholder = st.Dim
		s.EndOfBuffer = plain
	}
	// The textarea renders through a pointer to whichever style struct was
	// current when Focus/Blur last ran — a pointer into a *copy* once the
	// model has been returned by value — so re-point it at the styles just
	// set, or the new colours are never read.
	if ta.Focused() {
		ta.Focus()
	} else {
		ta.Blur()
	}
}

// setInputPrompt gives a textarea the prompt of the current layout: the
// short one in compact, the full one otherwise. layout() applies it on a
// resize, and newInputArea applies it once up front — a terminal that is
// compact from its very first frame must not have to wait for a resize to
// get the right prompt.
func setInputPrompt(ta *textarea.Model, compact bool) {
	if compact {
		ta.SetPromptFunc(2, func(i int) string {
			if i == 0 {
				return "> "
			}
			return "  "
		})
		return
	}
	ta.SetPromptFunc(5, func(i int) string {
		if i == 0 {
			return "(>): "
		}
		return "     "
	})
}

// inputRows is the height of the input area: three rows, or one when this
// terminal is small enough for the compact layout.
func (m *View) inputRows() int {
	if m.compact() {
		return 1
	}
	return 3
}

// wheelWidthNow is the width of the context-wheel column to the right of
// the input row: the full glyph-plus-percentage field, or the unpadded
// short form in compact layout.
func (m *View) wheelWidthNow() int {
	if m.compact() {
		return 5 // glyph + "NN%", no fixed-width padding
	}
	return wheelWidth
}

// inputWidth is the width of the input line, leaving room for the wheel.
func (m *View) inputWidth() int {
	w := m.width - m.wheelWidthNow() - 2
	if w < 0 {
		w = 0
	}
	return w
}

// inputRow is the input area plus the context wheel at its right.
func (m *View) inputRow() string {
	return lipgloss.JoinHorizontal(lipgloss.Top, m.input.View(), " "+m.wheelView())
}

// ---- slash commands --------------------------------------------------------

func (m *View) completeSlash() {
	in := &m.input
	v := in.Value()

	// @path completion on the last token.
	if i := strings.LastIndex(v, "@"); i >= 0 && !strings.ContainsAny(v[i:], " \n") {
		prefix := v[i+1:]
		matches := agent.CompleteMention(m.ag.Tools.Root, prefix)
		if len(matches) == 1 {
			in.SetValue(v[:i+1] + matches[0])
			in.CursorEnd()
		} else if len(matches) > 1 {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "@" + strings.Join(matches, "  @")})
		}
		return
	}

	if !strings.HasPrefix(v, "/") || strings.Contains(v, " ") {
		return
	}
	all := append([]string(nil), ui.SlashCommands...)
	for name := range m.custom {
		all = append(all, "/"+name)
	}
	var matches []string
	for _, c := range all {
		if strings.HasPrefix(c, v) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		in.SetValue(matches[0] + " ")
		in.CursorEnd()
	} else if len(matches) > 1 {
		m.appendEntryLocked(entry{Kind: entryDim, Text: strings.Join(matches, "  ")})
	}
}

// slashCommand runs a command typed at this terminal. Commands that fill an
// input line or act on a terminal act on this one: m.id is the client that
// typed it, because one program renders for one terminal.
func (m *View) slashCommand(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	switch fields[0] {
	case "/quit", "/exit", "/q":
		// The whole session ends, not just this terminal: Quit cancels any
		// run and broadcasts, and this view exits when its own quitMsg comes
		// back round through the mailbox.
		m.QuitLocked()
		return m, nil
	case "/menu":
		return m.openMenu()
	case "/theme":
		// This terminal's own theme, never a shared one: bare opens the
		// picker (its title says which theme is in use and where it came
		// from), a name sets it for this device, "default <name>" changes
		// what a new device gets.
		if len(fields) >= 3 && strings.ToLower(fields[1]) == "default" {
			return m.applyTheme(strings.ToLower(fields[2]), true)
		}
		if len(fields) > 1 {
			return m.applyTheme(strings.ToLower(fields[1]), false)
		}
		return m.openThemePicker()
	case "/copy":
		m.copyTarget(strings.TrimSpace(strings.TrimPrefix(text, "/copy")))
		return m, nil
	case "/help":
		help := strings.TrimSpace(`
/model /provider   pickers (or pass a name)   /sessions /resume  saved sessions
/plan <task>       plan → approve → execute   /undo              roll back last turn
/commit            model-written git commit   /init              generate BECODE.md
/verify /map       checks · repo map          /compact /stats    context · usage
/tools /config     tool list · settings       /handoff resume briefing
/clear /quit
Tab completes commands and @file mentions; @path pins a file into context.`)
		if len(m.custom) > 0 {
			help += "\ncustom: /" + strings.Join(commands.Names(m.custom), " /")
		}
		m.appendEntryLocked(entry{Kind: entryDim, Text: help})
	case "/clear":
		m.ag.History.Messages = nil
		m.ag.SetSession(store.NewSession(m.prov.Name(), m.ag.Model, m.ag.Tools.Root))
		m.appendEntryLocked(entry{Kind: entryOK, Text: "history cleared; new session started"})
	case "/tools":
		m.appendEntryLocked(entry{Kind: entryDim, Text: strings.Join(m.ag.Tools.Names(), " · ")})
	case "/config":
		p, _ := config.Path()
		m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf(
			"%s\nprovider=%s model=%s ui=%s ctx=%d max_turns=%d max_repairs=%d compat=%s previews=%v",
			p, m.cfg.DefaultProvider, m.cfg.Model, m.cfg.UI, m.cfg.ContextTokens,
			m.cfg.MaxTurns, m.cfg.MaxRepairs, m.cfg.CompatToolCalls, m.cfg.ApproveFileWrites)})
	case "/verify":
		m.setRunStateLocked(true, "verifying")
		ctx := m.runContextLocked()
		go func() {
			proj := verify.Detect(m.ag.Tools.Root)
			rep := verify.RunChecks(ctx, m.ag.Tools.Root, proj)
			m.finishTurn(&agent.ReviewedReport{Verify: rep}, nil)
		}()
	case "/model":
		if len(fields) > 1 {
			m.ag.SetModel(fields[1])
			m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("model set to %s (profile %s)", fields[1], m.ag.Profile.Family)})
			break
		}
		return m, m.askModelPicker()
	case "/undo":
		restored, err := m.ag.Undo()
		if err != nil {
			m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
			break
		}
		m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("restored: %s (%d undo levels left)",
			strings.Join(restored, ", "), m.ag.Checkpoints.Depth())})
	case "/commit":
		m.setRunStateLocked(true, "committing")
		ctx := m.runContextLocked()
		sess := m.Session
		go func() {
			line, err := sess.ag.GenerateCommit(ctx)
			if err != nil {
				sess.notice("commit failed: " + err.Error())
			} else {
				sess.notice("committed: " + line)
			}
			sess.finishTurn(nil, nil)
		}()
	case "/init":
		// Everything a turn does on the way in, because /init is a real
		// model request: the run state every terminal reacts to (busy mode,
		// the queue placeholder, the wheel) and a cancellable context in
		// cancelFn so Esc reaches the scan and the model call (handleBusyKey
		// cancels whatever is there).
		m.setRunStateLocked(true, "mapping the project")
		ctx := m.runContextLocked()
		sess := m.Session
		go func() {
			path, err := ui.RunInit(ctx, sess.ag, ui.InitOptions{
				Root:    sess.ag.Tools.Root,
				Approve: func(p string) bool { return sess.approveFromAgent("file_write", p) },
				Log:     sess.notice,
			})
			sess.finishInit(path, err)
		}()
		return m, nil
	case "/compact":
		m.setRunStateLocked(true, "compacting")
		ctx := m.runContextLocked()
		sess := m.Session
		go func() {
			if err := sess.ag.Compact(ctx); err != nil {
				sess.notice("compaction failed: " + err.Error())
			} else {
				sess.notice(fmt.Sprintf("compacted; context now ~%d tokens", sess.ag.History.Tokens()))
			}
			sess.finishTurn(nil, nil)
		}()
	case "/stats":
		s := m.ag.Usage()
		m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf(
			"requests=%d tool_calls=%d prompt_tokens=%d completion_tokens=%d elapsed=%s ctx=%d/%d",
			s.Requests, s.ToolCalls, s.PromptTokens, s.CompletionTokens,
			s.Elapsed.Round(time.Second/10), m.ag.History.Tokens(), m.ag.History.Budget)})
	case "/map":
		if mp := m.ag.RepoMap(); mp == "" {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "no repo map (unrecognized files or disabled)"})
		} else {
			m.appendEntryLocked(entry{Kind: entryDim, Text: mp})
		}
	case "/plan":
		req := strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
		if req == "" {
			m.appendEntryLocked(entry{Kind: entryErr, Text: "usage: /plan <task description>"})
			break
		}
		m.appendEntryLocked(entry{Kind: entryUser, Label: "plan> ", Text: req})
		m.setRunStateLocked(true, "planning (read-only)")
		ctx := m.runContextLocked()
		sess := m.Session
		go func() {
			plan, err := sess.ag.Plan(ctx, req)
			sess.send(sess.usageSnapshot())
			if err != nil {
				// Through finishTurn, not as an error entry of its own, so a
				// cancelled plan prints the same "cancelled" line every other
				// cancelled turn does.
				sess.finishTurn(nil, err)
				return
			}
			// The verdict entry ("plan approved" / "plan discarded") is
			// Answer's, so every terminal sees what was decided.
			a := &ask{Kind: askPlan, Title: "Implementation plan — approve to execute",
				Detail: plan, req: req}
			if !sess.Ask(ctx, a).OK {
				sess.finishTurn(nil, nil)
				return
			}
			sess.setRunState(true, "executing plan")
			_, rep, err := sess.ag.ExecutePlan(ctx, a.req, a.Detail)
			sess.send(sess.usageSnapshot())
			sess.finishTurn(rep, err)
		}()
		return m, nil
	case "/models":
		return m, m.askModelPicker()
	case "/provider":
		if len(fields) > 1 {
			return m.setProvider(fields[1])
		}
		return m, m.askProviderPicker()
	case "/handoff":
		if h := m.ag.Handoff(); h != "" {
			for _, line := range strings.Split(h, "\n") {
				m.appendEntryLocked(entry{Kind: entryDim, Text: line})
			}
		} else {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "no handoff briefing in this session"})
		}
	case "/review":
		m.reviewCommand(fields)
	case "/coworkers":
		// A listing, not a shared event: the terminal that asked is the one
		// that wants to read it.
		cws := m.ag.Coworkers()
		if len(cws) == 0 {
			m.renderLocalNote(`no co-working models configured (see README "Co-working models")`)
			return m, nil
		}
		lines := make([]string, 0, len(cws))
		for _, cw := range cws {
			line := "  " + m.st.Cowork.Render(cw.Name) + "  " + m.st.Dim.Render(cw.Provider+"/"+cw.Model)
			if cw.Skills != "" {
				line += "  " + cw.Skills
			}
			if cw.Online {
				line += m.st.Warn.Render(" (online)")
			}
			if n := m.ag.ConsultCount(cw.Name); n > 0 {
				line += m.st.Dim.Render(fmt.Sprintf(" · consulted %d", n))
			}
			lines = append(lines, line)
		}
		m.renderLocalLines(lines)
		return m, nil
	case "/consult":
		who, q := "", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
		// The first word is a co-worker name only when it names one that
		// exists; otherwise it is the first word of the question, so
		// "/consult why is this failing?" still reaches the default one.
		if len(fields) > 1 {
			for _, cw := range m.ag.Coworkers() {
				if cw.Name == fields[1] {
					who, q = cw.Name, strings.TrimSpace(strings.TrimPrefix(q, fields[1]))
					break
				}
			}
		}
		if q == "" {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "usage: /consult [name] <question>"})
			return m, nil
		}
		// A turn of its own when the session is idle: every terminal goes
		// busy, and Esc reaches the consultation through cancelFn. The
		// question and the answer are the events' to render
		// (onConsultStart/onConsultEnd), for every terminal at once.
		//
		// /consult is busy-safe, though, so it can also be asked *during* a
		// run — the nesting plain mode allows too. That one must not take the
		// turn over: runContextLocked would park its own cancel in cancelFn,
		// orphaning the run in flight (Esc would reach the consultation and
		// nothing else), and finishTurn would return the session to idle
		// while the model was still working. So it borrows the session's root
		// context and leaves the run state alone; Esc means the run and, through
		// consultCancel, the consultation too.
		sess, label := m.Session, m.clientLabel(m.id)
		owns := !m.running
		var ctx context.Context
		if owns {
			m.setRunStateLocked(true, "consulting")
			ctx = m.runContextLocked()
		} else {
			parent := m.rootCtx
			if parent == nil {
				parent = context.Background()
			}
			if m.consultCancel != nil {
				// Consult would refuse a second one anyway; say so here
				// rather than let its cancel get lost.
				m.appendEntryLocked(entry{Kind: entryError, Label: "error ", Text: "a consultation is already running"})
				return m, nil
			}
			ctx, m.consultCancel = context.WithCancel(parent)
		}
		go func() {
			// No Recent: RecentContext reads fields only the agent's own
			// goroutine may touch, and this one is a UI goroutine. A person
			// asking directly says what they mean anyway.
			res, err := sess.ag.Consult(ctx, agent.ConsultRequest{Who: who, Question: q, Origin: "user:" + label})
			// A consultation that got as far as running has reported itself
			// through the events; only the refusals before it started
			// (unknown name, declined, none configured) reach no event.
			if err != nil && !res.Started {
				sess.appendEntry(entry{Kind: entryError, Label: "error ", Text: err.Error()})
			}
			if owns {
				sess.send(sess.usageSnapshot())
				sess.finishTurn(nil, nil)
				return
			}
			sess.mu.Lock()
			if sess.consultCancel != nil {
				sess.consultCancel()
				sess.consultCancel = nil
			}
			sess.mu.Unlock()
		}()
		return m, nil
	case "/task":
		if m.ag.Engine == nil {
			m.renderLocalNote("working memory is off (engine.enabled)")
			return m, nil
		}
		var args []string
		if len(fields) > 1 {
			args = fields[1:]
		}
		switch {
		case len(args) > 0 && args[0] == "clear":
			m.ag.Engine.ClearSession()
			m.renderLocalNote("this session's open work is closed as dropped (reason: cleared); the task documents are untouched")
		case len(args) > 0 && args[0] == "open":
			if m.ag.Engine.HasTaskDocuments() {
				m.renderLocalNote(m.ag.Engine.TasksDir())
			} else {
				m.renderLocalNote("no task documents yet; the first one will be written to " + m.ag.Engine.TasksDir())
			}
		default:
			m.renderLocalLines(ui.TaskLines(m.ag.Engine, args))
		}
		return m, nil
	case "/notes":
		if m.ag.Engine == nil {
			m.renderLocalNote("working memory is off (engine.enabled)")
			return m, nil
		}
		sub := ""
		if len(fields) > 1 {
			sub = fields[1]
		}
		switch sub {
		case "add":
			text := ui.NoteArgument(text)
			if text == "" {
				m.renderLocalNote("usage: /notes add <text>")
				return m, nil
			}
			m.ag.Engine.AddNoteLine(text)
			m.renderLocalNote("noted")
		case "drop":
			if len(fields) < 3 {
				m.renderLocalNote("usage: /notes drop N")
				break
			}
			n, _ := strconv.Atoi(strings.Join(fields[2:], ""))
			if err := m.ag.Engine.DropNote(n); err != nil {
				m.renderLocalNote("error: " + err.Error())
			}
		case "clear":
			m.ag.Engine.ClearNotes()
			m.renderLocalNote("notes cleared")
		default:
			notes := strings.TrimRight(m.ag.Engine.Notes(), "\n")
			if notes == "" {
				m.renderLocalNote("no notes")
				return m, nil
			}
			var lines []string
			for i, l := range strings.Split(notes, "\n") {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, l))
			}
			m.renderLocalLines(lines)
		}
		return m, nil
	case "/clients":
		if !m.served {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "not served: this session is running in-process (start without --no-host to allow attach)"})
			return m, nil
		}
		if len(m.clients) == 0 {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "no terminals attached"})
			return m, nil
		}
		for _, c := range m.clients {
			m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("  %s  %dx%d", c.Label, c.Cols, c.Rows)})
		}
		return m, nil
	case "/detach":
		if m.detachClient == nil {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "nothing to detach: not served"})
			return m, nil
		}
		// As a command, not a call: detaching makes the host notify its
		// callbacks, which p.Send messages to this program — and Update runs
		// on the goroutine that receives them, so calling the host here would
		// deadlock the session for good (holding the host's notifyMu, so no
		// later attach or `sessions kill` could recover it).
		detach, id := m.detachClient, m.id
		return m, func() tea.Msg { detach(id); return nil }
	case "/sessions", "/resume":
		if fields[0] == "/resume" && len(fields) > 1 {
			return m.resumeFrom(fields[1], m.id)
		}
		return m, m.askSessionPicker()
	default:
		if c, ok := m.custom[strings.TrimPrefix(fields[0], "/")]; ok {
			args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
			m.Submit(c.Expand(args), m.id)
			return m, nil
		}
		m.appendEntryLocked(entry{Kind: entryErr, Text: "unknown command " + fields[0] + " (/help)"})
	}
	return m, nil
}

func (m *View) setProvider(name string) (tea.Model, tea.Cmd) {
	p, err := provider.FromConfig(m.cfg, name)
	if err != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
		return m, nil
	}
	m.prov = p
	m.ag.Provider = p
	m.ag.SetModel(provider.ResolveModel(m.cfg, name, ""))
	m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("provider set to %s (model %s)", name, m.ag.Model)})
	return m, m.pingCmd()
}

// resumeFrom loads a saved session into this program — unless that session
// already has a host running somewhere, in which case it is joined, never
// forked: two programs on one session file are blind to each other's turns
// and overwrite each other's saves. from is the terminal that asked, which
// is the one handed over to the live host.
func (m *View) resumeFrom(id string, from int) (tea.Model, tea.Cmd) {
	// The live registry answers before the store: a host's session exists
	// in memory before its first autosave, so a live code may have no file
	// yet (the same order as the launcher's decideStart).
	if code := strings.ToUpper(strings.TrimSpace(id)); m.liveCodes()[code] && !m.ownCode(code) {
		return m.joinLive(code, from)
	}
	s, err := m.loadSession(id)
	if err != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
		return m, nil
	}
	code := s.ResumeCode()
	if m.liveCodes()[code] && !m.ownCode(code) {
		return m.joinLive(code, from)
	}
	m.ag.Resume(s)
	m.seedResumeLocked(s)
	return m, nil
}

// ownCode reports whether code is this program's own session, which is live
// by definition; resuming it is a no-op, not a switch to itself.
func (m *View) ownCode(code string) bool {
	return m.ag.Session != nil && m.ag.Session.ResumeCode() == code
}

// joinLive hands from's terminal to the host of a live code instead of
// loading a second copy of its session.
func (m *View) joinLive(code string, from int) (tea.Model, tea.Cmd) {
	if m.served && m.switchClient != nil {
		m.switchPending = true
		// As a command, not a call: the host notifies its callbacks, which
		// p.Send into the channel this goroutine receives from (see /detach).
		sw, c := m.switchClient, from
		return m, func() tea.Msg { sw(c, code); return nil }
	}
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("%s is live elsewhere; join it with: be-code attach %s", code, code)})
	return m, nil
}
