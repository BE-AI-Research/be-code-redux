package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func twoClients(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	m.served = true
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	return m
}

func TestEachClientTypesIntoItsOwnInput(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("hello")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("world")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("!")})
	if got := m.inputFor(1).Value(); got != "hello!" {
		t.Fatalf("client 1 input %q", got)
	}
	if got := m.inputFor(2).Value(); got != "world" {
		t.Fatalf("client 2 input %q", got)
	}
	if !strings.Contains(m.View(), m.blankInputRows()) {
		t.Fatalf("served view must leave %d blank input rows:\n%s", m.inputRows(), m.View())
	}
	if strings.Contains(m.View(), "hello!") || strings.Contains(m.View(), "world") {
		t.Fatal("shared frame must not render any client's input text")
	}
}

func TestEnterQueuesOnlyTheSendersTextWithLabel(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("do it later")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("mine")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	items := m.ag.Items()
	if len(items) != 1 || items[0].Text != "do it later" || items[0].From != 2 {
		t.Fatalf("queue: %+v", items)
	}
	if got := m.inputFor(1).Value(); got != "mine" {
		t.Fatalf("client 1's draft was disturbed: %q", got)
	}
	if !strings.Contains(m.transcript.String(), "tablet (pid 2)> ") {
		t.Fatalf("queued line lacks the sender label:\n%s", m.transcript.String())
	}
}

func TestTranscriptPrefixUsesLabelOnlyWithSeveralClients(t *testing.T) {
	m := newTestModel(t)
	if got := m.userPrefix(0); got != "you> " {
		t.Fatalf("single client prefix %q", got)
	}
	m = twoClients(t)
	if got := m.userPrefix(1); got != "desk (pid 1)> " {
		t.Fatalf("multi client prefix %q", got)
	}
}

func TestQueuePopupShowsOnlyTheOpenersMessages(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.ag.EnqueueFrom("from one", 1)
	m.ag.EnqueueFrom("from two", 2)
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if m.mode != modeQueue {
		t.Fatal("Up on an empty input opens the queue popup")
	}
	v := m.View()
	if !strings.Contains(v, "from two") || strings.Contains(v, "from one") {
		t.Fatalf("popup must list only the opener's messages:\n%s", v)
	}
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}}) // edit
	if got := m.inputFor(2).Value(); got != "from two" {
		t.Fatalf("edit pulled %q into client 2's input", got)
	}
	if items := m.ag.Items(); len(items) != 1 || items[0].From != 1 {
		t.Fatalf("client 1's message must remain queued: %+v", items)
	}
}

func TestPaletteIsOwnedByTheClientThatOpenedIt(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/")})
	if m.mode != modePalette || m.paletteOwner != 2 {
		t.Fatalf("palette owner %d mode %v", m.paletteOwner, m.mode)
	}
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEsc}})
	if m.mode != modePalette {
		t.Fatal("another client's Esc must not close the owner's palette")
	}
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}}) // owner detached
	if m.mode == modePalette {
		t.Fatal("palette must close when its owner detaches")
	}
	if m.inputs[2] != nil {
		t.Fatal("detached client's textarea must be dropped")
	}
}

func TestInProcessModelStillUsesClientZero(t *testing.T) {
	m := newTestModel(t)
	m.Update(runes("abc"))
	if got := m.inputFor(0).Value(); got != "abc" {
		t.Fatalf("plain KeyMsg goes to client 0: %q", got)
	}
	if !strings.Contains(m.View(), "abc") {
		t.Fatal("in-process view renders the single textarea")
	}
}

func TestDetachCommandDetachesTheTypingClient(t *testing.T) {
	m := twoClients(t)
	var detached []int
	m.detachClient = func(id int) { detached = append(detached, id) }
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/detach")})
	_, cmd := m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if cmd != nil {
		cmd()
	}
	if len(detached) != 1 || detached[0] != 2 {
		t.Fatalf("detached %v", detached)
	}
}

func TestBottomLineListsClientLabels(t *testing.T) {
	m := twoClients(t)
	v := m.View()
	if !strings.Contains(v, "⧉ 2") || !strings.Contains(v, "desk (pid 1), tablet (pid 2)") {
		t.Fatalf("bottom line:\n%s", v)
	}
	if strings.Contains(v, "input:") || strings.Contains(v, "Ctrl+] t") {
		t.Fatal("holder text must be gone")
	}
}
