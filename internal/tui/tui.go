// Package tui implements BE-Code's full-screen terminal UI on the
// Charmbracelet stack: streaming transcript, multi-line input, status bar,
// approval modals with diffs, and interactive pickers.
package tui

import (
	"context"
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
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/ui"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// ---- styles (reassigned by SetTheme) ---------------------------------------

var (
	stAccent  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	stDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	stTool    = lipgloss.NewStyle().Foreground(lipgloss.Color("44"))
	stErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	stOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	stUser    = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	stStatus  = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("250")).Padding(0, 1)
	stModalTi = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	stBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

// ---- messages from the agent goroutine ------------------------------------

type deltaMsg string
type toolStartMsg struct{ name, args string }
type toolEndMsg struct {
	name string
	res  tools.Result
}
type noticeMsg string
type thinkingMsg int // cumulative hidden-reasoning characters this turn
// statusMsg sets the bottom-line status note directly, without adding a
// transcript line (used for editor-side review progress).
type statusMsg string
type turnDoneMsg struct {
	rep *agent.ReviewedReport
	err error
}

// usageMsg carries a usage snapshot taken ON THE AGENT GOROUTINE at a
// moment the agent is quiescent (inside an event callback, or after a run
// returns). The UI renders only these cached numbers, so the status bar
// never reads agent state concurrently with the loop mutating it.
type usageMsg struct {
	ctxTokens, budget, total int
}
type planReadyMsg struct {
	req  string
	plan string
	err  error
}
type approvalMsg struct {
	action, detail string
	resp           chan bool
}

// mode is the input routing state.
type mode int

const (
	modeInput mode = iota
	modeBusy
	modeApproval
	modePicker
	modePlan
	modePalette     // "/" command popup above the input
	modeMenu        // full-screen grouped menu (/menu)
	modeContextMenu // right-click copy/paste popup
)

// Model is the bubbletea model for the whole app.
type Model struct {
	cfg      *config.Config
	ag       *agent.Agent
	prov     provider.Provider
	program  *tea.Program // set after construction for goroutine sends
	rootCtx  context.Context
	cancelFn context.CancelFunc

	vp       viewport.Model
	input    textarea.Model
	spin     spinner.Model
	mode     mode
	width    int
	height   int
	ready    bool
	quitHint bool

	transcript strings.Builder // finished content
	streaming  strings.Builder // current assistant text
	statusNote string

	approval *approvalMsg
	modalVP  viewport.Model

	picker  *picker
	pending *planReadyMsg // approved-plan-awaiting-decision
	custom  map[string]commands.Command

	histFile   *inputHistory
	richText   bool     // markdown/syntax rendering enabled
	usage      usageMsg // cached usage for the wheel and menu (see usageMsg)
	wheelFrame int      // rotation frame while busy

	// Selection and clipboard (see selection.go, clipboard.go).
	sel            *selection
	wrapped        string // the wrapped transcript the viewport shows
	lastReply      string // last assistant answer, plain text
	lastTool       string // last tool output, full
	running        bool   // a run is in progress (mode may be a popup)
	prevMode       mode
	clipboardWrite func(string) error
	clipboardRead  func() (string, error)
}

// New builds the TUI model.
func New(cfg *config.Config, ag *agent.Agent, prov provider.Provider) *Model {
	ta := textarea.New()
	ta.Placeholder = "describe a task…  (Enter sends · Ctrl+J newline · / for commands)"
	ta.SetHeight(3)
	ta.SetPromptFunc(5, func(lineIdx int) string {
		if lineIdx == 0 {
			return "(>): "
		}
		return "     "
	})
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Focus()
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = stAccent

	SetTheme(cfg.Theme)
	m := &Model{
		cfg: cfg, ag: ag, prov: prov,
		input: ta, spin: sp,
		histFile: loadInputHistory(ui.HistoryFile()),
		custom:   commands.Load(ag.Tools.Root),
		richText: cfg.Theme != "mono",

		clipboardWrite: writeClipboard,
		clipboardRead:  readClipboard,
	}
	ag.Tools.Approve = m.approveFromAgent
	ag.Events = agent.Events{
		OnDelta: func(t string) { m.send(deltaMsg(t)) },
		OnToolStart: func(n, a string) {
			m.send(toolStartMsg{n, a})
			m.send(m.usageSnapshot())
		},
		OnToolEnd: func(n string, r tools.Result) {
			m.send(toolEndMsg{n, r})
			m.send(m.usageSnapshot())
		},
		OnNotice: func(s string) { m.send(noticeMsg(s)) },
		OnReasoning: func() func(string) {
			n, last := 0, 0
			return func(t string) {
				n += len(t)
				if n-last >= 200 { // throttle status updates
					last = n
					m.send(thinkingMsg(n))
				}
			}
		}(),
	}
	ag.Tools.OnStatus = func(s string) { m.send(statusMsg(s)) }
	m.usage = m.usageSnapshot() // pre-run, single-threaded: safe
	return m
}

// usageSnapshot must be called only where the agent loop is quiescent:
// before the program starts, inside agent event callbacks, or after a run
// returns on its goroutine.
func (m *Model) usageSnapshot() usageMsg {
	return usageMsg{
		ctxTokens: m.ag.History.Tokens(),
		budget:    m.ag.History.Limit(),
		total:     m.ag.Stats.PromptTokens + m.ag.Stats.CompletionTokens,
	}
}

// Run starts the program (alt screen) and blocks until exit.
func (m *Model) Run(ctx context.Context) error {
	m.rootCtx = ctx
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.program = p
	_, err := p.Run()
	m.histFile.save()
	return err
}

func (m *Model) send(msg tea.Msg) {
	if m.program != nil {
		m.program.Send(msg)
	}
}

// approveFromAgent bridges the agent goroutine into the UI event loop.
func (m *Model) approveFromAgent(action, detail string) bool {
	if action == "shell" && m.cfg.AutoApproveShell {
		return true
	}
	if action == "file_write" && !m.cfg.ApproveFileWrites {
		return true
	}
	resp := make(chan bool, 1)
	m.send(approvalMsg{action: action, detail: detail, resp: resp})
	return <-resp
}

// ---- tea.Model -------------------------------------------------------------

func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spin.Tick, m.pingCmd())
}

