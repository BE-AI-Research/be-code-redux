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

func (m *Model) slashEntries() []pickItem {
	items := make([]pickItem, 0, len(ui.SlashCommandTable)+len(m.custom))
	for _, c := range ui.SlashCommandTable {
		items = append(items, pickItem{id: c.Name, label: c.Name, desc: c.Desc, args: c.Args})
	}
	names := make([]string, 0, len(m.custom))
	for name := range m.custom {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		items = append(items, pickItem{id: "/" + name, label: "/" + name, desc: "custom command (.becode/commands)", args: true})
	}
	return items
}

// openPalette shows the command popup with an initial filter (text typed
// after the slash).
func (m *Model) openPalette(initial string) (tea.Model, tea.Cmd) {
	m.picker = &picker{title: "Commands", items: m.slashEntries(), filter: initial, inline: true, prefix: true,
		onPick: func(m *Model, it pickItem) (tea.Model, tea.Cmd) {
			if it.args {
				m.input.SetValue(it.id + " ")
				m.input.CursorEnd()
				return m, nil
			}
			return m.slashCommand(it.id)
		}}
	m.mode = modePalette
	m.input.SetValue("")
	return m, nil
}

func (m *Model) handlePaletteKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		m.mode = modeInput
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		// Leave what was typed in the input so nothing is lost.
		m.input.SetValue("/" + p.filter)
		m.input.CursorEnd()
		m.picker = nil
		m.mode = modeInput
		return m, nil
	case tea.KeyBackspace:
		if p.filter == "" {
			m.picker = nil
			m.mode = modeInput
			return m, nil
		}
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == ' ' {
			// A space ends the command name: hand over to the input.
			m.input.SetValue("/" + p.filter + " ")
			m.input.CursorEnd()
			m.picker = nil
			m.mode = modeInput
			return m, nil
		}
	case tea.KeyTab:
		if items := p.filtered(); len(items) > 0 {
			it := items[p.cursor]
			m.input.SetValue(it.id + " ")
			m.input.CursorEnd()
			m.picker = nil
			m.mode = modeInput
		}
		return m, nil
	}
	return m.handlePickerKey(k)
}

// paletteBox renders the popup: title, up to 8 matching entries.
func (m *Model) paletteBox() string {
	p := m.picker
	var b strings.Builder
	b.WriteString(stModalTi.Render("/ commands") + stDim.Render("  ↑↓ pick · Enter run · Tab fill · Esc close"))
	b.WriteString("\n")
	items := p.filtered()
	if len(items) == 0 {
		b.WriteString(stDim.Render("(no command matches /" + p.filter + ")"))
	}
	const maxRows = 8
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	for i := start; i < len(items) && i < start+maxRows; i++ {
		it := items[i]
		name := fmt.Sprintf("%-11s", it.label)
		if i == p.cursor {
			b.WriteString(stAccent.Render("> "+name) + " " + it.desc + "\n")
		} else {
			b.WriteString("  " + name + " " + stDim.Render(it.desc) + "\n")
		}
	}
	return stBorder.Width(m.width - 4).Render(strings.TrimRight(b.String(), "\n"))
}

// ---- menu --------------------------------------------------------------------

type menuEntry struct {
	group, label, desc string
	run                func(m *Model) (tea.Model, tea.Cmd)
}

func (m *Model) menuEntries() []menuEntry {
	cmd := func(c string) func(*Model) (tea.Model, tea.Cmd) {
		return func(m *Model) (tea.Model, tea.Cmd) { return m.slashCommand(c) }
	}
	return []menuEntry{
		{"Sessions", "Resume a saved session", "pick from the session list", func(m *Model) (tea.Model, tea.Cmd) { return m.openSessionPicker() }},
		{"Sessions", "New session", "clear the transcript and start fresh", cmd("/clear")},
		{"Sessions", "Show handoff briefing", "what was carried over from the resumed session", cmd("/handoff")},
		{"Models", "Switch model", "list models on the backend", func(m *Model) (tea.Model, tea.Cmd) { return m.openModelPicker() }},
		{"Models", "Switch provider", "ollama, llama.cpp, vLLM, LM Studio…", func(m *Model) (tea.Model, tea.Cmd) { return m.openProviderPicker() }},
		{"Tools", "List tools", "what the agent can call", cmd("/tools")},
		{"Tools", "Run verification", "build/lint/test checks for this workspace", cmd("/verify")},
		{"Tools", "Undo last turn", "roll back the last turn's file changes", cmd("/undo")},
		{"Context", "Compact now", "summarize older conversation with the model", cmd("/compact")},
		{"Context", "Repo map", "symbol outline in the system prompt", cmd("/map")},
		{"Context", "Usage stats", "requests, tool calls, tokens", cmd("/stats")},
		{"Settings", "Show config", "effective configuration", cmd("/config")},
		{"Settings", "Help", "command reference", cmd("/help")},
		{"Settings", "Quit", "exit BE-Code (writes the resume briefing)", cmd("/quit")},
	}
}

func (m *Model) openMenu() (tea.Model, tea.Cmd) {
	entries := m.menuEntries()
	items := make([]pickItem, 0, len(entries))
	for i, e := range entries {
		items = append(items, pickItem{id: fmt.Sprint(i),
			label: fmt.Sprintf("%-9s %-26s", e.group, e.label), desc: e.desc})
	}
	m.picker = &picker{title: "Menu", items: items, onPick: func(m *Model, it pickItem) (tea.Model, tea.Cmd) {
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

func (m *Model) handleMenuKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.handlePickerKey(k)
}

// menuStatus is the block above the menu entries: everything the old
// status bar showed, plus the backend window.
func (m *Model) menuStatus() string {
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
		fmt.Fprintf(&b, "%s %s\n", stDim.Render(fmt.Sprintf("%-14s", r[0])), r[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) viewMenu() string {
	p := m.picker
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(stModalTi.Render("BE-Code menu") + "\n\n")
	b.WriteString(m.menuStatus() + "\n\n")
	b.WriteString(m.renderPickList(p, m.height-14))
	body := stBorder.Width(m.width - 4).Render(b.String())
	return body + "\n" + stDim.Render(" ↑↓ move · Enter select · Esc back · type to filter")
}

var _ = lipgloss.Width
