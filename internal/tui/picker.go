package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/store"
)

// picker is a filterable arrow-key list overlay used for models, providers,
// and saved sessions.
type picker struct {
	title   string
	items   []pickItem
	cursor  int
	filter  string
	onPick  func(m *Model, it pickItem) (tea.Model, tea.Cmd)
	loading bool
	inline  bool // rendered as a popup above the input (the "/" palette)
	prefix  bool // rank label-prefix matches first, shortest first (commands)
}

type pickItem struct {
	id    string
	label string
	desc  string
	args  bool // command takes arguments (palette fills the input)
}

type pickerItemsMsg struct {
	items []pickItem
	err   error
}

func (m *Model) openPicker(title string, load func() ([]pickItem, error),
	onPick func(*Model, pickItem) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	m.picker = &picker{title: title, onPick: onPick, loading: true}
	m.mode = modePicker
	return m, func() tea.Msg {
		items, err := load()
		return pickerItemsMsg{items: items, err: err}
	}
}

func (m *Model) openModelPicker() (tea.Model, tea.Cmd) {
	return m.openPicker("Select model", func() ([]pickItem, error) {
		models, err := m.prov.ListModels(m.rootCtx)
		if err != nil {
			return nil, err
		}
		items := make([]pickItem, 0, len(models))
		for _, mo := range models {
			desc := ""
			if mo.SizeBytes > 0 {
				desc = fmt.Sprintf("%.1fGB %s %s", float64(mo.SizeBytes)/1e9, mo.Family, mo.Quantization)
			}
			items = append(items, pickItem{id: mo.ID, label: mo.ID, desc: desc})
		}
		return items, nil
	}, func(m *Model, it pickItem) (tea.Model, tea.Cmd) {
		m.ag.SetModel(it.id)
		m.appendLine(stOK.Render("model set to " + it.id))
		return m, nil
	})
}

func (m *Model) openProviderPicker() (tea.Model, tea.Cmd) {
	return m.openPicker("Select provider", func() ([]pickItem, error) {
		items := make([]pickItem, 0, len(m.cfg.Providers))
		for name, pc := range m.cfg.Providers {
			items = append(items, pickItem{id: name, label: name, desc: pc.BaseURL})
		}
		sortItems(items)
		return items, nil
	}, func(m *Model, it pickItem) (tea.Model, tea.Cmd) {
		return m.setProvider(it.id)
	})
}

func (m *Model) openSessionPicker() (tea.Model, tea.Cmd) {
	return m.openPicker("Resume session", func() ([]pickItem, error) {
		metas, err := store.List()
		if err != nil {
			return nil, err
		}
		items := make([]pickItem, 0, len(metas))
		for _, s := range metas {
			items = append(items, pickItem{
				id:    s.ID,
				label: s.Code + "  " + s.Title,
				desc:  fmt.Sprintf("%s · %d turns · %s", s.ID, s.Turns, s.UpdatedAt.Format("Jan 2 15:04")),
			})
		}
		return items, nil
	}, func(m *Model, it pickItem) (tea.Model, tea.Cmd) {
		return m.resumeSession(it.id)
	})
}

func sortItems(items []pickItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].id < items[j-1].id; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func (p *picker) filtered() []pickItem {
	if p.filter == "" {
		return p.items
	}
	f := strings.ToLower(p.filter)
	var out []pickItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.label), f) || strings.Contains(strings.ToLower(it.desc), f) {
			out = append(out, it)
		}
	}
	if p.prefix {
		rank := func(it pickItem) int {
			l := strings.ToLower(strings.TrimPrefix(it.label, "/"))
			switch {
			case l == f:
				return 0
			case strings.HasPrefix(l, f):
				return 1 + len(l) // shorter names first
			default:
				return 1000 + len(l)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	}
	return out
}

func (m *Model) handlePickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		m.mode = modeInput
		return m, nil
	}
	items := p.filtered()
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.picker = nil
		m.mode = modeInput
		m.input.Focus()
		return m, nil
	case tea.KeyUp:
		if p.cursor > 0 {
			p.cursor--
		}
	case tea.KeyDown:
		if p.cursor < len(items)-1 {
			p.cursor++
		}
	case tea.KeyEnter:
		if len(items) == 0 {
			return m, nil
		}
		it := items[p.cursor]
		m.picker = nil
		m.mode = modeInput
		return p.onPick(m, it)
	case tea.KeyBackspace:
		if len(p.filter) > 0 {
			p.filter = p.filter[:len(p.filter)-1]
			p.cursor = 0
		}
	case tea.KeyRunes:
		p.filter += string(k.Runes)
		p.cursor = 0
	}
	return m, nil
}

// pickerUpdate handles async item loading; called from Model.Update.
func (m *Model) pickerUpdate(msg pickerItemsMsg) {
	if m.picker == nil {
		return
	}
	m.picker.loading = false
	if msg.err != nil {
		m.appendLine(stErr.Render(msg.err.Error()))
		m.picker = nil
		m.mode = modeInput
		return
	}
	m.picker.items = msg.items
}

// renderPickList draws the filtered items with the cursor, scrolled so the
// cursor stays visible within maxRows. omitDesc drops item descriptions
// entirely (narrow/compact layouts), keeping each row to its label.
func (m *Model) renderPickList(p *picker, maxRows int, omitDesc bool) string {
	var b strings.Builder
	if p.filter != "" {
		b.WriteString(stDim.Render("filter: "+p.filter) + "\n")
	}
	items := p.filtered()
	if len(items) == 0 {
		b.WriteString(stDim.Render("(nothing matches)"))
	}
	if maxRows < 3 {
		maxRows = 3
	}
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	// Keep each entry on one row: trim the description to the space left.
	for i := start; i < len(items) && i < start+maxRows; i++ {
		it := items[i]
		desc := it.desc
		if omitDesc {
			desc = ""
		} else if room := m.width - 10 - lipgloss.Width(it.label); desc != "" && room > 8 && lipgloss.Width(desc) > room {
			desc = desc[:room-1] + "…"
		} else if room <= 8 {
			desc = ""
		}
		cursor := "  "
		line := it.label
		if desc != "" {
			line += "  " + stDim.Render(desc)
		}
		if i == p.cursor {
			cursor = stAccent.Render("> ")
			line = stAccent.Render(it.label)
			if desc != "" {
				line += "  " + stDim.Render(desc)
			}
		}
		b.WriteString(cursor + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) viewPicker() string {
	p := m.picker
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(stModalTi.Render(p.title))
	if p.loading {
		b.WriteString("\n\n" + m.spin.View() + " loading…")
	} else {
		b.WriteString(stDim.Render("   type to filter: "+p.filter) + "\n\n")
		b.WriteString(m.renderPickList(p, m.popupRows(m.height-8), m.omitPopupDesc()))
	}
	body := stBorder.Width(m.width - 4).Render(b.String())
	return body + "\n" + stDim.Render(" ↑↓ move · Enter select · Esc cancel · type to filter")
}

var _ = lipgloss.Width // keep import if styles change
