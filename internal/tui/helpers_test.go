package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// newTestSession builds a Session over a throwaway workspace and a provider
// that answers nothing. Each prep func runs on the agent before the session
// is created, for state the UI reads at startup (e.g. an attached editor).
func newTestSession(t *testing.T, prep ...func(*agent.Agent)) *Session {
	t.Helper()
	cfg := config.Default()
	cfg.RepoMap = false
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, nullProvider{}, "m", reg, "")
	for _, f := range prep {
		f(ag)
	}
	s := NewSession(cfg, ag, nullProvider{})
	s.rootCtx = context.Background()
	return s
}

// newTestModel keeps its name: one view of a fresh session at 80x24, id 0.
func newTestModel(t *testing.T, prep ...func(*agent.Agent)) *View {
	t.Helper()
	s := newTestSession(t, prep...)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return v
}

// flush delivers every queued broadcast to the given views, in order.
// Nothing runs a view's mailbox goroutine in a test, so a broadcast raised
// outside Update (which drains its own mailbox) waits here until asked for.
func flush(views ...*View) {
	for _, v := range views {
		if mb := v.mailboxForTest(); mb != nil {
			mb.drainInto(v)
		}
	}
}

// setClients applies a roster to the session and delivers the resulting
// messages to v, the way the host callback plus the mailbox goroutine would.
func setClients(v *View, infos ...live.ClientInfo) {
	v.SetClients(infos)
	flush(v)
}
