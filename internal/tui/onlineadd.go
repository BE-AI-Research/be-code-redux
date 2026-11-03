package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
)

type onlineStep int

const (
	stepPreset    onlineStep = iota // picker: presets + custom
	stepName                        // input line: custom name
	stepURL                         // input line: custom base URL
	stepMethod                      // picker: browser | paste (openrouter only)
	stepKey                         // masked field
	stepWait                        // validating, or waiting for the browser (pasted redirect accepted)
	stepWorkspace                   // input line: Anthropic workspace ID (a key not scoped to one)
	stepModel                       // picker over the listing, or typed when empty
	stepDefault                     // picker: yes | no
)

// onlineAddFlow is one terminal's /online add in progress. Everything a
// goroutine produces comes back as a message; the flow's own context is
// what Esc cancels.
type onlineAddFlow struct {
	step    onlineStep
	choice  setup.Choice
	note    string
	key     string
	source  string
	models  []provider.ModelInfo
	model   string
	secret  textinput.Model
	pasted  chan string
	ctx     context.Context
	cancel  context.CancelFunc
	browser bool
	status  string // this terminal's status line while the flow waits (bottomLine)
}

// The flow's messages name the flow that sent them: a validation or a
// sign-in still running when Esc cancelled its flow must not land in the
// next one.
type onlineValidatedMsg struct {
	f      *onlineAddFlow
	models []provider.ModelInfo
	err    error
}
type onlineSignedInMsg struct {
	f   *onlineAddFlow
	key string
	err error
}

// The network seams, swapped by the tests.
var (
	onlineValidate    = setup.Validate
	onlineSignIn      = signin.SignIn
	onlineOpenBrowser = signin.OpenBrowser
	onlineOpenPage    = signin.OpenPage
)

type signinOptions = signin.Options

// startOnlineAdd is /online add [name], also reached from the palette and
// the menu (both go through slashCommand). Not while a run
// is in flight (it ends in a provider switch), and one terminal at a time.
func (m *View) startOnlineAdd(name string) (tea.Model, tea.Cmd) {
	if m.running {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the agent is done (Esc cancels); plain text is queued"})
		return m, nil
	}
	s := m.Session
	// A terminal that detached mid-flow leaves the flag behind; it only
	// counts while that terminal is still attached.
	if s.onlineAdding && (s.onlineAdder != m.id || m.onlineAdd != nil) && s.viewByID(s.onlineAdder) != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: "another terminal is adding a provider"})
		return m, nil
	}
	s.onlineAdding, s.onlineAdder = true, m.id
	ctx, cancel := context.WithCancel(s.baseCtx())
	f := &onlineAddFlow{ctx: ctx, cancel: cancel, pasted: make(chan string, 1)}
	m.onlineAdd = f
	if name == "" {
		f.step = stepPreset
		return m.openFlowPicker("Add an online provider", presetRows(), func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			return v.onlineChose(it.id)
		})
	}
	return m.onlineChose(name)
}

// presetRows is the first screen: the quick sign-in rows (OpenRouter's
// real browser sign-in, then the providers people ask for by name, each
// opening its keys page), the remaining presets behind one row, and a
// custom endpoint.
func presetRows() []pickItem {
	var items []pickItem
	for _, p := range provider.QuickSignIn() {
		desc := "Open the API keys page, then paste a key"
		if p.KeysURL == "" {
			desc = "Sign in with browser (issues a key)"
		}
		items = append(items, pickItem{id: p.Name, label: p.Name, desc: desc})
	}
	var names []string
	for _, p := range provider.OtherPresets() {
		names = append(names, p.Name)
	}
	items = append(items, pickItem{id: "other", label: "other preset…", desc: strings.Join(names, ", ")})
	return append(items, pickItem{id: "custom", label: "custom OpenAI-compatible endpoint", desc: "a name and a base URL"})
}

// otherRows lists the presets the quick list leaves out.
func otherRows() []pickItem {
	var items []pickItem
	for _, p := range provider.OtherPresets() {
		items = append(items, pickItem{id: p.Name, label: p.Name, desc: p.Docs})
	}
	return items
}