func (m *Model) pingCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.prov.Ping(m.rootCtx)
		if err != nil {
			return noticeMsg(fmt.Sprintf("backend %s unreachable: %v", m.prov.Name(), err))
		}
		return noticeMsg(fmt.Sprintf("backend %s: %s", m.prov.Name(), status))
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sel = nil // columns no longer line up after a rewrap
		m.layout()
		m.ready = true
		m.refreshTranscript()
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
	case deltaMsg:
		m.streaming.WriteString(string(msg))
		m.refreshTranscript()
	case toolStartMsg:
		m.flushStreaming()
		args := msg.args
		if len(args) > 140 {
			args = args[:140] + "…"
		}
		m.appendLine(stTool.Render("● "+msg.name) + " " + stDim.Render(args))
		m.statusNote = "running " + msg.name
	case toolEndMsg:
		m.lastTool = msg.res.Content
		if msg.res.IsError {
			first := strings.SplitN(msg.res.Content, "\n", 2)[0]
			m.appendLine(stErr.Render("  ✗ ") + stDim.Render(first))
		} else {
			first := strings.SplitN(msg.res.Content, "\n", 2)[0]
			if len(first) > 100 {
				first = first[:100] + "…"
			}
			m.appendLine(stOK.Render("  ✓ ") + stDim.Render(first))
		}
		m.statusNote = "thinking"
	case thinkingMsg:
		if m.mode == modeBusy {
			m.statusNote = fmt.Sprintf("thinking (%dk chars of reasoning)", int(msg)/1000)
		}
	case noticeMsg:
		m.flushStreaming()
		m.appendLine(stWarn.Render("note ") + string(msg))
	case statusMsg:
		m.statusNote = string(msg)
		if m.statusNote == "" && m.running {
			m.statusNote = "thinking"
		}
	case approvalMsg:
		msgCopy := msg
		m.approval = &msgCopy
		m.mode = modeApproval
		m.modalVP = viewport.New(m.width-6, m.modalHeight())
		m.modalVP.SetContent(ui.ColorizeDiff(msg.detail, true))
	case turnDoneMsg:
		m.flushStreaming()
		if msg.err != nil {
			if msg.err != context.Canceled && !strings.Contains(msg.err.Error(), "context canceled") {
				m.appendLine(stErr.Render("error ") + msg.err.Error())
			} else {
				m.appendLine(stWarn.Render("cancelled"))
			}
		}
		if msg.rep != nil && msg.rep.Verify != nil {
			for _, line := range strings.Split(msg.rep.Verify.Human(), "\n") {
				m.appendLine(stDim.Render(line))
			}
			if msg.rep.Verify.Passed() {
				m.appendLine(stOK.Render("✓ verified"))
			} else {
				m.appendLine(stErr.Render("✗ verification failed after repairs"))
			}
		}
		if msg.rep != nil && msg.rep.Reviewed {
			if msg.rep.ReviewIssues == "" {
				m.appendLine(stOK.Render("✓ reviewer approved"))
			} else {
				m.appendLine(stWarn.Render("reviewer raised issues (repair attempted)"))
			}
		}
		m.appendLine("")
		m.running = false
		if m.mode == modeBusy {
			m.mode = modeInput
		}
		m.statusNote = ""
		m.input.Focus()
		// Anything queued during the run that the model never got to see
		// becomes the next turn.
		if left := m.ag.DrainInbox(); len(left) > 0 {
			return m.startTurn(strings.Join(left, "\n"))
		}
	case planReadyMsg:
		m.flushStreaming()
		if msg.err != nil {
			m.appendLine(stErr.Render("plan failed: ") + msg.err.Error())
			m.mode = modeInput
			m.input.Focus()
			break
		}
		msgCopy := msg
		m.pending = &msgCopy
		m.mode = modePlan
		m.modalVP = viewport.New(m.width-6, m.modalHeight())
		m.modalVP.SetContent(msg.plan)
	case usageMsg:
		m.usage = msg
	case pickerItemsMsg:
		m.pickerUpdate(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	}

	if m.mode == modeInput {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeApproval:
		return m.handleApprovalKey(k)
	case modePicker:
		return m.handlePickerKey(k)
	case modePlan:
		return m.handlePlanKey(k)
	case modePalette:
		return m.handlePaletteKey(k)
	case modeMenu:
		return m.handleMenuKey(k)
	case modeContextMenu:
		return m.handleContextMenuKey(k)
	case modeBusy:
		return m.handleBusyKey(k)
	}

	// modeInput
	switch k.Type {
	case tea.KeyCtrlC:
		if m.sel != nil {
			m.copyText(m.selectionText(), "selection")
			m.clearSelection()
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.Reset()
			m.quitHint = false
			return m, nil
		}
		if m.quitHint {
			return m, tea.Quit
		}
		m.quitHint = true
		m.appendLine(stDim.Render("press Ctrl+C again to quit"))
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.quitHint = false
		m.histFile.add(text)
		m.input.Reset()
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text)
		}
		return m.startTurn(text)
	case tea.KeyTab:
		m.completeSlash()
		return m, nil
	case tea.KeyEsc:
		if m.sel != nil {
			m.clearSelection()
			return m, nil
		}
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == '/' && strings.TrimSpace(m.input.Value()) == "" {
			return m.openPalette("")
		}
	case tea.KeyUp:
		if m.input.LineCount() <= 1 {
			if prev, ok := m.histFile.prev(); ok {
				m.input.SetValue(prev)
				m.input.CursorEnd()
			}
			return m, nil
		}
	case tea.KeyDown:
		if m.input.LineCount() <= 1 {
			next, _ := m.histFile.next()
			m.input.SetValue(next)
			m.input.CursorEnd()
			return m, nil
		}
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m *Model) handleApprovalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(k.String()) {
	case "y":
		m.resolveApproval(true, "")
	case "n", "esc":
		m.resolveApproval(false, "")
	case "a":
		if m.approval.action == "shell" {
			m.cfg.AutoApproveShell = true
			m.resolveApproval(true, "shell auto-approve enabled for this session")
		} else {
			m.cfg.ApproveFileWrites = false
			m.resolveApproval(true, "file-write previews disabled for this session")
		}
	default:
		var cmd tea.Cmd
		m.modalVP, cmd = m.modalVP.Update(k)
		return m, cmd
	}
	return m, nil
}

