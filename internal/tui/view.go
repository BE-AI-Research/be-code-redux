// Package tui implements BE-Code's full-screen terminal UI on the
// Charmbracelet stack: streaming transcript, multi-line input, status bar,
// approval modals with diffs, and interactive pickers.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/commands"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
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

type deltaMsg string
type toolStartMsg struct{ name, args string }
type toolEndMsg struct {
	name string
	res  tools.Result
}
type noticeMsg string

// transientMsg is a short-lived status notice (waiting for the backend,
// context budgeting): shown on the notice row for toastFor, never kept.
type transientMsg string

// toastTickMsg asks the view to expire the notice row if its time is up.
type toastTickMsg time.Time

const toastFor = 20 * time.Second

type thinkingMsg int // cumulative hidden-reasoning characters this turn
// statusMsg sets the bottom-line status note directly, without adding a
// transcript line (used for editor-side review progress).
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

	id      int    // the live client id this view renders for; 0 is the local terminal
	label   string // how that terminal names itself
	program *tea.Program

	st       styles // this terminal's theme; see theme.go
	richText bool   // markdown/syntax rendering enabled

	vp      viewport.Model
	modalVP viewport.Model
	spin    spinner.Model
	inputs  map[int]*textarea.Model // one input line per client; 0 is the local terminal

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

	width, height int
	ready         bool

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

	// quitHint remembers, per terminal, that this client's last key was a
	// Ctrl+C on an empty input: the second one quits. It is per client
	// because the confirmation belongs to the person who pressed it —
	// otherwise A's Ctrl+C plus B's unrelated Ctrl+C would end a session
	// neither of them asked to end.
	quitHint map[int]bool

	queueCursor  int // highlighted row in the queue popup
	queueOwner   int // client whose queue the popup is showing
	paletteOwner int // client that opened the "/" palette
	menuOwner    int // client that opened /menu or the right-click menu

	clipboardWrite func(string) error
	clipboardRead  func() (string, error)
	termWrite      func(string) // raw escape writer (terminal window colours); swappable for tests

	ascii bool // some attached client cannot show UTF-8 glyphs

	mb *mailbox // broadcasts from the session, waiting to be rendered here
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

// Update dispatches msg, then publishes overlays for whatever it changed.
// A mode transition that hides the input row (a full-screen modal —
// approval, plan, picker/menu — taking over the frame; see overlayVisible)
// clears every roster client's cached overlay at the host, not just
// whoever's key triggered it: Host.fanout.Write unconditionally re-appends
// a client's last overlay after every frame it writes (so an ordinary full
// repaint never erases a draft), and without clearing it first that stale
// draft would keep getting stamped, at its old input-row coordinates, over
// every render of the modal for as long as it stays open. The reverse
// transition (the modal closing back to a mode that renders the row)
// republishes the real rows for the whole roster the same way. Either
// transition can be driven by a key (approval y/n, plan y/n, picker/menu
// escape) or by a plain message, so this lives here rather than at each
// individual call site (askMsg opens one; pickerUpdate's load-error path
// closes one). Otherwise, a keystroke republishes only its
// sender — update's own cases (WindowSizeMsg, clientsMsg, and the
// modeInput tail loop) already republish everyone for their own triggers.
func (m *View) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wasVisible := m.overlayVisible()
	model, cmd := m.update(msg)
	nowVisible := m.overlayVisible()
	switch t := msg.(type) {
	case tea.KeyMsg:
		m.publishAfterKey(wasVisible, nowVisible, m.id)
	case live.ClientKeyMsg:
		m.publishAfterKey(wasVisible, nowVisible, t.Client)
	default:
		m.publishVisibilityChange(wasVisible, nowVisible)
	}
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

// publishAfterKey publishes the right set of overlays after a keystroke:
// clear/republish the whole roster if the key just flipped overlayVisible
// (see publishVisibilityChange), or just the sender otherwise.
func (m *View) publishAfterKey(wasVisible, nowVisible bool, client int) {
	if m.publishVisibilityChange(wasVisible, nowVisible) {
		return
	}
	m.publishOverlay(client)
}

