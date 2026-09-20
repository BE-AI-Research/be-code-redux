package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// picker is a filterable arrow-key list overlay. It is the local half of a
// list: the shared model/provider/session lists live on an ask (their items
// are the session's, their cursor and filter each terminal's own — see
// ask.go), while the theme picker, the palette and the menus keep the whole
// thing local with onPick wired up here.
type picker struct {
	title   string
	items   []pickItem
	cursor  int
	filter  string
	onPick  func(m *View, it pickItem) (tea.Model, tea.Cmd)
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

func (m *View) openPicker(title string, load func() ([]pickItem, error),
	onPick func(*View, pickItem) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	m.picker = &picker{title: title, onPick: onPick, loading: true}
	m.mode = modePicker
	return m, func() tea.Msg {
		items, err := load()
		return pickerItemsMsg{items: items, err: err}
	}
}

// askList is the shape every shared picker has: load the rows off the event
// loop, then put them to the whole session as one ask. The returned Cmd is
// what a slash command or a menu entry returns; it runs on Bubble Tea's own
// goroutine, so blocking in Ask is exactly right. A load that fails says so
// on the transcript and asks nothing.
func (m *View) askList(title string, load func() ([]pickItem, error),
	onPick func(v *View, id string, from int) tea.Cmd) tea.Cmd {
	s, from := m.Session, m.id
	ctx := s.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		// This goroutine is Bubble Tea's, not the host's supervisor: a panic
		// in a load (a provider listing malformed rows, a session file that
		// cannot be parsed) would take the whole session host down with it.
		defer func() {
			if rec := recover(); rec != nil {
				s.appendEntry(entry{Kind: entryErr, Text: fmt.Sprintf("%s failed: %v", title, rec)})
			}
		}()
		items, err := load()
		if err != nil {
			s.appendEntry(entry{Kind: entryErr, Text: err.Error()})
			return nil
		}
		if ans := s.Ask(ctx, &ask{Kind: askPicker, Title: title, Items: items, onPick: onPick}); ans.Refused {
			// An approval or a plan is already on every screen. The list is
			// not raised over it, and the terminal that asked for it — only
			// that one — is told why.
			s.sendTo(from, noticeMsg("a prompt is already open; answer it first"))
		}
		return nil
	}
}

// askModelPicker puts the backend's model list to every attached terminal;
// whichever answers switches the model for all of them.
func (m *View) askModelPicker() tea.Cmd {
	// The provider is read here, under the lock Update holds, not from the
	// loading goroutine: /provider can replace it while this list loads.
	prov, ctx := m.prov, m.rootCtx
	return m.askList("Select model", func() ([]pickItem, error) {
		// Details, not ListModels: the window a model is loaded with and
		// whether it is resident at all are what the list is being asked.
		models, err := provider.ModelDetails(ctx, prov)
		if err != nil {
			return nil, err
		}
		items := make([]pickItem, 0, len(models))
		for _, mo := range models {
			items = append(items, pickItem{id: mo.ID, label: mo.ID, desc: mo.Describe()})
		}
		return items, nil
	}, func(v *View, id string, _ int) tea.Cmd {
		v.ag.SetModel(id)
		v.appendEntryLocked(entry{Kind: entryOK, Text: "model set to " + id})
		return nil
	})
}

func (m *View) askProviderPicker() tea.Cmd {
	s := m.Session
	return m.askList("Select provider", func() ([]pickItem, error) {
		items := make([]pickItem, 0, len(s.cfg.Providers))
		for name, pc := range s.cfg.Providers {
			items = append(items, pickItem{id: name, label: name, desc: pc.BaseURL})
		}
		sortItems(items)
		return items, nil
	}, func(v *View, id string, _ int) tea.Cmd {
		_, cmd := v.setProvider(id)
		return cmd
	})
}

// askSessionPicker lists the saved sessions. The terminal a picked row acts
// for is the one that confirmed it, not the one that opened the list: a row
// whose session is already live hands *that* terminal to its host (see
// resumeFrom).
func (m *View) askSessionPicker() tea.Cmd {
	s := m.Session
	return m.askList("Resume session", func() ([]pickItem, error) {
		metas, err := store.List()
		if err != nil {
			return nil, err
		}
		return s.sessionItems(metas), nil
	}, func(v *View, id string, from int) tea.Cmd {
		_, cmd := v.resumeFrom(id, from)
		return cmd
	})
}

