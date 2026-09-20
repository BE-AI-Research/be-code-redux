package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// The server's own account of reading the prompt is added up for /stats, and
// a slow read — the prompt cache did not cover the request — is said once,
// as a status line, with the numbers.
func TestPromptProcessingTimeIsRecordedAndASlowReadIsSaid(t *testing.T) {
	durations := []time.Duration{43 * time.Second, 300 * time.Millisecond}
	call := 0
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		d := durations[call]
		call++
		return &provider.ChatResponse{Content: "ok", Usage: provider.Usage{
			PromptTokens: 19461, CompletionTokens: 2, PromptDuration: d, LoadDuration: 2 * time.Second}}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	var status []string
	ag.Events.OnTransient = func(m string) { status = append(status, m) }

	if _, err := ag.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if len(status) == 0 || !strings.Contains(strings.Join(status, "\n"), "read 19461 prompt tokens in 43s") {
		t.Fatalf("a 43s prompt read was not reported: %q", status)
	}
	status = nil
	if _, err := ag.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(status, "\n"), "prompt tokens in") {
		t.Fatalf("a cached read is not news: %q", status)
	}
	u := ag.Usage()
	if u.PromptTime != 43*time.Second+300*time.Millisecond || u.LoadTime != 4*time.Second || u.SlowReads != 1 {
		t.Fatalf("usage: %+v", u)
	}
}