// publishVisibilityChange clears every roster client's overlay if
// overlayVisible just went true→false (a modal opened), or republishes the
// real rows if it just went false→true (a modal closed). Reports whether
// either happened, so callers know not to do anything more granular of
// their own for this message.
func (m *View) publishVisibilityChange(wasVisible, nowVisible bool) bool {
	switch {
	case wasVisible && !nowVisible:
		m.clearAllOverlaysLocked()
		return true
	case !wasVisible && nowVisible:
		m.publishAllOverlays()
		return true
	}
	return false
}

func (m *View) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	enteredMode := m.mode
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sel = nil // columns no longer line up after a rewrap
		m.layout()
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
		m.rebuild()
		// A resize moves every input row's absolute position; republish for
		// the whole roster, not just whoever happens to type next.
		m.publishAllOverlays()
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
		m.streaming.WriteString(string(msg))
		m.refreshTranscript()
	case toolStartMsg:
		m.flushStreamingLocked()
		m.appendEntryLocked(entry{Kind: entryTool, Label: msg.name, Text: msg.args})
		m.statusNote = "running " + msg.name
	case toolEndMsg:
		m.lastTool = msg.res.Content
		if msg.res.IsError {
			first := strings.SplitN(msg.res.Content, "\n", 2)[0]
			m.appendEntryLocked(entry{Kind: entryToolErr, Text: first})
		} else {
			first := strings.SplitN(msg.res.Content, "\n", 2)[0]
			if len(first) > 100 {
				first = first[:100] + "…"
			}
			m.appendEntryLocked(entry{Kind: entryToolOK, Text: first})
		}
		m.statusNote = "thinking"
	case thinkingMsg:
		if m.mode == modeBusy {
			m.statusNote = fmt.Sprintf("thinking (%dk chars of reasoning)", int(msg)/1000)
		}
	case transientMsg:
		m.toast = string(msg)
		m.toastUntil = m.now().Add(toastFor)
		return m, tea.Tick(toastFor+100*time.Millisecond, func(t time.Time) tea.Msg { return toastTickMsg(t) })
	case toastTickMsg:
		if m.toast != "" && !m.now().Before(m.toastUntil) {
			m.toast = ""
		}
		return m, nil
	case noticeMsg:
		m.flushStreamingLocked()
		// The editor context note is ambient information, not a warning:
		// render it dimmed and unlabelled.
		if strings.HasPrefix(string(msg), "[editor:") {
			m.appendEntryLocked(entry{Kind: entryDim, Text: string(msg)})
		} else {
			m.appendEntryLocked(entry{Kind: entryNote, Text: string(msg)})
		}
	case statusMsg:
		m.statusNote = string(msg)
		if m.statusNote == "" && m.running {
			m.statusNote = "thinking"
		}
	case askMsg:
		m.showAsk(msg.a)
	case askResolvedMsg:
		// Someone answered, or the editor did and the coordinator withdrew
		// it. The terminal that answered has already closed its own modal.
		if m.mode == modeAsk && m.askShown == msg.gen {
			m.closeAsk()
			if note := answeredNote(msg.by, m.label); note != "" {
				m.renderLocalNote(note)
			}
		}
	case runStateMsg:
		if msg.running {
			for _, ta := range m.inputs {
				ta.Placeholder = busyPlaceholder
			}
			if m.mode == modeInput {
				m.mode = modeBusy
			}
			// The placeholder just changed for every client, not only the
			// one whose key started this turn.
			m.publishAllOverlays()
			cmds = append(cmds, m.wheelTick())
		} else {
			if m.mode == modeQueue {
				m.closeQueue()
			}
			if m.mode == modeBusy {
				m.mode = modeInput
			}
			m.focusInputs()
		}
	case usageMsg:
		m.usage = msg
	case clientsMsg:
		cmds = append(cmds, m.updateClients(msg))
		// A roster change can drop or add textareas; republish for whoever
		// remains (updateClients walks m.clients, so this never resurrects a
		// dropped client's textarea).
		m.publishAllOverlays()
	case idleTickMsg:
		return m.updateIdleTick(msg)
	case hostQuitMsg:
		m.clearAllOverlaysLocked() // see the /quit path
		return m, tea.Quit
	case pickerItemsMsg:
		m.pickerUpdate(msg)
	case tea.KeyMsg:
		// An untagged key is this terminal's own — in-process that is client
		// 0, and a per-terminal program's is its own client id, which is the
		// one an answer, a draft or a detach has to be recorded under.
		// Overlay publishing for the sender (or the roster, if this key
		// closed a modal) happens in Update, the exported wrapper around
		// this method — see publishAfterKey.
		return m.handleKey(msg, m.id)
	case live.ClientKeyMsg:
		return m.handleKey(msg.Key, msg.Client)
	case tea.MouseMsg:
		return m.handleMouse(msg, m.id)
	case live.ClientMouseMsg:
		return m.handleMouse(msg.Mouse, msg.Client)
	}

	if m.mode == modeInput {
		// Cursor blinks and the like are not tagged with a sender: every
		// client's input line gets them.
		for _, ta := range m.inputs {
			updated, cmd := ta.Update(msg)
			*ta = updated
			cmds = append(cmds, cmd)
		}
		// A non-key update can still change a textarea's rendered view (e.g.
		// a paste message that reached here rather than through handleKey);
		// republish so no client is left showing a stale draft. Cursors are
		// static in served mode (see newInputArea), so a genuine blink never
		// changes the view and this is not a per-blink publish. WindowSizeMsg
		// and clientsMsg already republished above, in their own cases; a
		// message that just switched the mode to modeInput from a hidden one
		// is Update's job (see publishAfterKey and publishVisibilityChange),
		// not this one, to avoid publishing the whole roster twice.
		switch msg.(type) {
		case tea.WindowSizeMsg, clientsMsg:
		default:
			if m.served && enteredMode == modeInput {
				m.publishAllOverlays()
			}
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// handleKey routes one keystroke, tagged with the client that typed it (0
// is the local terminal). Drafts, the palette and the queue popup belong to
// their sender; modals that speak for the whole session (approvals, plans,
// pickers) stay shared.
func (m *View) handleKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeAsk:
		return m.handleAskKey(k, from)
	case modePicker:
		return m.handlePickerKey(k, from)
	case modePalette:
		return m.handlePaletteKey(k, from)
	case modeMenu:
		return m.handleMenuKey(k, from)
	case modeContextMenu:
		return m.handleContextMenuKey(k, from)
	case modeQueue:
		return m.handleQueueKey(k, from)
	case modeBusy:
		return m.handleBusyKey(k, from)
	}
	return m.handleInputKey(k, from)
}

