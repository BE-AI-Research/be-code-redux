package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/packsrc"
)

// openPackPicker is what the menu's "Install a skill pack" row opens, and
// what `/packs add` with no name opens: the packs the configured source
// offers, so a person does not have to know a pack's name to install one
// (spec §1, §9).
//
// The rows are loaded by openPicker's Cmd, which runs on Bubble Tea's own
// goroutine rather than inside Update — that is what makes the clone this
// does safe. Picking hands the install to packsCmd, the same off-thread path
// `/packs add <name>` takes, because the install raises pack_install through
// Session.Ask and Ask must never be waited on under the lock Update holds.
func (m *View) openPackPicker() (tea.Model, tea.Cmd) {
	spec := ""
	if m.cfg != nil {
		spec = m.cfg.Packs.Source
	}
	s, err := packsrc.Resolve(spec, "")
	if err != nil {
		m.renderLocalNote(err.Error())
		return m, nil
	}
	title := fmt.Sprintf("Install a skill pack — from %s", s.Spec)
	return m.openPicker(title,
		func() ([]pickItem, error) {
			avail, err := packsrc.List(m.Session.baseCtx(), s)
			if err != nil {
				return nil, err
			}
			items := make([]pickItem, 0, len(avail))
			for _, a := range avail {
				desc := a.Version
				if a.Description != "" {
					desc += " · " + a.Description
				}
				items = append(items, pickItem{id: a.Name, label: a.Name, desc: desc})
			}
			return items, nil
		},
		func(m *View, it pickItem) (tea.Model, tea.Cmd) {
			return m, m.packsCmd([]string{"/packs", "add", it.id})
		})
}