// sessionItems turns saved-session metadata into picker rows, marking the
// codes that already have a host running somewhere: those rows join the
// live session instead of loading a second copy of its file.
func (s *Session) sessionItems(metas []store.Meta) []pickItem {
	now := s.liveCodes()
	items := make([]pickItem, 0, len(metas))
	seen := map[string]bool{}
	for _, meta := range metas {
		seen[meta.Code] = true
		label := meta.Code + "  " + meta.Title
		desc := fmt.Sprintf("%s · %d turns · %s", meta.ID, meta.Turns, meta.UpdatedAt.Format("Jan 2 15:04"))
		if now[meta.Code] {
			label = meta.Code + " LIVE " + meta.Title
			desc = "live now · joins it · " + desc
		}
		items = append(items, pickItem{id: meta.ID, label: label, desc: desc})
	}
	// A host's session exists before its first autosave, so a live session
	// with no turns yet has no file for store.List to find. List those from
	// the registry (as `be-code sessions` does); their id is the code, which
	// resumeFrom joins without touching the store.
	if s.liveRecords != nil {
		for _, r := range s.liveRecords() {
			if seen[r.Code] {
				continue
			}
			seen[r.Code] = true
			items = append(items, pickItem{
				id:    r.Code,
				label: r.Code + " LIVE (no saved turns yet)",
				desc:  fmt.Sprintf("live now · joins it · no saved turns yet · %s · started %s", r.Workspace, r.StartedAt.Format("Jan 2 15:04")),
			})
		}
	}
	return items
}

// liveSessionRecords is the default for Session.liveRecords: the hosts
// advertised in ~/.be-code/live, minus the records whose host is gone
// (live.List prunes those).
func liveSessionRecords() []live.Record {
	dir, err := live.Dir()
	if err != nil {
		return nil
	}
	recs, _ := live.List(dir)
	return recs
}

// liveSessionCodes is the default for Session.liveCodes: the codes of
// liveSessionRecords.
func liveSessionCodes() map[string]bool {
	codes := map[string]bool{}
	for _, r := range liveSessionRecords() {
		codes[r.Code] = true
	}
	return codes
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

// pickerNav applies one key's cursor movement or filter edit to p, reporting
// whether it was one of those keys. It is the half of list navigation the
// local pickers and the shared ask-picker have in common.
func pickerNav(p *picker, k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyUp:
		if p.cursor > 0 {
			p.cursor--
		}
	case tea.KeyDown:
		if p.cursor < len(p.filtered())-1 {
			p.cursor++
		}
	case tea.KeyBackspace:
		if len(p.filter) > 0 {
			p.filter = p.filter[:len(p.filter)-1]
			p.cursor = 0
		}
	case tea.KeyRunes:
		p.filter += string(k.Runes)
		p.cursor = 0
	default:
		return false
	}
	return true
}

// handlePickerKey drives the view-local list overlays (the theme picker, the
// palette and the menus), refocusing this terminal's input line on the way
// out. The shared lists go through handleAskPickerKey instead.
func (m *View) handlePickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	if p == nil {
		m.mode = m.idleMode()
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.picker = nil
		m.mode = m.idleMode()
		m.input.Focus()
		return m, nil
	case tea.KeyEnter:
		items := p.filtered()
		if len(items) == 0 {
			return m, nil
		}
		it := items[p.cursor]
		m.picker = nil
		m.mode = m.idleMode()
		return p.onPick(m, it)
	}
	pickerNav(p, k)
	return m, nil
}

// pickerUpdate handles async item loading for a view-local picker; called
// from View.Update. A shared ask that has taken the frame since the load
// started owns m.picker now, and its items are not this load's to replace.
func (m *View) pickerUpdate(msg pickerItemsMsg) {
	if m.picker == nil || m.mode != modePicker {
		return
	}
	m.picker.loading = false
	if msg.err != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error()})
		m.picker = nil
		m.mode = m.idleMode()
		return
	}
	m.picker.items = msg.items
}

// renderPickList draws the filtered items with the cursor, scrolled so the
// cursor stays visible within maxRows. omitDesc drops item descriptions
// entirely (narrow/compact layouts), keeping each row to its label.
func (m *View) renderPickList(p *picker, maxRows int, omitDesc bool) string {
	var b strings.Builder
	if p.filter != "" {
		b.WriteString(m.st.Dim.Render("filter: "+p.filter) + "\n")
	}
	items := p.filtered()
	if len(items) == 0 {
		b.WriteString(m.st.Dim.Render("(nothing matches)"))
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
			line += "  " + m.st.Dim.Render(desc)
		}
		if i == p.cursor {
			cursor = m.st.Accent.Render("> ")
			line = m.st.Accent.Render(it.label)
			if desc != "" {
				line += "  " + m.st.Dim.Render(desc)
			}
		}
		b.WriteString(cursor + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *View) viewPicker() string {
	p := m.picker
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render(p.title))
	if p.loading {
		b.WriteString("\n\n" + m.spin.View() + " loading…")
	} else {
		b.WriteString(m.st.Dim.Render("   type to filter: "+p.filter) + "\n\n")
		b.WriteString(m.renderPickList(p, m.popupRows(m.height-8), m.omitPopupDesc()))
	}
	body := m.st.Border.Width(m.width - 4).Render(b.String())
	return body + "\n" + m.st.Dim.Render(" ↑↓ move · Enter select · Esc cancel · type to filter")
}

var _ = lipgloss.Width // keep import if styles change