// handleGuestKey routes a key from a client that is *not* the owner of the
// popup currently on screen (the palette, /menu, the right-click menu, the
// queue popup — the ones that belong to whoever opened them).
//
// Dropping those keys, as this used to, deadens every other terminal's
// keyboard for as long as someone else browses a menu: not only their Esc
// and Ctrl+C but every ordinary letter they type. Instead the key goes to
// that client's own input line exactly as it would in the mode underneath —
// busy while a run is in progress, otherwise input — so they keep typing
// into their own draft (their overlay shows it) and their Esc/Ctrl+C acts
// on their own draft or selection, never on the owner's popup.
//
// The owner's popup stays open regardless: there is one m.mode and one
// m.picker for the whole session, so a guest's key is not allowed to change
// either. A guest key that would have opened a popup of its own (its own
// palette, its own queue) is therefore undone here rather than fighting for
// the screen — everything else it did (editing, submitting, queueing) has
// already happened. A turn a guest starts this way really does start; only
// the mode switch that would have hidden the owner's popup is rolled back,
// and the owner's own Esc lands them in m.idleMode(), which is modeBusy
// while that turn runs.
func (m *View) handleGuestKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	// Everything the popup is made of, put back exactly as it was once the
	// guest's key has had its effect on the guest's own draft. A guest key
	// that would have opened a popup of its own (its own palette, its own
	// queue) is undone this way rather than fighting for the one m.mode,
	// m.picker and owner the session has; everything else it did —
	// editing, submitting, queueing, cancelling a run — has already
	// happened and stands. A turn a guest starts really does start: only
	// the mode switch that would have hidden the owner's popup is rolled
	// back, and the owner's own Esc then lands in m.idleMode(), which is
	// modeBusy while that turn runs.
	// A slash command, though, is refused outright while the popup is up:
	// its work arrives later as a message (pickerItemsMsg, askMsg,
	// runStateMsg) that would rewrite or close whatever popup is open by
	// then — the owner's. Busy mode already refuses commands.
	if !m.running && k.Type == tea.KeyEnter && strings.HasPrefix(strings.TrimSpace(m.inputFor(from).Value()), "/") {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the open popup closes; plain text still sends"})
		return m, nil
	}
	mode, pick, prev := m.mode, m.picker, m.prevMode
	pal, menu, queue := m.paletteOwner, m.menuOwner, m.queueOwner
	cursor := m.queueCursor
	held := m.ag.Held()
	var model tea.Model
	var cmd tea.Cmd
	if m.running {
		model, cmd = m.handleBusyKey(k, from)
	} else {
		model, cmd = m.handleInputKey(k, from)
	}
	m.mode, m.picker, m.prevMode = mode, pick, prev
	m.paletteOwner, m.menuOwner, m.queueOwner = pal, menu, queue
	m.queueCursor = cursor
	if m.ag.Held() != held {
		// openQueue/closeQueue ran for a popup that is not going to be
		// shown: the owner's hold is what counts.
		m.ag.Hold(held)
	}
	return model, cmd
}