// openFlowPicker opens a view-local picker whose rows are already known.
// They are set at once, so the flow never waits on a load to answer a pick;
// the returned load Cmd hands back the same rows.
func (m *View) openFlowPicker(title string, items []pickItem, onPick func(*View, pickItem) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	mm, cmd := m.openPicker(title, func() ([]pickItem, error) { return items, nil }, onPick)
	m.picker.items, m.picker.loading = items, false
	return mm, cmd
}

// closeFlowPicker leaves a picker the flow answered (handlePickerKey has
// already done this for a real keystroke; doing it twice is harmless).
func (m *View) closeFlowPicker() {
	if m.mode == modePicker {
		m.picker = nil
		m.mode = m.returnMode()
		m.input.Focus()
	}
}

func (m *View) onlineChose(id string) (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	if f == nil {
		return m, nil
	}
	m.closeFlowPicker()
	if id == "custom" {
		f.step = stepName
		m.appendEntryLocked(entry{Kind: entryDim, Text: "name for the new provider:"})
		return m, nil
	}
	if id == "other" {
		f.step = stepPreset
		return m.openFlowPicker("Other presets", otherRows(), func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			return v.onlineChose(it.id)
		})
	}
	ch, note, err := setup.ChoiceFor(m.cfg, id, "")
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	f.choice, f.note = ch, note
	if note != "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: note})
	}
	if id == "openrouter" {
		f.step = stepMethod
		return m.openFlowPicker("openrouter: how to sign in", []pickItem{
			{id: "browser", label: "Sign in with browser", desc: "OpenRouter issues a key for this machine"},
			{id: "paste", label: "Paste a key", desc: "from openrouter.ai/keys"},
		}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			if v.onlineAdd == nil {
				return v, nil
			}
			v.closeFlowPicker()
			if it.id == "browser" {
				return v.onlineBrowser()
			}
			return v.onlineAskKey()
		})
	}
	// A provider with a keys page gets it opened (or shown) before the
	// masked field: the "sign in" these providers can honestly offer.
	if pre, ok := provider.PresetByName(id); ok && pre.KeysURL != "" {
		opened := onlineOpenPage(pre.KeysURL)
		m.appendEntryLocked(entry{Kind: entryDim, Text: setup.KeysPageLine(pre.KeysURL, opened)})
	}
	return m.onlineAskKey()
}

func (m *View) onlineAskKey() (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	f.step = stepKey
	ti := textinput.New()
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.Prompt = fmt.Sprintf("key for %s: ", f.choice.Name)
	ti.Focus()
	f.secret = ti
	// Nothing typed before the key step may sit hidden under the masked
	// field and resurface after it.
	m.input.Reset()
	return m, nil
}

// onlineLine is the Enter handler's hook: true when the line belonged to
// the flow. The key step never reaches here (its field is separate); the
// name, URL, typed-model and pasted-redirect steps do. While a flow is
// open the input line is the flow's: nothing typed is submitted.
func (m *View) onlineLine(text string) (bool, tea.Cmd) {
	f := m.onlineAdd
	if f == nil {
		return false, nil
	}
	switch f.step {
	case stepName:
		if _, ok := provider.PresetByName(text); ok {
			m.appendEntryLocked(entry{Kind: entryErr, Text: text + " is a preset; a custom endpoint needs another name"})
			return true, nil
		}
		f.choice.Name = text
		f.step = stepURL
		m.appendEntryLocked(entry{Kind: entryDim, Text: "base URL (ending in /v1):"})
		return true, nil
	case stepWorkspace:
		id := strings.TrimSpace(text)
		if id == "" {
			return true, nil
		}
		if f.choice.Headers == nil {
			f.choice.Headers = map[string]string{}
		}
		f.choice.Headers[setup.WorkspaceHeader] = id
		return true, m.onlineValidateCmd()
	case stepURL:
		ch, note, err := setup.ChoiceFor(m.cfg, f.choice.Name, text)
		if err != nil {
			m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
			return true, nil
		}
		f.choice, f.note = ch, note
		_, _ = m.onlineAskKey()
		return true, nil
	case stepWait:
		if f.browser {
			select {
			case f.pasted <- text:
			default:
			}
		}
		return true, nil
	case stepModel:
		f.model = text
		return true, m.onlineAskDefault()
	}
	return true, nil
}