func (m *Model) resolveApproval(ok bool, note string) {
	if m.approval == nil {
		return
	}
	verdict := stErr.Render("denied")
	if ok {
		verdict = stOK.Render("approved")
	}
	m.appendLine(stWarn.Render(m.approval.action+" ") + verdict)
	if note != "" {
		m.appendLine(stDim.Render(note))
	}
	m.approval.resp <- ok
	m.approval = nil
	m.mode = modeBusy
}

// startTurn launches the agent in a goroutine.
func (m *Model) startTurn(text string) (tea.Model, tea.Cmd) {
	m.appendLine(stUser.Render("you> ") + text)
	m.mode = modeBusy
	m.running = true
	m.statusNote = "thinking"
	m.input.Placeholder = "type to queue a message for the agent…  (Enter queues · Esc cancels)"
	root := m.rootCtx
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	m.cancelFn = cancel
	go func() {
		_, rep, err := m.ag.RunFull(ctx, text)
		m.send(m.usageSnapshot()) // run finished; agent quiescent
		m.send(turnDoneMsg{rep: rep, err: err})
	}()
	return m, m.wheelTick()
}

// handleBusyKey: while the agent works the input stays live. Enter queues
// the text for delivery at the model's next call; Esc/Ctrl-C cancels the
// run and discards the queue; PgUp/PgDn scroll; everything else edits.
func (m *Model) handleBusyKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
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
			m.appendLine(stDim.Render(fmt.Sprintf("discarded %d queued message(s)", n)))
		}
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		if strings.HasPrefix(text, "/") {
			m.appendLine(stDim.Render("commands wait until the agent is done (Esc cancels); plain text is queued"))
			return m, nil
		}
		m.histFile.add(text)
		m.ag.Enqueue(text)
		m.appendLine(stDim.Render("queued (delivered at the next step)> ") + text)
		return m, nil
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// handlePlanKey resolves the plan-approval modal.
func (m *Model) handlePlanKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.pending
	if p == nil {
		m.mode = modeInput
		return m, nil
	}
	switch strings.ToLower(k.String()) {
	case "y":
		m.pending = nil
		m.appendLine(stOK.Render("plan approved — executing"))
		m.mode = modeBusy
		m.statusNote = "executing plan"
		ctx, cancel := context.WithCancel(m.rootCtx)
		m.cancelFn = cancel
		go func() {
			_, rep, err := m.ag.ExecutePlan(ctx, p.req, p.plan)
			m.send(m.usageSnapshot())
			m.send(turnDoneMsg{rep: rep, err: err})
		}()
	case "n", "esc":
		m.pending = nil
		m.appendLine(stWarn.Render("plan discarded"))
		m.mode = modeInput
		m.input.Focus()
	default:
		var cmd tea.Cmd
		m.modalVP, cmd = m.modalVP.Update(k)
		return m, cmd
	}
	return m, nil
}

