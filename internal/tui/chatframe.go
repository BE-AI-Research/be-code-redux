package tui

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/inbox"
)

// The frame the room, the inbox and a DM thread share (2026-11-03, owner's
// design): a title row with a button row on its right, a heavy rule in the
// theme's accent under it and another above the input, a colour chip per
// person, a bar on any line that names you, and dividers where time passes.
// A compact terminal keeps the plain footer it had: no buttons, no rules.

// frameRows is how many rows the frame takes above the input: the title
// row and the two rules, or none in the compact layout.
func (m *View) frameRows() int {
	if m.compact() || m.height-m.inputRows()-2-3 < 3 {
		// Compact, or a layout forced to full on a terminal too short to
		// hold the frame and three rows of content: the plain footer.
		return 0
	}
	return 3
}

// framed reports whether the frame is drawn: frameRows() > 0.
func (m *View) framed() bool { return m.frameRows() > 0 }

// neonRule is the heavy accent rule across the terminal.
func (m *View) neonRule() string {
	if m.width <= 0 {
		return ""
	}
	return m.st.Accent.Bold(true).Render(strings.Repeat("━", m.width))
}

// thinRule is the dim rule between inbox rows.
func (m *View) thinRule() string {
	if m.width <= 0 {
		return ""
	}
	return m.st.Dim.Render(strings.Repeat("─", m.width))
}

// chatButton is one button on the title row and where it was drawn, so a
// click on the row can find it. Zones are rebuilt on every render.
type chatButton struct {
	label  string
	action string // room | inbox | dm | pings | back
	x0, x1 int    // columns, inclusive-exclusive
}

// chatButtons is the row's buttons for this terminal, right now.
func (m *View) chatButtons() []chatButton {
	inboxLabel := "Inbox"
	if n := m.unreadDMs(); n > 0 {
		inboxLabel = fmt.Sprintf("Inbox ●%d", n)
	}
	pings := "@ pings on"
	if !m.cfg.Chat.PingsOn(m.userID()) {
		pings = "@ pings off"
	}
	return []chatButton{
		{label: "Room", action: "room"},
		{label: inboxLabel, action: "inbox"},
		{label: "DM ▸", action: "dm"},
		{label: pings, action: "pings"},
		{label: "← Back", action: "back"},
	}
}

// renderButtons draws the buttons, the current view's filled, and records
// their zones from x0 (the column the row's right part starts at).
func (m *View) renderButtons(x0 int) string {
	active := map[mode]string{modeChat: "room", modeInbox: "inbox", modeDM: "dm"}[m.mode]
	var b strings.Builder
	x := x0
	m.btnZones = m.btnZones[:0]
	for i, bt := range m.chatButtons() {
		if i > 0 {
			b.WriteString(" ")
			x++
		}
		text := " " + bt.label + " "
		w := lipgloss.Width(text)
		if bt.action == active {
			b.WriteString(m.st.ChipOn.Render(text))
		} else {
			b.WriteString(m.st.Button.Render(text))
		}
		bt.x0, bt.x1 = x, x+w
		m.btnZones = append(m.btnZones, bt)
		x += w
	}
	return b.String()
}

// buttonsWidth is the row's buttons' width, for laying out the title row.
func (m *View) buttonsWidth() int {
	w := 0
	for i, bt := range m.chatButtons() {
		if i > 0 {
			w++
		}
		w += lipgloss.Width(" " + bt.label + " ")
	}
	return w
}

// titleRow is "▌ TITLE · detail" on the left and the buttons on the right,
// buttons dropped (and no zones kept) when the row cannot hold both.
func (m *View) titleRow(title, detail string) string {
	left := m.st.Accent.Bold(true).Render("▌ "+title) + " " + detail
	lw := lipgloss.Width(left)
	bw := m.buttonsWidth()
	if lw+2+bw > m.width {
		m.btnZones = m.btnZones[:0]
		return padToWidth(left, m.width)
	}
	x0 := m.width - bw
	return left + strings.Repeat(" ", x0-lw) + m.renderButtons(x0)
}

// frameTop is the title row and the rule under it; frameBottom the rule
// above the input. Both are empty in the compact layout.
func (m *View) frameTop(title, detail string) string {
	if !m.framed() {
		m.btnZones = m.btnZones[:0]
		return ""
	}
	return m.titleRow(title, detail) + "\n" + m.neonRule() + "\n"
}

func (m *View) frameBottom() string {
	if !m.framed() {
		return ""
	}
	return m.neonRule() + "\n"
}

// clickButton acts on a left click at (x, y) on the title row: the button
// there, if any. It reports whether the click was taken.
func (m *View) clickButton(x, y int) (tea.Model, tea.Cmd, bool) {
	if y != 0 || !m.framed() {
		return m, nil, false
	}
	active := map[mode]string{modeChat: "room", modeInbox: "inbox"}[m.mode]
	for _, bt := range m.btnZones {
		if x < bt.x0 || x >= bt.x1 {
			continue
		}
		if bt.action == active {
			return m, nil, true // already here: the draft stays as it is
		}
		switch bt.action {
		case "room":
			m.parkDraft()
			mm, cmd := m.enterChat()
			return mm, cmd, true
		case "inbox":
			m.parkDraft()
			mm, cmd := m.enterInbox()
			return mm, cmd, true
		case "dm":
			// Inside a thread too: the way to start another conversation.
			if m.input.Value() == "" {
				m.input.SetValue("/dm ")
				m.input.CursorEnd()
			}
			return m, nil, true
		case "pings":
			arg := "off"
			if !m.cfg.Chat.PingsOn(m.userID()) {
				arg = "on"
			}
			mm, cmd := m.chatPings([]string{arg})
			return mm, cmd, true
		case "back":
			m.parkDraft()
			mm, cmd := m.leaveMode()
			return mm, cmd, true
		}
	}
	return m, nil, false
}

