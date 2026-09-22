package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/ui"
)

// Two ways into the command set: the palette (a popup above the input that
// opens the moment "/" is typed and filters as you type) and the menu (a
// full-screen grouped list behind /menu). Both reuse the picker's list
// state and key handling.

// ---- palette -----------------------------------------------------------------

func (m *View) slashEntries() []pickItem {
	items := make([]pickItem, 0, len(ui.SlashCommandTable)+len(m.custom))
	for _, c := range ui.SlashCommandTable {
		desc := c.Desc
		if m.running && !ui.BusySafeCommand(c.Name) {
			desc = "after the run · " + desc
		}
		items = append(items, pickItem{id: c.Name, label: c.Name, desc: desc, args: c.Args})
	}
	names := make([]string, 0, len(m.custom))
	for name := range m.custom {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		desc := "custom command (.becode/commands)"
		if m.running {
			desc = "after the run · " + desc
		}
		items = append(items, pickItem{id: "/" + name, label: "/" + name, desc: desc, args: true})
	}
	return items
}

// openPalette shows the command popup with an initial filter (text typed
// after the slash). Like every popup it is local to this terminal: it fills
// this terminal's input line, and no other terminal sees it.
func (m *View) openPalette(initial string) (tea.Model, tea.Cmd) {
	m.picker = &picker{title: "Commands", items: m.slashEntries(), filter: initial, inline: true, prefix: true,
		onPick: func(m *View, it pickItem) (tea.Model, tea.Cmd) {
			if it.args {
				m.input.SetValue(it.id + " ")
				m.input.CursorEnd()
				return m, nil
			}
			return m.slashCommand(it.id)
		}}
	// Remember what this terminal was doing before the palette took over —
	// chat, the inbox or a DM in particular — so closing it (handlePaletteKey's
	// returnMode()) puts it back there instead of dropping it to the transcript.
	if m.mode != modePalette {
		m.prevMode = m.mode
	}
	m.mode = modePalette
	m.input.SetValue("")
	return m, nil
}

func (m *View) handlePaletteKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		m.mode = m.returnMode()
		return m, nil
	}
	in := &m.input
	// handBack gives the typed text back to the owner's input line and
	// closes the popup, so nothing is lost on the way out.
	handBack := func(text string) (tea.Model, tea.Cmd) {
		in.SetValue(text)
		in.CursorEnd()
		m.picker = nil
		m.mode = m.returnMode()
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		return handBack("/" + p.filter)
	case tea.KeyBackspace:
		if p.filter == "" {
			return handBack("")
		}
	case tea.KeySpace:
		// A space ends the command name: hand over to the input, so an
		// argument (`/resume ABC123`) can be typed at the prompt. Bubble Tea
		// delivers a space as KeySpace, not as a KeyRunes " ", but a
		// terminal or a paste can still produce the runes form — both end
		// the command name.
		return handBack("/" + p.filter + " ")
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == ' ' {
			return handBack("/" + p.filter + " ")
		}
	case tea.KeyTab:
		if items := p.filtered(); len(items) > 0 {
			return handBack(items[p.cursor].id + " ")
		}
		return m, nil
	}
	return m.handlePickerKey(k)
}

// paletteBox renders the popup: title, up to 8 matching entries.
func (m *View) paletteBox() string {
	p := m.picker
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("/ commands") + m.st.Dim.Render("  ↑↓ pick · Enter run · Tab fill · Esc close"))
	b.WriteString("\n")
	items := p.filtered()
	if len(items) == 0 {
		b.WriteString(m.st.Dim.Render("(no command matches /" + p.filter + ")"))
	}
	maxRows := m.popupRows(8)
	omitDesc := m.omitPopupDesc()
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	for i := start; i < len(items) && i < start+maxRows; i++ {
		it := items[i]
		name := fmt.Sprintf("%-11s", it.label)
		desc := it.desc
		if omitDesc {
			desc = ""
		}
		if i == p.cursor {
			line := m.st.Accent.Render("> " + name)
			if desc != "" {
				line += " " + desc
			}
			b.WriteString(line + "\n")
		} else {
			line := "  " + name
			if desc != "" {
				line += " " + m.st.Dim.Render(desc)
			}
			b.WriteString(line + "\n")
		}
	}
	return m.st.Border.Width(m.width - 4).Render(strings.TrimRight(b.String(), "\n"))
}

