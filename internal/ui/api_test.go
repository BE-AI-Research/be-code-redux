package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/store"
)

type fakeControl struct {
	st      agent.APIStatus
	started int
	stopped int
}

func (f *fakeControl) Start() error            { f.started++; f.st.Serving = true; return nil }
func (f *fakeControl) Stop()                   { f.stopped++; f.st.Serving = false }
func (f *fakeControl) Status() agent.APIStatus { return f.st }

func TestAPICommandReportsAndToggles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	fc := &fakeControl{st: agent.APIStatus{Socket: "/tmp/x/api.sock", ServedBy: "abc123", ServedByPID: 42}}
	r.Agent.API = fc
	out := APICommand(r.Agent, []string{"/api"})
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "not serving") {
		t.Fatalf("status names who serves:\n%s", out)
	}
	if out := APICommand(r.Agent, []string{"/api", "on"}); fc.started != 1 || !strings.Contains(out, "serving") {
		t.Fatalf("/api on starts: %d %q", fc.started, out)
	}
	fc.st.Served, fc.st.LastApp, fc.st.LastAt = 3, "ue5-helper", time.Now()
	if out := APICommand(r.Agent, []string{"/api"}); !strings.Contains(out, "3 requests") || !strings.Contains(out, "ue5-helper") {
		t.Fatalf("status counts requests and names the last app:\n%s", out)
	}
	if out := APICommand(r.Agent, []string{"/api", "off"}); fc.stopped != 1 || !strings.Contains(out, "stopped") {
		t.Fatalf("/api off stops: %d %q", fc.stopped, out)
	}
	r.Agent.API = nil
	if out := APICommand(r.Agent, []string{"/api", "on"}); !strings.Contains(out, "cannot serve") {
		t.Fatalf("a session with no control says so: %q", out)
	}
}

func TestSessionCommandNeverPutsTheTokenInTheNote(t *testing.T) {
	r := newTestREPL(t)
	t.Setenv("HOME", t.TempDir())
	r.Agent.SetSession(store.NewSession("null", "m", r.Agent.Tools.Root))
	secret, note, err := SessionCommand(r.Agent, []string{"/session", "token"})
	if err != nil || !strings.HasPrefix(secret, "sess_1_") || strings.Contains(note, secret) {
		t.Fatalf("%v %q %q", err, secret, note)
	}
	secret2, note2, err := SessionCommand(r.Agent, []string{"/session", "rotate", "5m"})
	if err != nil || secret2 == secret || strings.Contains(note2, secret2) || !strings.Contains(note2, "5m") {
		t.Fatalf("%v %q %q", err, secret2, note2)
	}
	if _, _, err := SessionCommand(r.Agent, []string{"/session", "rotate", "48h"}); err == nil {
		t.Fatal("a grace over 24h is refused")
	}
	if _, _, err := SessionCommand(r.Agent, []string{"/session", "dance"}); err == nil {
		t.Fatal("an unknown verb is an error")
	}
	_ = store.MaxTokenGrace
}

func TestSlashTableListsTheNewCommands(t *testing.T) {
	want := map[string]bool{"/api": false, "/apps": false, "/session": false}
	for _, c := range SlashCommandTable {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s is not in SlashCommandTable", name)
		}
	}
	for _, name := range []string{"/api", "/apps", "/session token"} {
		if !BusySafeCommand(name) {
			t.Errorf("%s must be busy-safe: it never touches the turn", name)
		}
	}
}