// ---- transcript helpers ----------------------------------------------------

func (m *Model) appendLine(s string) {
	m.transcript.WriteString(s)
	m.transcript.WriteString("\n")
	m.refreshTranscript()
}

func (m *Model) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	text := strings.TrimRight(m.streaming.String(), "\n")
	m.lastReply = text
	m.transcript.WriteString(ui.RenderMarkdown(text, m.richText))
	m.transcript.WriteString("\n")
	m.streaming.Reset()
	m.refreshTranscript()
}

func (m *Model) refreshTranscript() {
	if !m.ready {
		return
	}
	content := m.transcript.String() + m.streaming.String()
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

func (m *Model) showHeader() bool { return m.height >= headerMinRows }

func (m *Model) headerHeight() int {
	if m.showHeader() {
		return 5 // 3-row title box, attribution line, dashed rule
	}
	return 0
}

func (m *Model) layout() {
	inputH := 3
	bottomH := 1
	vpH := m.height - m.headerHeight() - inputH - bottomH - 1
	if vpH < 3 {
		vpH = 3
	}
	m.vp.Width = m.width
	m.vp.Height = vpH
	m.input.SetWidth(m.width - wheelWidth - 2)
}

func (m *Model) modalHeight() int {
	h := m.height - 8
	if h < 5 {
		h = 5
	}
	return h
}

// ---- view ------------------------------------------------------------------

func (m *Model) View() string {
	if !m.ready {
		return "loading…"
	}
	switch m.mode {
	case modeApproval:
		return m.viewApproval()
	case modePicker:
		return m.viewPicker()
	case modeMenu:
		return m.viewMenu()
	case modePlan:
		body := stBorder.Width(m.width - 4).Render(
			stModalTi.Render("Implementation plan — approve to execute") + "\n\n" + m.modalVP.View())
		return body + "\n" + stDim.Render(" y execute · n discard · ↑↓ scroll")
	}

	var b strings.Builder
	if m.showHeader() {
		b.WriteString(m.headerView())
		b.WriteString("\n")
	}
	transcript := m.vp.View()
	if (m.mode == modePalette || m.mode == modeContextMenu) && m.picker != nil {
		// Popups sit over the bottom of the transcript, right above the
		// input; the newest lines stay visible above them.
		box := m.paletteBox()
		if m.mode == modeContextMenu {
			box = m.contextMenuBox()
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
	b.WriteString(transcript)
	b.WriteString("\n")
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, m.input.View(), " "+m.wheelView()))
	b.WriteString("\n")
	b.WriteString(m.bottomLine())
	return b.String()
}

// headerView draws the branded header: boxed title, logo at the right,
// attribution, dashed rule.
func (m *Model) headerView() string {
	box := stBorder.Render("BE-Code Redux")
	lines := strings.Split(box, "\n")
	logo := stAccent.Render("⚛")
	if len(lines) >= 2 {
		gap := m.width - lipgloss.Width(lines[1]) - lipgloss.Width(logo) - 1
		if gap < 1 {
			gap = 1
		}
		lines[1] += strings.Repeat(" ", gap) + logo
	}
	attribution := stDim.Render("  2026 BE AI Research · https://github.com/BE-AI-Research - " + PublicVersion)
	rule := stDim.Render(strings.Repeat("- ", m.width/2))
	return strings.Join(lines, "\n") + "\n" + attribution + "\n" + rule
}

// bottomLine is the single row under the input: menu hint, model, state.
func (m *Model) bottomLine() string {
	state := stOK.Render("ready")
	if m.running {
		state = m.spin.View() + " " + m.statusNote + stDim.Render(" · Enter queues · Esc cancels")
	}
	line := " " + stAccent.Render("/menu") + " " + stAccent.Render("/help") +
		stDim.Render(" · "+shortModel(m.ag.Model)+" · ") + state
	if m.ag.IDEName != "" {
		line += stAccent.Render(" ⌘ ide")
	}
	if m.sel != nil {
		line += stDim.Render(" · selection: Ctrl+C copy · right-click menu · Esc clear")
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

func (m *Model) viewApproval() string {
	title := "Shell command"
	hint := "y approve · n deny · a always-approve shell · ↑↓ scroll"
	if m.approval != nil && m.approval.action == "file_write" {
		title = "File change"
		hint = "y approve · n deny · a stop asking for writes · ↑↓ scroll"
	}
	body := stBorder.Width(m.width - 4).Render(
		stModalTi.Render(title+" — approval required") + "\n\n" + m.modalVP.View())
	return body + "\n" + stDim.Render(" "+hint)
}

// ---- slash commands --------------------------------------------------------

func (m *Model) completeSlash() {
	v := m.input.Value()

	// @path completion on the last token.
	if i := strings.LastIndex(v, "@"); i >= 0 && !strings.ContainsAny(v[i:], " \n") {
		prefix := v[i+1:]
		matches := agent.CompleteMention(m.ag.Tools.Root, prefix)
		if len(matches) == 1 {
			m.input.SetValue(v[:i+1] + matches[0])
			m.input.CursorEnd()
		} else if len(matches) > 1 {
			m.appendLine(stDim.Render("@" + strings.Join(matches, "  @")))
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
		m.input.SetValue(matches[0] + " ")
		m.input.CursorEnd()
	} else if len(matches) > 1 {
		m.appendLine(stDim.Render(strings.Join(matches, "  ")))
	}
}

func (m *Model) slashCommand(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	switch fields[0] {
	case "/quit", "/exit", "/q":
		return m, tea.Quit
	case "/menu":
		return m.openMenu()
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
		m.appendLine(stDim.Render(help))
	case "/clear":
		m.ag.History.Messages = nil
		m.ag.Session = store.NewSession(m.prov.Name(), m.ag.Model, m.ag.Tools.Root)
		m.appendLine(stOK.Render("history cleared; new session started"))
	case "/tools":
		m.appendLine(stDim.Render(strings.Join(m.ag.Tools.Names(), " · ")))
	case "/config":
		p, _ := config.Path()
		m.appendLine(stDim.Render(fmt.Sprintf(
			"%s\nprovider=%s model=%s ui=%s ctx=%d max_turns=%d max_repairs=%d compat=%s previews=%v",
			p, m.cfg.DefaultProvider, m.cfg.Model, m.cfg.UI, m.cfg.ContextTokens,
			m.cfg.MaxTurns, m.cfg.MaxRepairs, m.cfg.CompatToolCalls, m.cfg.ApproveFileWrites)))
	case "/verify":
		m.mode = modeBusy
		m.statusNote = "verifying"
		go func() {
			proj := verify.Detect(m.ag.Tools.Root)
			rep := verify.RunChecks(m.rootCtx, m.ag.Tools.Root, proj)
			m.send(turnDoneMsg{rep: &agent.ReviewedReport{Verify: rep}})
		}()
	case "/model":
		if len(fields) > 1 {
			m.ag.SetModel(fields[1])
			m.appendLine(stOK.Render(fmt.Sprintf("model set to %s (profile %s)", fields[1], m.ag.Profile.Family)))
			break
		}
		return m.openModelPicker()
	case "/undo":
		restored, err := m.ag.Undo()
		if err != nil {
			m.appendLine(stErr.Render(err.Error()))
			break
		}
		m.appendLine(stOK.Render(fmt.Sprintf("restored: %s (%d undo levels left)",
			strings.Join(restored, ", "), m.ag.Checkpoints.Depth())))
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
			m.send(turnDoneMsg{})
		}()
	case "/init":
		return m.startTurn(agent.InitPrompt)
	case "/compact":
		m.mode = modeBusy
		m.statusNote = "compacting"
		go func() {
			if err := m.ag.Compact(m.rootCtx); err != nil {
				m.send(noticeMsg("compaction failed: " + err.Error()))
			} else {
				m.send(noticeMsg(fmt.Sprintf("compacted; context now ~%d tokens", m.ag.History.Tokens())))
			}
			m.send(turnDoneMsg{})
		}()
	case "/stats":
		s := m.ag.Stats
		m.appendLine(stDim.Render(fmt.Sprintf(
			"requests=%d tool_calls=%d prompt_tokens=%d completion_tokens=%d elapsed=%s ctx=%d/%d",
			s.Requests, s.ToolCalls, s.PromptTokens, s.CompletionTokens,
			s.Elapsed.Round(time.Second/10), m.ag.History.Tokens(), m.ag.History.Budget)))
	case "/map":
		if mp := m.ag.RepoMap(); mp == "" {
			m.appendLine(stDim.Render("no repo map (unrecognized files or disabled)"))
		} else {
			m.appendLine(stDim.Render(mp))
		}
	case "/plan":
		req := strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
		if req == "" {
			m.appendLine(stErr.Render("usage: /plan <task description>"))
			break
		}
		m.appendLine(stUser.Render("plan> ") + req)
		m.mode = modeBusy
		m.statusNote = "planning (read-only)"
		m.input.Blur()
		go func() {
			plan, err := m.ag.Plan(m.rootCtx, req)
			m.send(m.usageSnapshot())
			m.send(planReadyMsg{req: req, plan: plan, err: err})
		}()
	case "/models":
		return m.openModelPicker()
	case "/provider":
		if len(fields) > 1 {
			return m.setProvider(fields[1])
		}
		return m.openProviderPicker()
	case "/handoff":
		if h := m.ag.Handoff(); h != "" {
			for _, line := range strings.Split(h, "\n") {
				m.appendLine(stDim.Render(line))
			}
		} else {
			m.appendLine(stDim.Render("no handoff briefing in this session"))
		}
	case "/sessions", "/resume":
		if fields[0] == "/resume" && len(fields) > 1 {
			return m.resumeSession(fields[1])
		}
		return m.openSessionPicker()
	default:
		if c, ok := m.custom[strings.TrimPrefix(fields[0], "/")]; ok {
			args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
			return m.startTurn(c.Expand(args))
		}
		m.appendLine(stErr.Render("unknown command " + fields[0] + " (/help)"))
	}
	return m, nil
}

func (m *Model) setProvider(name string) (tea.Model, tea.Cmd) {
	p, err := provider.FromConfig(m.cfg, name)
	if err != nil {
		m.appendLine(stErr.Render(err.Error()))
		return m, nil
	}
	m.prov = p
	m.ag.Provider = p
	m.ag.SetModel(provider.ResolveModel(m.cfg, name, ""))
	m.appendLine(stOK.Render(fmt.Sprintf("provider set to %s (model %s)", name, m.ag.Model)))
	return m, m.pingCmd()
}

func (m *Model) resumeSession(id string) (tea.Model, tea.Cmd) {
	s, err := store.Load(id)
	if err != nil {
		m.appendLine(stErr.Render(err.Error()))
		return m, nil
	}
	m.ag.Resume(s)
	m.appendLine(stOK.Render(fmt.Sprintf("resumed %s — %s (%d messages)", s.ResumeCode(), s.Title, len(s.Messages))))
	if s.Handoff != "" {
		m.appendLine(stDim.Render("handoff briefing loaded into the system prompt; /handoff shows it"))
	}
	return m, nil
}