// setQuitHint arms one terminal's "press Ctrl+C again to quit", and
// clearQuitHint disarms it. Only the sender's own hint moves: a Ctrl+C from
// another terminal is about that terminal's draft, not this one's.
func (m *View) setQuitHint(from int) {
	if m.quitHint == nil {
		m.quitHint = map[int]bool{}
	}
	m.quitHint[from] = true
}

func (m *View) clearQuitHint(from int) {
	delete(m.quitHint, from)
}

// handleInputKey is modeInput: the key edits, submits or acts on the
// sender's own draft. Reached both from handleKey and, for a client that
// does not own the popup currently on screen, from handleGuestKey.
func (m *View) handleInputKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	in := m.inputFor(from)
	switch k.Type {
	case tea.KeyCtrlC:
		if m.sel != nil {
			m.copyText(m.selectionText(), "selection")
			m.clearSelection()
			return m, nil
		}
		if in.Value() != "" {
			in.Reset()
			m.clearQuitHint(from)
			return m, nil
		}
		if m.quitHint[from] {
			m.clearAllOverlaysLocked() // see the /quit path: no draft may follow the teardown frame
			return m, tea.Quit
		}
		m.setQuitHint(from)
		m.appendEntryLocked(entry{Kind: entryDim, Text: "press Ctrl+C again to quit"})
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(in.Value())
		if text == "" {
			return m, nil
		}
		m.clearQuitHint(from)
		m.histFile.add(text, from)
		in.Reset()
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text, from)
		}
		return m.startTurnFrom(text, from)
	case tea.KeyTab:
		m.completeSlash(from)
		return m, nil
	case tea.KeyEsc:
		if m.sel != nil {
			m.clearSelection()
			return m, nil
		}
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == '/' && strings.TrimSpace(in.Value()) == "" {
			return m.openPalette("", from)
		}
	case tea.KeyUp:
		if in.LineCount() <= 1 {
			if prev, ok := m.histFile.prev(from); ok {
				in.SetValue(prev)
				in.CursorEnd()
			}
			return m, nil
		}
	case tea.KeyDown:
		if in.LineCount() <= 1 {
			next, _ := m.histFile.next(from)
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
	m.focusInputs()
}

