package tui

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/store"
)

type fakeAPI struct{ st agent.APIStatus }

func (f *fakeAPI) Start() error            { f.st.Serving = true; return nil }
func (f *fakeAPI) Stop()                   { f.st.Serving = false }
func (f *fakeAPI) Status() agent.APIStatus { return f.st }

func TestSessionTokenIsShownLocallyAndNeverStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestSession(t, func(ag *agent.Agent) {
		ag.SetSession(store.NewSession("null", "m", ag.Tools.Root))
	})
	v := newTestView(t, s)
	v.slashCommand("/session token")
	tok := s.ag.Session.Token
	if tok == "" || !strings.Contains(v.rendered.String(), tok) {
		t.Fatal("the token is rendered on this terminal")
	}
	for _, e := range s.Entries() {
		if strings.Contains(e.Text, tok) || strings.Contains(e.Label, tok) {
			t.Fatal("the token must not be a transcript entry")
		}
	}
	for _, l := range s.ag.Session.Chat {
		if strings.Contains(l.Text, tok) {
			t.Fatal("the token must not be in the chat")
		}
	}
}

func TestHeaderAndBottomLineMarkTheAPI(t *testing.T) {
	s := newTestSession(t)
	v := newTestView(t, s)
	v.width, v.height = 120, 40
	f := &fakeAPI{st: agent.APIStatus{Serving: true, Socket: "/tmp/x"}}
	s.ag.API = f
	if h := v.headerView(); !strings.Contains(h, "api") {
		t.Fatalf("the header marks a serving session: %q", h)
	}
	f.st.InFlight = "ue5-helper"
	if b := v.bottomLine(); !strings.Contains(b, "api: ue5-helper") {
		t.Fatalf("the bottom line names the app in flight: %q", b)
	}
	f.st.Serving = false
	if h := v.headerView(); strings.Contains(h, "api") {
		t.Fatalf("no marker when not serving: %q", h)
	}
}