// ---- theme picker ------------------------------------------------------------

// themeItems lists every theme (marking this view's own current one) plus a
// trailing row for the config default that a new device would start from;
// picking that row (its id is "") is a no-op — see applyTheme.
func (m *View) themeItems() []pickItem {
	items := make([]pickItem, 0, len(themes)+1)
	for _, n := range ThemeNames() {
		p := themes[n]
		desc := p.Desc
		if n == m.theme {
			desc += "  (current)"
		}
		items = append(items, pickItem{id: n, label: n, desc: desc})
	}
	items = append(items, pickItem{id: "", label: "default: " + m.cfg.Theme,
		desc: "what a new device gets; /theme default <name> changes it"})
	return items
}

// openThemePicker is this terminal's own theme list. The title reports the
// theme in use and its provenance (remembered for this device, the config
// default, or the built-in dark) so bare /theme answers "which theme am I
// on?" and "how do I change it?" in one popup.
func (m *View) openThemePicker() (tea.Model, tea.Cmd) {
	title := fmt.Sprintf("Theme — %s (%s)", m.theme, m.themeOrigin)
	return m.openPicker(title, func() ([]pickItem, error) { return m.themeItems(), nil },
		func(m *View, it pickItem) (tea.Model, tea.Cmd) { return m.applyTheme(it.id, false) })
}

// applyTheme switches this terminal's live palette. asDefault also sets
// cfg.Theme (what a new device gets); otherwise the pick is remembered only
// for this device, in cfg.ClientThemes. Every note here — the confirmation,
// a save failure, an unknown name — is local to this terminal: two people on
// one session must never see each other's theme changes on their shared
// transcript.
func (m *View) applyTheme(name string, asDefault bool) (tea.Model, tea.Cmd) {
	if name == "" {
		// The theme picker's trailing "default: <cfg.Theme>" row: nothing to
		// apply, it only shows what a new device would get.
		return m, nil
	}
	st, ok := newStyles(name)
	if !ok {
		m.renderEntryLocal(entry{Kind: entryErr, Text: "unknown theme " + name + "; try /theme to pick one"})
		return m, nil
	}
	m.st = st
	m.spin.Style = st.Accent
	m.richText = name != "mono"
	m.theme = name
	styleInput(&m.input, st)
	m.rebuild()
	if m.cfg.ThemeTerminalColors && m.termWrite != nil {
		m.termWrite(terminalColorSeq(name))
	}
	// cfg.Save() is a file write, not a broadcast — fine to make under mu
	// (already held: applyTheme is only ever reached from Update). Applying
	// the styles first means a save failure still leaves this terminal
	// themed; only the note differs.
	var confirm string
	if asDefault {
		m.cfg.Theme = name
		m.themeOrigin = "config default"
		confirm = "theme default set to " + name
	} else {
		key := live.LabelKey(m.label)
		m.cfg.ClientThemes[key] = name
		m.themeOrigin = fmt.Sprintf("remembered for %q", key)
		confirm = "theme set to " + name
	}
	if err := m.cfg.Save(); err != nil {
		m.renderEntryLocal(entry{Kind: entryWarn, Text: confirm + " for this session; could not save config: " + err.Error()})
	} else {
		m.renderEntryLocal(entry{Kind: entryOK, Text: confirm})
	}
	return m, nil
}

// ---- menu --------------------------------------------------------------------

type menuEntry struct {
	group, label, desc string
	run                func(m *View) (tea.Model, tea.Cmd)
}

