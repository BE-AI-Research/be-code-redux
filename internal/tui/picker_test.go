package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// tempHome points the dotdir at a temp directory: no test may read or
// write the real ~/.be-code, whatever a missing stub might fall back to.
func tempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir()) // windows
}

// The session picker is the path that used to fork a live session: picking
// a row whose code is already live must hand this terminal over to that
// session's host instead of loading its file into a second program. The
// switch goes out as a tea.Cmd — calling the host from inside Update would
// deadlock the program (see /detach).
func TestSessionPickerMarksLiveRowsAndSwitches(t *testing.T) {
	tempHome(t)
	m := twoClients(t)
	var switched []string
	m.switchClient = func(id int, code string) { switched = append(switched, fmt.Sprint(id, ":", code)) }
	m.liveCodes = func() map[string]bool { return map[string]bool{"ABC123": true} }
	sessions := map[string]*store.Session{
		"1": {ID: "1", Code: "ABC123", Title: "live one"},
		"2": {ID: "2", Code: "DEF456", Title: "saved"},
	}
	m.loadSession = func(id string) (*store.Session, error) { return sessions[id], nil }

	items := m.sessionItems([]store.Meta{
		{ID: "1", Code: "ABC123", Title: "live one", UpdatedAt: time.Now()},
		{ID: "2", Code: "DEF456", Title: "saved", UpdatedAt: time.Now()},
	})
	if len(items) != 2 || !strings.Contains(items[0].label, "LIVE") || strings.Contains(items[1].label, "LIVE") {
		t.Fatalf("live marking: %+v", items)
	}

	_, cmd := m.resumeFrom("1", 2) // client 2 picked the live row
	if cmd == nil {
		t.Fatal("picking a live row must return a switch command")
	}
	cmd()
	if len(switched) != 1 || switched[0] != "2:ABC123" {
		t.Fatalf("switch: %v", switched)
	}
	if !m.switchPending {
		t.Fatal("a switch leaves the host waiting to see whether anyone is left")
	}

	// A row that is not live still loads into this program.
	switched = nil
	if _, cmd := m.resumeFrom("2", 2); cmd != nil {
		t.Fatal("a cold session must not switch")
	}
	if len(switched) != 0 {
		t.Fatalf("switch on a cold session: %v", switched)
	}
	if m.ag.Session == nil || m.ag.Session.ID != "2" {
		t.Fatalf("cold session was not resumed: %+v", m.ag.Session)
	}
}

// An in-process TUI (--no-host) has no host to switch through, so it says
// how to join the live session by hand and loads nothing.
func TestInProcessResumeOfALiveCodePrintsInsteadOfLoading(t *testing.T) {
	tempHome(t)
	m := newTestModel(t)
	m.liveCodes = func() map[string]bool { return map[string]bool{"ABC123": true} }
	m.loadSession = func(id string) (*store.Session, error) {
		return &store.Session{ID: id, Code: "ABC123", Title: "live one"}, nil
	}
	before := m.ag.Session
	m.resumeFrom("1", 0)
	if m.ag.Session != before {
		t.Fatal("an in-process resume of a live code must load nothing")
	}
	if !strings.Contains(m.transcript.String(), "ABC123 is live elsewhere; join it with: be-code attach ABC123") {
		t.Fatalf("in-process resume of a live code:\n%s", m.transcript.String())
	}
}

// The host's own session is live in the records by definition; re-picking it
// must not switch the terminal to itself.
func TestResumingTheSessionThisProgramAlreadyRunsIsANoSwitch(t *testing.T) {
	tempHome(t)
	m := twoClients(t)
	var switched int
	m.switchClient = func(id int, code string) { switched++ }
	m.liveCodes = func() map[string]bool { return map[string]bool{"ABC123": true} }
	m.ag.Session = &store.Session{ID: "1", Code: "ABC123"}
	m.loadSession = func(id string) (*store.Session, error) {
		return &store.Session{ID: "1", Code: "ABC123", Title: "mine"}, nil
	}
	if _, cmd := m.resumeFrom("1", 1); cmd != nil {
		t.Fatal("resuming this program's own session must not switch")
	}
	if switched != 0 {
		t.Fatalf("switched %d times", switched)
	}
}

// After a switch, a fresh host with no turns and nobody watching quits
// rather than lingering; one with turns, or with a client left, keeps
// running.
func TestEmptyHostQuitsAfterTheLastClientSwitchesAway(t *testing.T) {
	tempHome(t)
	// The switch emptied the roster: nothing left to render for.
	m := twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999"}
	m.switchPending = true
	if cmd := m.updateClients(clientsMsg{}); cmd == nil {
		t.Fatal("an empty fresh host must quit after a switch")
	}

	// A client left behind keeps the host running, and forgets the switch:
	// that client was told the session is still running, so its own later
	// detach must not quit it.
	m = twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999"}
	m.switchPending = true
	if cmd := m.updateClients(clientsMsg{{ID: 1, Label: "desk", UTF8: true}}); cmd != nil {
		t.Fatal("a host with a client left must keep running")
	}
	if m.switchPending {
		t.Fatal("a non-empty roster must clear the pending switch")
	}
	if cmd := m.updateClients(clientsMsg{}); cmd != nil {
		t.Fatal("the remaining client detaching later must not quit the session")
	}

	// With turns recorded, the session outlives its terminals.
	m = twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}}
	m.switchPending = true
	if cmd := m.updateClients(clientsMsg{}); cmd != nil {
		t.Fatal("a session with turns must keep running")
	}

	// A run in flight keeps the host alive even before its first turn has
	// reached the session file.
	m = twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999"}
	m.switchPending = true
	m.running = true
	if cmd := m.updateClients(clientsMsg{}); cmd != nil {
		t.Fatal("a host with a run in flight must keep running")
	}

	// Without a switch, an empty roster is just everyone detaching.
	m = twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999"}
	if cmd := m.updateClients(clientsMsg{}); cmd != nil {
		t.Fatal("detaching is not switching; the session keeps running")
	}

	// A client arriving cancels the pending switch.
	m = twoClients(t)
	m.ag.Session = &store.Session{ID: "1", Code: "ZZZ999"}
	m.clients = nil
	m.switchPending = true
	m.updateClients(clientsMsg{{ID: 3, Label: "new", UTF8: true}})
	if m.switchPending {
		t.Fatal("an attach must clear the pending switch")
	}
}
