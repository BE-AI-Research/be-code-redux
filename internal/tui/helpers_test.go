package tui

import (
	"context"
	"fmt"
	"strings"
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
	// NewSession points usersPath and inboxDir at the real ~/.be-code/users.json
	// and ~/.be-code/inbox (under TestMain's throwaway HOME, so this never
	// reaches the developer's own dotdir either way); a test session gets
	// neither, so identity resolution and the mailbox never touch disk
	// unless a test wires resolveFn/bindFn or sets its own temp inboxDir.
	s.usersPath = ""
	s.inboxDir = ""
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

// testViewSeq numbers the views newTestView hands out, so two terminals
// added to one session never collide on the id the roster keys them by.
var testViewSeq int

// newTestView adds one more terminal to an existing session, at 80x24, the
// way a second attach would.
func newTestView(t *testing.T, s *Session) *View {
	t.Helper()
	testViewSeq++
	v := s.NewView(testViewSeq, fmt.Sprintf("term%d", testViewSeq))
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return v
}

// transcriptText is everything this view has rendered into its own buffer
// so far, exactly as its terminal would show it.
func (m *View) transcriptText() string { return m.rendered.String() }

// isRunning reports the shared run state, locked, for a test goroutine that
// does not hold mu (unlike idle, cowork_test's own version, which assumes the
// caller already does).
func (s *Session) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// transcriptText is the session-wide transcript (every entry's label and
// text, one per line) before any view renders it — the source a two-terminal
// test checks against, as opposed to (*View).transcriptText's per-view
// rendered copy.
func transcriptText(s *Session) string {
	var b strings.Builder
	for _, e := range s.Entries() {
		b.WriteString(e.Label)
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// lastEntryText is the newest transcript entry's text (label plus body), for
// a test that ran a command through the session directly (not through
// Update) and wants to check what it told the terminal.
func lastEntryText(s *Session) string {
	e := s.Entries()
	if len(e) == 0 {
		return ""
	}
	last := e[len(e)-1]
	return last.Label + last.Text
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

// drainAll delivers every broadcast queued for the given views the way
// Bubble Tea itself would: through Update, not straight into drainInto (which
// flush uses to skip the mutex a real program always holds). A chat test
// wants that: PostLocked can be called while a view's own Update still holds
// the lock, so the broadcast it raises must be picked up the same way any
// other terminal's mailbox poke is — a drainMsg through Update — not by
// reaching past it.
func drainAll(t *testing.T, views ...*View) {
	t.Helper()
	for _, v := range views {
		for {
			v.Update(drainMsg{})
			if len(v.mb.ch) == 0 {
				break
			}
		}
	}
}

// hasQuit reports whether cmd is tea.Quit, or a batch containing it —
// Update batches whatever its own mailbox drain produced onto its result, so
// a quit decided by a drained message can arrive either way.
func hasQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tea.QuitMsg:
		return true
	case tea.BatchMsg:
		for _, c := range msg {
			if hasQuit(c) {
				return true
			}
		}
	}
	return false
}

// setClients applies a roster to the session and delivers the resulting
// messages to v, the way the host callback plus the mailbox goroutine would.
func setClients(v *View, infos ...live.ClientInfo) {
	v.SetClients(infos)
	flush(v)
}

// runes is the KeyMsg for typed text.
func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// twoClients is one view of a served session with a two-terminal roster: the
// shape tests need when what they exercise is the roster (labels, prefixes,
// live-session switching), not per-terminal rendering. Use twoViews when the
// test is about two terminals each running their own program.
func twoClients(t *testing.T) *View {
	t.Helper()
	m := newTestModel(t)
	m.served = true
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	setClients(m, live.ClientInfo{ID: 1, Label: "desk (pid 1)", UTF8: true}, live.ClientInfo{ID: 2, Label: "tablet (pid 2)", UTF8: true})
	return m
}

// queued takes everything waiting on a channel without delivering it, for a
// test that cares what was sent rather than what the view did with it.
func queued(ch <-chan tea.Msg) []tea.Msg {
	var out []tea.Msg
	for {
		select {
		case msg := <-ch:
			out = append(out, msg)
		default:
			return out
		}
	}
}

// pump delivers everything a program has been sent — its own control queue
// first, then the session broadcasts — the way Bubble Tea and the mailbox
// goroutine would. Tests use it in place of running a real program.
func pump(prs ...*program) {
	for _, pr := range prs {
		for _, msg := range queued(pr.ctrl) {
			pr.v.update(msg)
		}
		flush(pr.v)
	}
}
