package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// pingPair is alice and bob attached to one session under their own IDs,
// with a mailbox of their own.
func pingPair(t *testing.T) (*Session, *View, *View) {
	t.Helper()
	s := newTestSession(t)
	s.inboxDir = t.TempDir()
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{ID: tm.User, How: "config"}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "a", User: "alice"}, {ID: 2, Label: "b", User: "bob"}})
	va, vb := s.NewView(1, "a"), s.NewView(2, "b")
	for _, v := range []*View{va, vb} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	return s, va, vb
}

func TestMentionedUsersNamesPeopleNotTheAgent(t *testing.T) {
	got := MentionedUsers("hey @Alice. and @bob-2, not @agent, a@b.c, @alice again")
	want := []string{"alice", "bob-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %q want %q", got, want)
	}
}

// A room line naming @alice prints one line in alice's transcript and
// nowhere else; the rest of the room, and alice's own lines, print nothing.
// A resize re-renders every view from the shared entries and keeps the same
// answer.
func TestAMentionPingsOnlyTheNamedUsersTranscript(t *testing.T) {
	s, va, vb := pingPair(t)
	s.Post("bob", "@alice look at this", "")
	s.Post("bob", "hello everyone", "")
	s.Post("alice", "@alice talking to myself", "")
	drainAll(t, va, vb)
	if n := strings.Count(va.transcriptText(), "User replied: bob (chat)"); n != 1 {
		t.Fatalf("alice saw %d pings:\n%s", n, va.transcriptText())
	}
	if strings.Contains(vb.transcriptText(), "User replied") {
		t.Fatalf("bob's transcript carries alice's ping:\n%s", vb.transcriptText())
	}
	for _, v := range []*View{va, vb} {
		v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	}
	drainAll(t, va, vb)
	if n := strings.Count(va.transcriptText(), "User replied: bob (chat)"); n != 1 {
		t.Fatalf("after a rebuild alice saw %d pings", n)
	}
	if strings.Contains(vb.transcriptText(), "User replied") {
		t.Fatal("a rebuild leaked alice's ping into bob's transcript")
	}
}

// /chat pings off is saved per user in config and silences mentions for
// that user alone; a DM always prints New DM (<from>) for its recipient.
func TestChatPingsOffIsPerUserAndNeverSilencesDMs(t *testing.T) {
	s, va, vb := pingPair(t)
	va.slashCommand("/chat pings off")
	if on := s.cfg.Chat.PingsOn("alice"); on {
		t.Fatal("alice's switch not saved")
	}
	if !s.cfg.Chat.PingsOn("bob") {
		t.Fatal("bob's switch moved with alice's")
	}
	s.Post("carol", "@alice and @bob", "")
	drainAll(t, va, vb)
	if strings.Contains(va.transcriptText(), "User replied") {
		t.Fatalf("alice was pinged with pings off:\n%s", va.transcriptText())
	}
	if !strings.Contains(vb.transcriptText(), "User replied: carol (chat)") {
		t.Fatalf("bob was not pinged:\n%s", vb.transcriptText())
	}
	m, _ := inbox.Send(s.inboxDir, "carol", "alice", "hey")
	s.deliverDM(m)
	drainAll(t, va, vb)
	if !strings.Contains(va.transcriptText(), "New DM (carol)") {
		t.Fatalf("alice's DM line missing:\n%s", va.transcriptText())
	}
	if strings.Contains(vb.transcriptText(), "New DM") {
		t.Fatal("bob's transcript carries alice's DM line")
	}
	va.slashCommand("/chat")
	if !strings.Contains(va.View(), "pings off") {
		t.Fatalf("the room footer does not say pings are off:\n%s", va.View())
	}
	va.slashCommand("/chat pings on")
	if !s.cfg.Chat.PingsOn("alice") {
		t.Fatal("alice's switch not restored")
	}
	if strings.Contains(va.View(), "pings off") {
		t.Fatal("the footer still says pings are off")
	}
}
