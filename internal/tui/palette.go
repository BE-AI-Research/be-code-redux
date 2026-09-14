package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
// after the slash). The popup belongs to the client that opened it: it
// fills that client's input line, and only its keys reach it.
func (m *View) openPalette(initial string, from int) (tea.Model, tea.Cmd) {
	m.paletteOwner = from
	m.picker = &picker{title: "Commands", items: m.slashEntries(), filter: initial, inline: true, prefix: true,
		onPick: func(m *View, it pickItem) (tea.Model, tea.Cmd) {
			if it.args {
				in := m.inputFor(m.paletteOwner)
				in.SetValue(it.id + " ")
				in.CursorEnd()
				return m, nil
			}
			return m.slashCommand(it.id, m.paletteOwner)
		}}
	m.mode = modePalette
	m.inputFor(from).SetValue("")
	return m, nil
}

func (m *View) handlePaletteKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	if from != m.paletteOwner {
		// Another terminal's keys are none of this popup's business — but
		// they are that terminal's own business (see handleGuestKey).
		return m.handleGuestKey(k, from)
	}
	p := m.picker
	if p == nil {
		m.mode = m.idleMode()
		return m, nil
	}
	in := m.inputFor(m.paletteOwner)
	// handBack gives the typed text back to the owner's input line and
	// closes the popup, so nothing is lost on the way out.
	handBack := func(text string) (tea.Model, tea.Cmd) {
		in.SetValue(text)
		in.CursorEnd()
		m.picker = nil
		m.mode = m.idleMode()
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
	return m.handlePickerKey(k, from)
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

func (m *View) themeItems() []pickItem {
	items := make([]pickItem, 0, len(themes))
	for _, n := range ThemeNames() {
		p := themes[n]
		desc := p.Desc
		if n == m.cfg.Theme {
			desc += "  (current)"
		}
		items = append(items, pickItem{id: n, label: n, desc: desc})
	}
	return items
}

func (m *View) openThemePicker() (tea.Model, tea.Cmd) {
	return m.openPicker("Theme", func() ([]pickItem, error) { return m.themeItems(), nil },
		func(m *View, it pickItem) (tea.Model, tea.Cmd) { return m.applyTheme(it.id) })
}

// applyTheme switches the live palette and persists the choice.
func (m *View) applyTheme(name string) (tea.Model, tea.Cmd) {
	st, ok := newStyles(name)
	if !ok {
		m.appendEntryLocked(entry{Kind: entryErr, Text: "unknown theme " + name + "; try /theme to pick one"})
		return m, nil
	}
	m.st = st
	m.spin.Style = st.Accent
	m.cfg.Theme = name
	m.richText = name != "mono"
	if m.cfg.ThemeTerminalColors && m.termWrite != nil {
		m.termWrite(terminalColorSeq(name))
	}
	if err := m.cfg.Save(); err != nil {
		m.appendEntryLocked(entry{Kind: entryWarn, Text: "theme set to " + name + " for this session; could not save config: " + err.Error()})
	} else {
		m.appendEntryLocked(entry{Kind: entryOK, Text: "theme set to " + name})
	}
	m.rebuild()
	return m, nil
}

// ---- menu --------------------------------------------------------------------

type menuEntry struct {
	group, label, desc string
	run                func(m *View) (tea.Model, tea.Cmd)
}

// menuEntries builds the grouped menu. Its commands run as the client that
// opened the menu — "Detach this terminal" has to mean the terminal whose
// user picked it, not whoever happened to press a key.
func (m *View) menuEntries(owner int) []menuEntry {
	cmd := func(c string) func(*View) (tea.Model, tea.Cmd) {
		return func(m *View) (tea.Model, tea.Cmd) { return m.slashCommand(c, owner) }
	}
	return []menuEntry{
		{"Sessions", "Resume a saved session", "pick from the session list", func(m *View) (tea.Model, tea.Cmd) { return m.openSessionPicker(owner) }},
		{"Sessions", "New session", "clear the transcript and start fresh", cmd("/clear")},
		{"Sessions", "Show handoff briefing", "what was carried over from the resumed session", cmd("/handoff")},
		{"Sessions", "Attached terminals", "who is viewing this session", cmd("/clients")},
		{"Sessions", "Detach this terminal", "session keeps running; be-code attach <code> to return", cmd("/detach")},
		{"Models", "Switch model", "list models on the backend", func(m *View) (tea.Model, tea.Cmd) { return m.openModelPicker() }},
		{"Models", "Switch provider", "ollama, llama.cpp, vLLM, LM Studio…", func(m *View) (tea.Model, tea.Cmd) { return m.openProviderPicker() }},
		{"Tools", "List tools", "what the agent can call", cmd("/tools")},
		{"Tools", "Run verification", "build/lint/test checks for this workspace", cmd("/verify")},
		{"Tools", "Undo last turn", "roll back the last turn's file changes", cmd("/undo")},
		{"Context", "Compact now", "summarize older conversation with the model", cmd("/compact")},
		{"Context", "Repo map", "symbol outline in the system prompt", cmd("/map")},
		{"Context", "Usage stats", "requests, tool calls, tokens", cmd("/stats")},
		{"Settings", "Theme", "pick a colour theme (applies immediately)", cmd("/theme")},
		{"Settings", "Show config", "effective configuration", cmd("/config")},
		{"Settings", "Help", "command reference", cmd("/help")},
		{"Settings", "Quit", "exit BE-Code (writes the resume briefing)", cmd("/quit")},
	}
}

func (m *View) openMenu(from int) (tea.Model, tea.Cmd) {
	m.menuOwner = from
	entries := m.menuEntries(from)
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
	m.mode = modeMenu
	return m, nil
}

func (m *View) handleMenuKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	if from != m.menuOwner {
		// The menu acts for the terminal that opened it; everyone else
		// keeps typing into their own input line (see handleGuestKey).
		return m.handleGuestKey(k, from)
	}
	return m.handlePickerKey(k, from)
}

// menuStatus is the block above the menu entries: everything the old
// status bar showed, plus the backend window.
func (m *View) menuStatus() string {
	win := "unknown"
	if m.ag.Window > 0 {
		win = fmt.Sprintf("%d tokens", m.ag.Window)
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
		rows = append(rows, [2]string{"editor", m.ag.IDEName})
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