// menuEntries builds the grouped menu. Its commands run on the terminal
// whose menu this is — "Detach this terminal" means that one.
func (m *View) menuEntries() []menuEntry {
	cmd := func(c string) func(*View) (tea.Model, tea.Cmd) {
		return func(m *View) (tea.Model, tea.Cmd) { return m.slashCommand(c) }
	}
	return []menuEntry{
		{"Sessions", "Resume a saved session", "pick from the session list", func(m *View) (tea.Model, tea.Cmd) { return m, m.askSessionPicker() }},
		{"Sessions", "New session", "clear the transcript and start fresh", cmd("/clear")},
		{"Sessions", "Show handoff briefing", "what was carried over from the resumed session", cmd("/handoff")},
		{"Sessions", "Attached terminals", "who is viewing this session", cmd("/clients")},
		{"Sessions", "Detach this terminal", "session keeps running; be-code attach <code> to return", cmd("/detach")},
		{"Models", "Switch model", "list models on the backend", func(m *View) (tea.Model, tea.Cmd) { return m, m.askModelPicker() }},
		{"Models", "Switch provider", "ollama, llama.cpp, vLLM, LM Studio…", func(m *View) (tea.Model, tea.Cmd) { return m, m.askProviderPicker() }},
		{"Tools", "List tools", "what the agent can call", cmd("/tools")},
		{"Tools", "Run verification", "build/lint/test checks for this workspace", cmd("/verify")},
		{"Tools", "Undo last turn", "roll back the last turn's file changes", cmd("/undo")},
		{"Context", "Compact now", "summarize older conversation with the model", cmd("/compact")},
		{"Context", "Repo map", "symbol outline in the system prompt", cmd("/map")},
		{"Context", "Usage stats", "requests, tool calls, tokens", cmd("/stats")},
		{"Settings", "Theme", "pick a colour theme for this terminal (applies immediately)",
			func(m *View) (tea.Model, tea.Cmd) { return m.openThemePicker() }},
		{"Settings", "Show config", "effective configuration", cmd("/config")},
		{"Settings", "Help", "command reference", cmd("/help")},
		{"Settings", "Quit", "exit BE-Code (writes the resume briefing)", cmd("/quit")},
	}
}

func (m *View) openMenu() (tea.Model, tea.Cmd) {
	entries := m.menuEntries()
	items := make([]pickItem, 0, len(entries))
	for i, e := range entries {
		items = append(items, pickItem{id: fmt.Sprint(i),
			label: fmt.Sprintf("%-9s %-26s", e.group, e.label), desc: e.desc})
	}
	m.picker = &picker{title: "Menu", items: items, onPick: func(m *View, it pickItem) (tea.Model, tea.Cmd) {
		var idx int
		fmt.Sscan(it.id, &idx)
		if idx >= 0 && idx < len(entries) {
			return entries[idx].run(m)
		}
		return m, nil
	}}
	// Remember what this terminal was doing before /menu took the screen —
	// chat, the inbox or a DM in particular — so leaving the menu (Esc or a
	// pick, both through handlePickerKey's returnMode()) puts it back there
	// instead of dropping it out to the ordinary transcript.
	if m.mode != modeMenu {
		m.prevMode = m.mode
	}
	m.mode = modeMenu
	return m, nil
}

// handleMenuKey drives /menu and the right-click popup, which share a mode
// pair and a picker and differ only in how they are drawn.
func (m *View) handleMenuKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Both leave through m.idleMode(), so a run still in progress lands back
	// in modeBusy on its own.
	return m.handlePickerKey(k)
}

// menuStatus is the block above the menu entries: everything the old
// status bar showed, plus the backend window.
func (m *View) menuStatus() string {
	win := "unknown"
	if w := m.ag.Window(); w > 0 {
		win = fmt.Sprintf("%d tokens", w)
	}
	rows := [][2]string{
		{"provider", m.prov.Name()},
		{"model", m.ag.Model},
		{"profile", m.ag.Profile.Family},
		{"window", win},
		{"context", fmt.Sprintf("%d of %d tokens (%d%%)", m.usage.ctxTokens, m.usage.budget, m.ctxPercent())},
		{"session total", fmt.Sprintf("%dk tokens", m.usage.total/1000)},
	}
	if m.ag.IDEName != "" {
		rows = append(rows, [2]string{"editor", agent.EditorLabel(m.ag.IDEName)})
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s %s\n", m.st.Dim.Render(fmt.Sprintf("%-14s", r[0])), r[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *View) viewMenu() string {
	p := m.picker
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("BE-Code menu") + "\n\n")
	if m.compact() {
		b.WriteString(m.compactMenuStatus() + "\n\n")
	} else {
		b.WriteString(m.menuStatus() + "\n\n")
	}
	b.WriteString(m.renderPickList(p, m.popupRows(m.height-14), m.compact()))
	body := m.st.Border.Width(m.width - 4).Render(b.String())
	return body + "\n" + m.st.Dim.Render(" ↑↓ move · Enter select · Esc back · type to filter")
}

var _ = lipgloss.Width