func (m *View) onlineKeyEntered() tea.Cmd {
	f := m.onlineAdd
	f.key = strings.TrimSpace(f.secret.Value())
	f.secret.SetValue("")
	if f.key == "" {
		return nil
	}
	f.source = "pasted"
	m.appendEntryLocked(entry{Kind: entryDim, Text: "key entered (hidden)"})
	return m.onlineValidateCmd()
}

// The flow's status is this terminal's alone: it lives on the flow and the
// bottom line reads it from there (withFlowStatus), never the shared
// Session.statusNote, which belongs to a run and is drawn only while one is.
func (m *View) setFlowStatus(f *onlineAddFlow, note string) { f.status = note }

func (m *View) clearFlowStatus(f *onlineAddFlow) { f.status = "" }

// withFlowStatus puts an open flow's status in place of "ready", or ahead
// of a run's status, on this terminal's bottom line.
func (m *View) withFlowStatus(state string) string {
	f := m.onlineAdd
	if f == nil || f.status == "" {
		return state
	}
	st := m.st.Accent.Render(f.status) + m.st.Dim.Render(" · Esc cancels")
	if m.running {
		return st + m.st.Dim.Render(" · ") + state
	}
	return st
}

func (m *View) onlineValidateCmd() tea.Cmd {
	f := m.onlineAdd
	f.step = stepWait
	m.setFlowStatus(f, "checking "+f.choice.Name+"…")
	ctx, ch, key := f.ctx, f.choice, f.key
	return func() tea.Msg {
		models, err := onlineValidate(ctx, ch, key)
		return onlineValidatedMsg{f: f, models: models, err: err}
	}
}

func (m *View) onlineBrowser() (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	f.step, f.browser, f.source = stepWait, true, "oauth"
	m.setFlowStatus(f, "waiting for the browser; or paste the redirect URL")
	ctx, pasted, s := f.ctx, f.pasted, m.Session
	show := func(u string) { s.notice("open this URL to sign in: " + u) }
	return m, func() tea.Msg {
		key, err := onlineSignIn(ctx, signinOptions{Open: onlineOpenBrowser, Show: show, Pasted: pasted})
		return onlineSignedInMsg{f: f, key: key, err: err}
	}
}

// onlineMsg handles the flow's own messages; update routes them here. A
// message from a flow that has since ended is swallowed.
func (m *View) onlineMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case onlineSignedInMsg:
		f := m.onlineAdd
		if f == nil || msg.f != f {
			return m, nil, true
		}
		m.clearFlowStatus(f)
		if msg.err != nil {
			if errors.Is(msg.err, signin.ErrCancelled) {
				return m.endOnlineAddPair("sign-in cancelled; nothing was stored")
			}
			m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error()})
			f.browser = false
			mm, cmd := m.onlineAskKey() // paste-a-key offered instead
			return mm, cmd, true
		}
		f.key = msg.key
		return m, m.onlineValidateCmd(), true
	case onlineValidatedMsg:
		f := m.onlineAdd
		if f == nil || msg.f != f {
			return m, nil, true
		}
		m.clearFlowStatus(f)
		switch {
		case errors.Is(msg.err, setup.ErrKeyRejected):
			// The sentinel's own text names no provider; Validate's
			// wrapping does, but say it the same way whatever came back.
			m.appendEntryLocked(entry{Kind: entryErr, Text: setup.RejectionLine(msg.err, f.choice.Name)})
			f.key = ""
			mm, cmd := m.onlineAskKey()
			return mm, cmd, true
		case errors.As(msg.err, new(*setup.WorkspaceNeeded)):
			// The key is fine; the request needs the workspace header.
			f.step = stepWorkspace
			m.appendEntryLocked(entry{Kind: entryDim, Text: setup.WorkspacePrompt})
			return m, nil, true
		case msg.err != nil:
			m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error() + " (Enter the key again to retry, Esc to cancel)"})
			f.key = ""
			mm, cmd := m.onlineAskKey()
			return mm, cmd, true
		}
		f.models = msg.models
		f.step = stepModel
		if len(f.models) == 0 {
			m.appendEntryLocked(entry{Kind: entryDim, Text: f.choice.Name + " lists no models; type a model name:"})
			return m, nil, true
		}
		items := make([]pickItem, 0, len(f.models))
		for _, mo := range f.models {
			items = append(items, pickItem{id: mo.ID, label: mo.ID, desc: modelInfoDesc(mo)})
		}
		mm, cmd := m.openFlowPicker("Select model on "+f.choice.Name, items, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			if v.onlineAdd == nil {
				return v, nil
			}
			v.closeFlowPicker()
			v.onlineAdd.model = it.id
			return v, v.onlineAskDefault()
		})
		return mm, cmd, true
	}
	return m, nil, false
}