// draftKey names the view a draft belongs to: the room, the inbox, one DM
// thread, or the transcript.
func (m *View) draftKey() string {
	switch m.mode {
	case modeChat:
		return "room"
	case modeInbox:
		return "inbox"
	case modeDM:
		return "dm:" + m.dm.with
	}
	return "main"
}

// parkDraft puts aside what is being typed here before a click moves this
// terminal elsewhere. The draft never travels: a line meant for a DM must
// not land in the room, and a draft in the inbox's input would take its
// keys from the list.
func (m *View) parkDraft() {
	if v := m.input.Value(); v != "" {
		if m.parked == nil {
			m.parked = map[string]string{}
		}
		m.parked[m.draftKey()] = v
	}
}

// restoreDraft brings back the draft parked for the view just entered, if
// its input is empty. Every entry point calls it, keyboard or click, so a
// thread reopened by Enter in the inbox has its half-typed line back.
func (m *View) restoreDraft() {
	if m.input.Value() != "" {
		return
	}
	if v, ok := m.parked[m.draftKey()]; ok {
		delete(m.parked, m.draftKey())
		m.input.SetValue(v)
		m.input.CursorEnd()
	}
}

// userColorIndex picks a stable colour for a user ID: the same ID lands on
// the same colour on every terminal, by hashing the name.
func userColorIndex(id string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(n))
}

// chipFor is the person's chip: their name on their colour. You are always
// the "you" colour, the model always its own.
func (m *View) chipFor(id string) string {
	switch {
	case id == m.userID():
		return m.st.ChipYou.Render(" " + id + " ")
	case id == inbox.Reserved:
		return m.st.ChipAgent.Render(" " + id + " ")
	}
	if len(m.st.Chips) == 0 {
		return m.st.ChipYou.Render(" " + id + " ")
	}
	return m.st.Chips[userColorIndex(id, len(m.st.Chips))].Render(" " + id + " ")
}

// nameStyle is the person's colour as a foreground, for an @name inline.
func (m *View) nameStyle(id string) lipgloss.Style {
	switch {
	case id == m.userID():
		return m.st.NameYou
	case id == inbox.Reserved:
		return m.st.NameAgent
	}
	if len(m.st.Names) == 0 {
		return m.st.NameYou
	}
	return m.st.Names[userColorIndex(id, len(m.st.Names))]
}

// colorMentions draws every @name in text in that person's colour, the
// name spelled as typed ("@Alice." keeps its capital and its full stop).
// "@agent" is the model's own mention and takes the model's colour.
func (m *View) colorMentions(text string) string {
	return userMentionRe.ReplaceAllStringFunc(text, func(s string) string {
		at := strings.Index(s, "@")
		lead, name := s[:at], s[at+1:]
		trimmed := strings.TrimRight(name, ".-_")
		id := strings.ToLower(trimmed)
		if id != inbox.Reserved {
			var err error
			if id, err = inbox.ValidID(trimmed); err != nil {
				return s
			}
		}
		return lead + m.nameStyle(id).Bold(true).Render("@"+trimmed) + name[len(trimmed):]
	})
}

// namesMe reports whether a line addresses this terminal's user.
func (m *View) namesMe(text string) bool {
	me := m.userID()
	if me == "" {
		return false
	}
	for _, id := range MentionedUsers(text) {
		if id == me {
			return true
		}
	}
	return false
}

// dividerGap is the quiet between two lines that earns a time divider.
const dividerGap = 10 * time.Minute

// divider is the dim centred marker between prev and cur: the day when it
// changed, the time when more than dividerGap passed, else "".
func (m *View) divider(prev, cur time.Time, width int) string {
	if prev.IsZero() {
		return ""
	}
	py, pm, pd := prev.Date()
	cy, cm, cd := cur.Date()
	label := ""
	switch {
	case py != cy || pm != cm || pd != cd:
		ty, tm, td := m.now().Date()
		if cy == ty && cm == tm && cd == td {
			label = "today"
		} else if m.now().Sub(cur) < 6*24*time.Hour {
			label = cur.Format("Mon")
		} else {
			label = cur.Format("2 Jan")
		}
	case cur.Sub(prev) >= dividerGap:
		label = cur.Format("15:04")
	default:
		return ""
	}
	w := width
	if w <= 0 {
		w = 40
	}
	side := (w - lipgloss.Width(label) - 2) / 2
	if side < 3 {
		side = 3
	}
	return m.st.Dim.Render(strings.Repeat("─", side) + " " + label + " " + strings.Repeat("─", side))
}

// mentionBar is the bar a line that names you carries on its left.
func (m *View) mentionBar(named bool) string {
	if named {
		return m.st.Mention.Render("▐")
	}
	return " "
}

// presence is the "● online" / "○ away" tag for an ID, answered from a
// per-view cache (onlineTTL) since a render must not scan the live records.
func (m *View) presence(id string) string {
	var on bool
	if id == m.dm.with {
		// The open thread's answer is withOnline's: one scan for the
		// title and the footer alike.
		on = m.withOnline()
	} else {
		if m.dm.presenceAt.IsZero() || m.now().Sub(m.dm.presenceAt) > onlineTTL {
			// One read of the live records answers every row until the
			// TTL passes, however many correspondents the inbox lists.
			m.dm.presence = m.liveUsers()
			m.dm.presenceAt = m.now()
		}
		on = m.dm.presence[id]
	}
	if on {
		return m.st.OK.Render("● online")
	}
	return m.st.Dim.Render("○ away")
}
