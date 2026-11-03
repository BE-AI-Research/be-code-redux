// internal/tui/coworkeradd.go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/ui"
)

type coworkerStep int

const (
	cwStepProvider coworkerStep = iota // picker: configured providers
	cwStepModel                        // picker: the provider's listing (or typed when empty)
	cwStepName                         // input line: a name, default suggested
	cwStepSubAgent                     // picker: may it own task steps?
)

// coworkerAddFlow is one terminal's /coworkers add in progress: pick a
// provider, pick a model from its live listing, name it, decide whether it
// may own task steps, then ui.AddCoworker writes and registers it.
type coworkerAddFlow struct {
	step     coworkerStep
	provider string
	model    string
	name     string
}

// coworkerModels lists a provider's models for the picker; tests stub it.
var coworkerModels = func(v *View, name string) ([]provider.ModelDetail, error) {
	p, err := provider.FromConfig(v.cfg, name)
	if err != nil {
		return nil, err
	}
	return provider.ModelDetails(v.Session.baseCtx(), p)
}

// startCoworkerAdd is /coworkers add and the menu row "Add a co-worker".
// Not while a run is in flight: it rewrites the tool catalog and the
// prompt.
func (m *View) startCoworkerAdd() (tea.Model, tea.Cmd) {
	if m.running {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the agent is done (Esc cancels); plain text is queued"})
		return m, nil
	}
	if m.onlineAdd != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: "finish or cancel /online add first"})
		return m, nil
	}
	names := ui.ProviderNames(m.cfg)
	if len(names) == 0 {
		m.appendEntryLocked(entry{Kind: entryErr, Text: "no providers are configured"})
		return m, nil
	}
	f := &coworkerAddFlow{step: cwStepProvider}
	m.coworkerAdd = f
	items := make([]pickItem, 0, len(names))
	for _, n := range names {
		desc := m.cfg.Providers[n].BaseURL
		if config.ProviderIsOnline(m.cfg.Providers[n]) {
			desc += " (online)"
		}
		items = append(items, pickItem{id: n, label: n, desc: desc})
	}
	return m.openFlowPicker("Add a co-worker: provider", items, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
		f := v.coworkerAdd
		if f == nil {
			return v, nil
		}
		v.closeFlowPicker()
		f.provider, f.step = it.id, cwStepModel
		return v.openPicker("Add a co-worker: model on "+it.id, func() ([]pickItem, error) {
			models, err := coworkerModels(v, it.id)
			if err != nil {
				return nil, err
			}
			items := make([]pickItem, 0, len(models))
			for _, mo := range models {
				items = append(items, pickItem{id: mo.ID, label: mo.ID, desc: mo.Describe()})
			}
			return items, nil
		}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			if v.coworkerAdd == nil {
				return v, nil
			}
			v.closeFlowPicker()
			return v.coworkerAskName(it.id)
		})
	})
}

func (m *View) coworkerAskName(model string) (tea.Model, tea.Cmd) {
	f := m.coworkerAdd
	f.model, f.step = model, cwStepName
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("name for this co-worker (Enter keeps %s):", ui.SuggestCoworkerName(model))})
	return m, nil
}

// coworkerLine is the Enter handler's hook for the name step; true when
// the line belonged to the flow.
func (m *View) coworkerLine(text string) (bool, tea.Cmd) {
	f := m.coworkerAdd
	if f == nil {
		return false, nil
	}
	switch f.step {
	case cwStepName:
		f.name = strings.TrimSpace(text)
		if f.name == "" {
			f.name = ui.SuggestCoworkerName(f.model)
		}
		f.step = cwStepSubAgent
		_, cmd := m.openFlowPicker(fmt.Sprintf("may %s own task steps as a sub-agent?", f.name), []pickItem{
			{id: "no", label: "no", desc: "consult only"},
			{id: "yes", label: "yes", desc: "assignable to task steps from the next launch"},
		}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			if v.coworkerAdd == nil {
				return v, nil
			}
			v.closeFlowPicker()
			return v.coworkerFinish(it.id == "yes")
		})
		return true, cmd
	case cwStepModel:
		// A provider that lists nothing: the model typed on the line.
		if t := strings.TrimSpace(text); t != "" {
			_, cmd := m.coworkerAskName(t)
			return true, cmd
		}
		return true, nil
	}
	return true, nil
}

func (m *View) coworkerFinish(sub bool) (tea.Model, tea.Cmd) {
	f := m.coworkerAdd
	m.coworkerAdd = nil
	line, err := ui.AddCoworker(m.cfg, m.ag, config.CoworkerConfig{Name: f.name, Provider: f.provider, Model: f.model, SubAgent: sub})
	if err != nil {
		m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
		return m, nil
	}
	m.appendEntryLocked(entry{Kind: entryOK, Text: line})
	return m, nil
}

// cancelCoworkerAdd is Esc anywhere in the flow: nothing was written.
func (m *View) cancelCoworkerAdd() {
	if m.coworkerAdd == nil {
		return
	}
	m.coworkerAdd = nil
	m.appendEntryLocked(entry{Kind: entryDim, Text: "cancelled; nothing was added"})
}