// answeredNote is the dimmed line a terminal that did *not* answer shows in
// its own buffer. A withdrawal passes the note itself ("answered in VS
// Code"); anything else is a client label. The answering terminal (and an
// empty by) gets nothing.
func answeredNote(by, label string) string {
	if by == "" || by == label {
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

// handleAskKey answers the shared question, or scrolls its body. from is the
// terminal that pressed the key: it is who the answer is recorded as, and
// the only one whose modal this closes directly (the rest close on the
// askResolvedMsg that Answer broadcasts).
func (m *View) handleAskKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	a := m.shownAsk
	if a == nil {
		m.mode = m.idleMode()
		m.picker = nil
		return m, nil
	}
	if a.Kind == askPicker {
		return m.handleAskPickerKey(k, from)
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
	cmd, ok := m.Answer(m.askShown, ans, from)
	if ok {
		m.closeAsk()
	}
	return m, cmd
}

// handleAskPickerKey drives a shared picker: the cursor and filter are this
// terminal's own, the row it confirms decides for the session.
func (m *View) handleAskPickerKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		p = &picker{title: m.shownAsk.Title, items: m.shownAsk.Items}
		m.picker = p
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		cmd, ok := m.Answer(m.askShown, askAnswer{}, from)
		if ok {
			m.closeAsk()
		}
		m.inputFor(from).Focus()
		return m, cmd
	case tea.KeyEnter:
		items := p.filtered()
		if len(items) == 0 {
			return m, nil
		}
		cmd, ok := m.Answer(m.askShown, askAnswer{OK: true, Note: items[p.cursor].id}, from)
		if ok {
			m.closeAsk()
		}
		return m, cmd
	}
	pickerNav(p, k)
	return m, nil
}

// startTurnFrom echoes the request under its sender's prefix and launches
// the agent.
func (m *View) startTurnFrom(text string, from int) (tea.Model, tea.Cmd) {
	m.appendEntryLocked(entry{Kind: entryUser, Label: m.userPrefix(from), Text: text})
	return m.startTurn(text)
}

// startTurn launches the agent. The caller has already echoed the request
// into the transcript. Everything about the run itself is the session's
// (startTurnLocked); what happens to this terminal's screen arrives, like
// every other terminal's, as the runStateMsg that broadcasts — which this
// Update drains before it returns, so the frame it draws is already busy.
func (m *View) startTurn(text string) (tea.Model, tea.Cmd) {
	m.startTurnLocked(text)
	return m, nil
}

// handleBusyKey: while the agent works the input stays live. Enter queues
// the text for delivery at the model's next call; Esc/Ctrl-C cancels the
// run and discards the queue; PgUp/PgDn scroll; everything else edits.
func (m *View) handleBusyKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	in := m.inputFor(from)
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
				m.histFile.add(text, from)
				return m.slashCommand(text, from)
			}
			m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the agent is done (Esc cancels); plain text is queued"})
			return m, nil
		}
		m.histFile.add(text, from)
		m.ag.EnqueueFrom(text, from)
		if len(m.clients) > 1 {
			m.appendEntryLocked(entry{Kind: entryQueued, Label: "queued> ", Text: m.userPrefix(from) + text})
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
			return m.openQueue(from)
		}
	case tea.KeyCtrlQ:
		return m.openQueue(from)
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == '/' && strings.TrimSpace(in.Value()) == "" {
			return m.openPalette("", from)
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
	content := m.rendered.String() + m.streaming.String()
	m.wrapped = lipgloss.NewStyle().Width(m.vp.Width).Render(content)
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
	for _, ta := range m.inputs {
		setInputPrompt(ta, m.compact())
		ta.SetWidth(m.inputWidth())
		ta.SetHeight(m.inputRows())
	}
}

