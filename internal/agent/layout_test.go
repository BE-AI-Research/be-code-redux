package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// readTwice scripts two reads and an answer: three requests, with Working
// memory changing between every one of them.
func readTwice(t *testing.T, mut func(*config.Config)) (*Agent, *funcProvider) {
	t.Helper()
	n := 0
	p := &funcProvider{}
	p.fn = func(provider.ChatRequest) (*provider.ChatResponse, error) {
		n++
		switch n {
		case 1:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}}, nil
		case 2:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "read_file", Arguments: `{"path":"b.go"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}
	ag, dir := newTestAgent(t, p, mut)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n\nfunc B() {}\n"), 0o644)
	withEngine(t, ag)
	if _, err := ag.Run(context.Background(), "look at both files"); err != nil {
		t.Fatal(err)
	}
	return ag, p
}

func withoutTail(s string) string {
	if i := strings.Index(s, tailHeader); i >= 0 {
		return s[:i]
	}
	return s
}

// The point of the cached layout, stated as the server sees it: each request
// is the previous one, byte for byte, up to the end of the previous request's
// last message — so a prefix cache covers everything but what is new. Measured
// before this on a real five-read task: nine cache misses in nine requests,
// 225 of 296 seconds spent re-reading a prompt that had barely changed.
func TestEachRequestExtendsThePreviousOneByteForByte(t *testing.T) {
	_, p := readTwice(t, nil)
	reqs := p.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(reqs))
	}
	changed := false
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1].Messages, reqs[i].Messages
		if len(cur) <= len(prev) {
			t.Fatalf("request %d did not grow", i)
		}
		for j := range prev {
			want := prev[j].Content
			if j == len(prev)-1 {
				want = withoutTail(want) // the tail is the one part that is re-sent elsewhere
			}
			if got := cur[j].Content; got != want {
				t.Fatalf("request %d changed message %d, which the server had cached:\n%q\nvs\n%q", i, j, got, want)
			}
		}
		if requestState(reqs[i]) != requestState(reqs[i-1]) {
			changed = true
		}
		last := cur[len(cur)-1].Content
		if !strings.Contains(last, tailHeader) || !strings.Contains(last[strings.Index(last, tailHeader):], "Working memory:") {
			t.Fatalf("request %d carries no Working memory at its end:\n%s", i, last)
		}
	}
	if !changed {
		t.Fatal("Working memory never changed between requests; this test would pass in any layout")
	}
	if strings.Contains(reqs[2].Messages[0].Content, "Working memory:\n") {
		t.Fatal("Working memory is still in the system prompt")
	}
}

// The tail is a property of what is sent, never of what is kept: a saved
// session, a replay, a compaction and the next request all see the message
// as it was.
func TestTheHistoryNeverCarriesTheTail(t *testing.T) {
	ag, _ := readTwice(t, nil)
	for i, m := range ag.History.Messages {
		if strings.Contains(m.Content, tailHeader) || strings.Contains(m.Content, "Working memory:\n") {
			t.Fatalf("message %d kept the tail: %q", i, m.Content)
		}
	}
}

// The tail is counted: it is sent with every request and no compaction can
// remove it, so it belongs to the floor the budget is measured against.
func TestTheTailIsCountedInTheBudget(t *testing.T) {
	ag, _ := readTwice(t, nil)
	tail := ag.volatileTail()
	if tail == "" {
		t.Fatal("no tail to count")
	}
	if min := ag.History.est(tail); ag.History.Extra < min {
		t.Fatalf("Extra is %d, less than the tail's own %d tokens", ag.History.Extra, min)
	}
}

// prompt_layout: "classic" is the arrangement every version before this had.
func TestTheClassicLayoutKeepsWorkingMemoryInTheSystemPrompt(t *testing.T) {
	_, p := readTwice(t, func(c *config.Config) { c.PromptLayout = "classic" })
	reqs := p.requests()
	last := reqs[2]
	if !strings.Contains(last.Messages[0].Content, "\n\nWorking memory:\n") {
		t.Fatal("classic: Working memory is not in the system prompt")
	}
	for _, m := range last.Messages[1:] {
		if strings.Contains(m.Content, tailHeader) {
			t.Fatalf("classic: a message carries the tail: %q", m.Content)
		}
	}
}
