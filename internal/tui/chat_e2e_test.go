package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// streamingReplyProvider answers with one canned reply, feeding it through
// onDelta first (the way a real backend's streamed tokens do) before
// returning it as the response's Content. This session's transcript entry
// for the model's answer comes from Session.flushLocked, which converts
// whatever has accrued in s.streaming (fed only by onDelta) — RunFull's own
// returned answer string is otherwise discarded by startTurnLocked. The
// package's existing scriptedProvider (cowork_test.go) never calls onDelta:
// it exists for the read-only consult scratch agent, whose answer the caller
// reads directly from the returned string, not through the session's
// transcript.
type streamingReplyProvider struct {
	nullProvider
	reply string
}

func (p streamingReplyProvider) Chat(_ context.Context, _ provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	if onDelta != nil {
		onDelta(p.reply)
	}
	return &provider.ChatResponse{Content: p.reply}, nil
}

// Two terminals on one session: a room line reaches both; a DM from one
// reaches the other's inbox; a mention produces one reply in the transcript
// and one in the room.
func TestTwoTerminalsChatDMAndMention(t *testing.T) {
	s := newTestSession(t, func(ag *agent.Agent) {
		ag.Provider = streamingReplyProvider{reply: "The picking tests need max_dist honoured."}
	})
	s.inboxDir = t.TempDir()
	s.onlineFn = func(string) bool { return true }
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{ID: tm.User, How: "config"}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "vscode (pid 1)", User: "alice"}, {ID: 2, Label: "ssh from 192.168.1.38 (pid 2)", User: "bob"}})
	a, b := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.38 (pid 2)")
	for _, v := range []*View{a, b} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		v.slashCommand("/chat")
	}
	a.input.SetValue("tests are red")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainAll(t, a, b)
	if !strings.Contains(b.View(), "alice: tests are red") {
		t.Fatalf("bob's room:\n%s", b.View())
	}
	b.input.SetValue("/dm alice")
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	b.input.SetValue("private: lunch?")
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ := inbox.Thread(s.inboxDir, "alice", "bob")
	s.deliverDM(m[0])
	drainAll(t, a, b)
	if a.unreadDMs() != 1 || strings.Contains(a.View(), "lunch?") {
		t.Fatalf("alice: unread %d; a DM must not appear in the room:\n%s", a.unreadDMs(), a.View())
	}
	// Bob leaves the DM thread for the room again: /dm has its own view
	// (dm.go's viewDM), so what follows in the room only shows up once he's
	// back in /chat — the same way a person would check it.
	b.slashCommand("/chat")
	a.input.SetValue("@agent why are they red?")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, func() bool { return !s.isRunning() })
	drainAll(t, a, b)
	room := s.Room()
	last := room[len(room)-1]
	if last.User != "agent" || !strings.Contains(last.Text, "max_dist") {
		t.Fatalf("room reply: %+v", last)
	}
	if n := strings.Count(transcriptText(s), "max_dist"); n != 1 {
		t.Fatalf("transcript has the answer %d times", n)
	}
	if !strings.Contains(b.View(), "agent: The picking tests") {
		t.Fatalf("bob did not see the reply:\n%s", b.View())
	}
}