// focusInputs refocuses every client's input line after a modal closes, and
// republishes every client's overlay since a focus change can alter what a
// textarea renders.
func (m *View) focusInputs() {
	for _, ta := range m.inputs {
		ta.Focus()
	}
	m.publishAllOverlays()
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
		// (and the served overlays anchored to them) never move.
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
	b.WriteString(m.bottomLine())
	return b.String()
}

// headerView draws the branded header: boxed title, logo at the right,
// attribution, dashed rule.
func (m *View) headerView() string {
	box := m.st.Border.Render("BE-Code Redux")
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
func shortModel(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if len(name) > 32 {
		name = name[:31] + "…"
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
	if a.Action == "file_write" {
		title = "File change"
		hint = "y approve · n deny · a stop asking for writes · ↑↓ scroll"
	}
	if m.compact() {
		hint = "y/n/a · ↑↓"
	}
	body := m.st.Border.Width(m.width - 4).Render(
		m.st.ModalTi.Render(title+" — approval required") + "\n\n" + m.modalVP.View())
	return body + "\n" + m.st.Dim.Render(" "+hint)
}

// ---- slash commands --------------------------------------------------------

func (m *View) completeSlash(from int) {
	in := m.inputFor(from)
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

// slashCommand runs a command typed by client from (0 is the local
// terminal): commands that fill an input line or act on a terminal need to
// know whose.
func (m *View) slashCommand(text string, from int) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	switch fields[0] {
	case "/quit", "/exit", "/q":
		if m.running && m.cancelFn != nil {
			m.cancelFn() // leaving mid-turn: stop the run, then write the briefing
		}
		// Clear the overlays before the quit, not only after the program
		// returns: Bubble Tea's own teardown flushes one last frame, and
		// the host would re-append every client's draft to it.
		m.clearAllOverlaysLocked()
		return m, tea.Quit
	case "/menu":
		return m.openMenu(from)
	case "/theme":
		if len(fields) > 1 {
			return m.applyTheme(strings.ToLower(fields[1]))
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
		m.mode = modeBusy
		m.statusNote = "verifying"
		go func() {
			proj := verify.Detect(m.ag.Tools.Root)
			rep := verify.RunChecks(m.rootCtx, m.ag.Tools.Root, proj)
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
		m.mode = modeBusy
		m.statusNote = "committing"
		go func() {
			line, err := m.ag.GenerateCommit(m.rootCtx)
			if err != nil {
				m.send(noticeMsg("commit failed: " + err.Error()))
			} else {
				m.send(noticeMsg("committed: " + line))
			}
			m.finishTurn(nil, nil)
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
				Log:     func(s string) { sess.send(noticeMsg(s)) },
			})
			sess.finishInit(path, err)
		}()
		return m, nil
	case "/compact":
		m.mode = modeBusy
		m.statusNote = "compacting"
		go func() {
			if err := m.ag.Compact(m.rootCtx); err != nil {
				m.send(noticeMsg("compaction failed: " + err.Error()))
			} else {
				m.send(noticeMsg(fmt.Sprintf("compacted; context now ~%d tokens", m.ag.History.Tokens())))
			}
			m.finishTurn(nil, nil)
		}()
	case "/stats":
		s := m.ag.Stats
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
				sess.appendEntry(entry{Kind: entryError, Label: "plan failed: ", Text: err.Error()})
				sess.finishTurn(nil, nil)
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
		detach, id := m.detachClient, from
		return m, func() tea.Msg { detach(id); return nil }
	case "/sessions", "/resume":
		if fields[0] == "/resume" && len(fields) > 1 {
			return m.resumeFrom(fields[1], from)
		}
		return m, m.askSessionPicker()
	default:
		if c, ok := m.custom[strings.TrimPrefix(fields[0], "/")]; ok {
			args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
			return m.startTurnFrom(c.Expand(args), from)
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
	m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("resumed %s — %s (%d messages)", s.ResumeCode(), s.Title, len(s.Messages))})
	if s.Handoff != "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "handoff briefing loaded into the system prompt; /handoff shows it"})
	}
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
