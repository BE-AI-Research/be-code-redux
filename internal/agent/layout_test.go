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

// The rule, stated as the server sees it: each request is the previous one,
// byte for byte, with more on the end. Nothing that was sent is changed or
// taken back. On the owner's server a strictly extending conversation costs
// about 6s a turn whatever its length; one whose last message lost a block on
// the next turn cost 11s, 18s, 25s, 41s, 39s over the same five reads.
func TestEachRequestStrictlyExtendsThePreviousOne(t *testing.T) {
	_, p := readTwice(t, nil)
	reqs := p.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1].Messages, reqs[i].Messages
		if len(cur) <= len(prev) {
			t.Fatalf("request %d did not grow", i)
		}
		for j := range prev {
			if cur[j].Content != prev[j].Content {
				t.Fatalf("request %d changed message %d, which the server had cached:\n%q\nvs\n%q", i, j, cur[j].Content, prev[j].Content)
			}
		}
	}
	// The state went out once, on the request's own message, and stayed.
	first := reqs[0].Messages[1].Content
	if !strings.HasPrefix(first, "look at both files") || !strings.Contains(first, tailHeader) || !strings.Contains(first, "Working memory:\n") {
		t.Fatalf("the user's message does not carry the state:\n%s", first)
	}
	for j, m := range reqs[2].Messages[2:] {
		if strings.Contains(m.Content, tailHeader) {
			t.Fatalf("message %d carries a second state mid-run; the conversation is its own record there:\n%s", j+2, m.Content)
		}
	}
	if strings.Contains(reqs[2].Messages[0].Content, "Working memory:\n") {
		t.Fatal("Working memory is still in the system prompt")
	}
}

// When the history is rewritten — here by collapsing old tool results — the
// server's cache is cold whatever happens next, and Working memory now holds
// what the conversation no longer does. That is when a fresh state is
// attached, to the newest message.
func TestARewrittenHistoryGetsAFreshState(t *testing.T) {
	ag, p := readTwice(t, nil)
	before := len(p.requests())
	if n := ag.History.CollapseToolResults(0); n == 0 {
		t.Fatal("nothing to collapse; the test would prove nothing")
	}
	p.fn = func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "done"}, nil
	}
	if _, err := ag.run(context.Background(), "continue", false); err != nil { // a repair round: no new-turn state of its own
		t.Fatal(err)
	}
	reqs := p.requests()
	sent := reqs[before]
	last := sent.Messages[len(sent.Messages)-1].Content
	if !strings.Contains(last, tailHeader) || !strings.Contains(last, "a.go") {
		t.Fatalf("no fresh state on the newest message after a rewrite:\n%s", last)
	}
}

// The state is for the model. What reads the history back as somebody's words
// reads it without.
func TestStripHarnessState(t *testing.T) {
	ag, _ := readTwice(t, nil)
	stored := ag.History.Messages[0].Content
	if !strings.Contains(stored, tailHeader) {
		t.Fatal("the stored user message should carry the state")
	}
	if got := StripHarnessState(stored); !strings.HasPrefix(got, "look at both files") || strings.Contains(got, "Working memory") {
		t.Fatalf("not stripped: %q", got)
	}
	if got := StripHarnessState("plain"); got != "plain" {
		t.Fatalf("a message without a state changed: %q", got)
	}
}

// A second request's state replaces nothing that was sent: the first
// request's message keeps its snapshot, the new message gets its own.
func TestANewRequestAttachesItsOwnStateAndLeavesTheOldOne(t *testing.T) {
	ag, p := readTwice(t, nil)
	firstStored := ag.History.Messages[0].Content
	p.fn = func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "done"}, nil
	}
	if _, err := ag.Run(context.Background(), "and now summarise"); err != nil {
		t.Fatal(err)
	}
	if ag.History.Messages[0].Content != firstStored {
		t.Fatal("the first request's message was rewritten")
	}
	reqs := p.requests()
	last := reqs[len(reqs)-1].Messages
	newest := last[len(last)-1].Content
	if !strings.HasPrefix(newest, "and now summarise") || !strings.Contains(newest, tailHeader) {
		t.Fatalf("the new request's message has no state:\n%s", newest)
	}
}

// prompt_layout: "classic" is the arrangement every version before this had.
func TestTheClassicLayoutKeepsWorkingMemoryInTheSystemPrompt(t *testing.T) {
	ag, p := readTwice(t, func(c *config.Config) { c.PromptLayout = "classic" })
	reqs := p.requests()
	last := reqs[2]
	if !strings.Contains(last.Messages[0].Content, "\n\nWorking memory:\n") {
		t.Fatal("classic: Working memory is not in the system prompt")
	}
	for _, m := range ag.History.Messages {
		if strings.Contains(m.Content, tailHeader) {
			t.Fatalf("classic: a message carries a state: %q", m.Content)
		}
	}
}

// The reasoning level is part of the prompt the server caches, so it does not
// flap: once it has stepped down above Target it stays down — through a dip
// back under the line — until the history is rewritten.
func TestTheReasoningLevelDoesNotFlapAroundTheTarget(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ReasoningEffort = "medium" })
	big := provider.Message{Role: provider.RoleTool, Content: strings.Repeat("x", 3*(ag.History.Target()+200))}
	if got := ag.steadyEffort("medium", true); got != "medium" {
		t.Fatalf("below target: %q", got)
	}
	ag.History.Add(big)
	if got := ag.steadyEffort("medium", true); got != "low" {
		t.Fatalf("above target: %q", got)
	}
	ag.History.Messages = ag.History.Messages[:0] // back under the line, nothing rewritten as far as the level knows
	if got := ag.steadyEffort("medium", true); got != "low" {
		t.Fatalf("the level flapped back up: %q", got)
	}
	ag.effortLowered = false // what requestFor does when it sees a rewritten history
	if got := ag.steadyEffort("medium", true); got != "medium" {
		t.Fatalf("after a rewrite: %q", got)
	}
}

// A prefill must warm the prompt the next request will really send, and the
// reasoning level is part of that prompt.
func TestThePrefillCarriesTheRealReasoningLevel(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0; c.ReasoningEffort = "medium" })
	pp := &prefillProvider{funcProvider: &funcProvider{}, release: make(chan struct{})}
	close(pp.release)
	ag.Provider = pp
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.ResolveModelNow(context.Background())
	waitFor(t, "the prefill", func() bool { n, _ := pp.seen(); return n == 1 })
	ag.stopPrefill()
	pp.mu.Lock()
	got := pp.prefills[0].ReasoningEffort
	pp.mu.Unlock()
	if got != "medium" {
		t.Fatalf("the prefill went out at reasoning level %q; the real request will use %q", got, "medium")
	}
}