// modelInfoDesc is a listing row's description: what the listing said
// about the window, when it said anything.
func modelInfoDesc(mo provider.ModelInfo) string {
	if mo.ContextLength > 0 {
		return fmt.Sprintf("%dk context", mo.ContextLength/1024)
	}
	return ""
}

func (m *View) onlineAskDefault() tea.Cmd {
	f := m.onlineAdd
	f.step = stepDefault
	_, cmd := m.openFlowPicker(fmt.Sprintf("make %s the default provider?", f.choice.Name), []pickItem{
		{id: "no", label: "no", desc: "this session only; the next launch keeps its default"},
		{id: "yes", label: "yes", desc: "write default_provider"},
	}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
		if v.onlineAdd == nil {
			return v, nil
		}
		v.closeFlowPicker()
		return v.onlineFinish(it.id == "yes")
	})
	return cmd
}

// onlineFinish is the only place the flow writes anything: the credential,
// then the provider block, then the switch.
func (m *View) onlineFinish(makeDefault bool) (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	dir, err := config.Dir()
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	store, err := credentials.Open(dir)
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	added, err := setup.AddOnline(m.cfg, store, f.choice, f.key, f.source, f.model, makeDefault)
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	if added.Note != "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: added.Note})
	}
	m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("%s added (model %s); switching to it", f.choice.Name, added.Model)})
	name := f.choice.Name
	m.clearOnlineAdd()
	return m.setProvider(name)
}

func (m *View) endOnlineAdd(line string) (tea.Model, tea.Cmd) {
	m.appendEntryLocked(entry{Kind: entryErr, Text: line})
	m.clearOnlineAdd()
	return m, nil
}

func (m *View) endOnlineAddPair(line string) (tea.Model, tea.Cmd, bool) {
	mm, cmd := m.endOnlineAdd(line)
	return mm, cmd, true
}

func (m *View) clearOnlineAdd() {
	f := m.onlineAdd
	if f == nil {
		return
	}
	f.cancel()
	f.key = ""
	f.secret.SetValue("")
	// The input line was the flow's while it was open (onlineLine).
	m.input.Reset()
	m.clearFlowStatus(f)
	ownPicker := f.onlinePickerStep()
	m.onlineAdd = nil
	if m.Session.onlineAdder == m.id {
		m.Session.onlineAdding = false
	}
	if ownPicker {
		m.closeFlowPicker()
	}
}

// cancelOnlineAdd is Esc: nothing has been written before onlineFinish.
func (m *View) cancelOnlineAdd() {
	if m.onlineAdd == nil {
		return
	}
	m.appendEntryLocked(entry{Kind: entryDim, Text: "cancelled; nothing was stored"})
	m.clearOnlineAdd()
}

// onlinePickerStep reports whether the flow is waiting on one of its own
// pickers, so Esc there cancels the flow rather than only the list.
func (f *onlineAddFlow) onlinePickerStep() bool {
	switch f.step {
	case stepPreset, stepMethod, stepDefault:
		return true
	case stepModel:
		return len(f.models) > 0
	}
	return false
}
