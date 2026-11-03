package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestCoworkerAddPlainFlowWritesAndRegisters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	r.Cfg.Providers["far"] = config.ProviderConfig{Type: "openai", BaseURL: "https://api.example.com/v1"}
	if err := r.Cfg.Save(); err != nil {
		t.Fatal(err)
	}
	old := coworkerModels
	coworkerModels = func(ctx context.Context, cfg *config.Config, name string) ([]provider.ModelDetail, error) {
		if name != "far" {
			return nil, errors.New("wrong provider")
		}
		return []provider.ModelDetail{{ID: "vendor/claude-x:latest"}, {ID: "m2"}}, nil
	}
	t.Cleanup(func() { coworkerModels = old })
	// providers sorted: be-ai-engine, far, llamacpp, … → 2 = far; model 1;
	// name default; sub-agent yes
	p := &scriptedPrompter{answers: []string{"2", "1", "", "y"}}
	name, err := CoworkerAdd(context.Background(), r.Cfg, r.Agent, p)
	if err != nil || name != "claude-x" {
		t.Fatalf("%q %v\n%s", name, err, p.all())
	}
	out := p.all()
	for _, want := range []string{"[2] far          https://api.example.com/v1 (online)", "[1] vendor/claude-x:latest", "name for this co-worker [claude-x]:", "co-worker claude-x added (far/vendor/claude-x:latest)", "it is online", "from the next launch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	cws := r.Agent.Coworkers()
	if len(cws) != 1 || !cws[0].Online || !cws[0].SubAgent {
		t.Fatalf("%+v", cws)
	}
	if _, ok := r.Agent.Tools.Tool("consult"); !ok {
		t.Fatal("consult not registered")
	}
	re, _ := config.Load()
	if len(re.Coworkers) != 1 || re.Coworkers[0].Name != "claude-x" || !re.Coworkers[0].Online {
		t.Fatalf("saved: %+v", re.Coworkers)
	}
	line, err := RemoveCoworker(r.Cfg, r.Agent, "claude-x")
	if err != nil || line != "co-worker claude-x removed" {
		t.Fatalf("%q %v", line, err)
	}
	re, _ = config.Load()
	if len(re.Coworkers) != 0 || r.Agent.HasCoworkers() {
		t.Fatal("not removed")
	}
	if !strings.Contains(capture(t, func() { r.command(context.Background(), "/coworkers remove nobody") }), "no co-worker named") {
		t.Fatal("remove of a missing name must say so")
	}
}

func TestCoworkerAddIsNotBusySafe(t *testing.T) {
	for line, want := range map[string]bool{"/coworkers": true, "/coworkers add": false, "/coworkers remove x": false} {
		if got := BusySafeCommand(line); got != want {
			t.Errorf("%q: %v", line, got)
		}
	}
}

func TestSuggestCoworkerName(t *testing.T) {
	for in, want := range map[string]string{"qwen3:32b": "qwen3", "anthropic/claude-x:latest": "claude-x", "gpt-4o": "gpt-4o"} {
		if got := SuggestCoworkerName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
