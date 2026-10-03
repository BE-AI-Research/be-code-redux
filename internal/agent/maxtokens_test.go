package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// ownerConfig is the configuration the bug was found on: max_tokens equal to
// context_tokens equal to the server's window, on a thinking model.
func ownerConfig(c *config.Config) {
	c.MaxTokens = 32768
	c.ContextTokens = 32768
}

// noticeLog collects notices from any goroutine.
type noticeLog struct {
	mu   sync.Mutex
	msgs []string
}

func (n *noticeLog) add(s string) { n.mu.Lock(); n.msgs = append(n.msgs, s); n.mu.Unlock() }
func (n *noticeLog) count(sub string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, m := range n.msgs {
		if strings.Contains(m, sub) {
			c++
		}
	}
	return c
}

// max_tokens is the reply's limit, not the room to keep free: a value as
// large as the window must not reserve the whole window.
func TestReserveCappedAtHalfTheBudget(t *testing.T) {
	cases := []struct {
		name               string
		maxTokens, ctxToks int
		window, want       int
	}{
		{"owner: max_tokens = window", 32768, 32768, 32768, 16384},
		{"modest max_tokens unchanged", 4096, 32768, 32768, 4096},
		{"context_tokens below the window caps by the budget", 32768, 16384, 32768, 8192},
		{"exactly half is not lowered", 16384, 0, 32768, 16384},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) {
				c.MaxTokens, c.ContextTokens = tc.maxTokens, tc.ctxToks
			})
			ag.SetModel("qwen3:8b")
			ag.ApplyWindow(tc.window)
			if got := ag.History.Reserve; got != tc.want {
				t.Fatalf("reserve = %d, want %d", got, tc.want)
			}
		})
	}
}

// max_tokens unset keeps the profile's own reserve exactly as before.
func TestReserveProfileDefaultsUnchanged(t *testing.T) {
	thinker, _ := newTestAgent(t, &scriptedProvider{}, nil)
	thinker.SetModel("qwen3:8b")
	thinker.ApplyWindow(32768)
	if got := thinker.History.Reserve; got != 32768/3 {
		t.Fatalf("thinking reserve = %d, want %d", got, 32768/3)
	}
	plain, _ := newTestAgent(t, &scriptedProvider{}, nil)
	plain.SetModel("llama3:8b")
	plain.ApplyWindow(32768)
	if got := plain.History.Reserve; got != 4096 {
		t.Fatalf("plain reserve = %d, want 4096", got)
	}
}

// The owner's exact session: Limit was 0 (floored to 512), Target 512, the
// wheel pinned at its cap and compaction fired before every call.
func TestOwnerConfigBudgetIsSane(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, ownerConfig)
	ag.SetModel("qwen3:8b")
	ag.ApplyWindow(32768)
	if l := ag.History.Limit(); l != 16384 {
		t.Fatalf("Limit = %d, want 16384", l)
	}
	if tg := ag.History.Target(); tg < 4096 {
		t.Fatalf("Target = %d, want well above the 512 floor", tg)
	}
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "hello there"})
	if pct := ag.History.Tokens() * 100 / ag.History.Limit(); pct > 50 {
		t.Fatalf("a small conversation reads %d%% of the budget", pct)
	}
	if ag.History.Over() {
		t.Fatal("a small conversation is over the limit")
	}
}

// The cap says so, once per session, and says what to change.
func TestReserveCapNoticeOnce(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, ownerConfig)
	var log noticeLog
	ag.Events.OnNotice = log.add
	ag.FlushQueuedNotices() // what New raised before any UI existed
	ag.SetModel("qwen3:8b")
	ag.ApplyWindow(32768)
	ag.ApplyWindow(32768)
	ag.SetModel("llama3:8b")
	want := "max_tokens 32768 leaves no room for the conversation in a 32768-token window; reserving 16384 instead. Change max_tokens in config (0 lets the server decide) to avoid this."
	if n := log.count(want); n != 1 {
		t.Fatalf("notice delivered %d times, want 1: %q", n, log.msgs)
	}
}

// A modest max_tokens raises nothing.
func TestReserveCapNoticeSilentWhenNotCapped(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.MaxTokens = 4096; c.ContextTokens = 32768 })
	var log noticeLog
	ag.Events.OnNotice = log.add
	ag.FlushQueuedNotices()
	ag.ApplyWindow(32768)
	if n := log.count("max_tokens"); n != 0 {
		t.Fatalf("unexpected notice: %q", log.msgs)
	}
}

// A budget the reserve leaves almost nothing of is named, once — reachable
// through the profile reserve, which max_tokens' cap does not touch: a
// thinking model's floor of 4096 in a 4096-token window.
func TestLowLimitNoticeNamesTheCause(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	var log noticeLog
	ag.Events.OnNotice = log.add
	ag.FlushQueuedNotices()
	ag.SetModel("qwen3:8b")
	ag.ApplyWindow(4096)
	ag.ApplyWindow(4096)
	if n := log.count("context budget 4096 minus reserve 4096 leaves 0 tokens"); n != 1 {
		t.Fatalf("low-limit notice delivered %d times, want 1: %q", n, log.msgs)
	}
	if l := ag.History.Limit(); l != 512 {
		t.Fatalf("the 512 floor must stay: %d", l)
	}
}

// The compaction summary is a few hundred words: it must not inherit a
// max_tokens sized for the whole window, while the ordinary turn still sends
// what the user configured.
func TestCompactionSummaryReplyIsBounded(t *testing.T) {
	var mu sync.Mutex
	var reqs []provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		if strings.HasPrefix(req.Messages[0].Content, "Summarize") {
			return &provider.ChatResponse{Content: "summary"}, nil
		}
		return &provider.ChatResponse{Content: "final answer"}, nil
	}}
	ag, _ := newTestAgent(t, p, ownerConfig)
	if _, err := ag.Run(context.Background(), "do the thing"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "work"})
	}
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var turn, summary *provider.ChatRequest
	for i := range reqs {
		if strings.HasPrefix(reqs[i].Messages[0].Content, "Summarize") {
			summary = &reqs[i]
		} else if turn == nil {
			turn = &reqs[i]
		}
	}
	if turn == nil || summary == nil {
		t.Fatalf("missing requests: %d sent", len(reqs))
	}
	if turn.MaxTokens != 32768 {
		t.Fatalf("ordinary turn sent max_tokens %d, want the configured 32768", turn.MaxTokens)
	}
	if summary.MaxTokens <= 0 || summary.MaxTokens > summaryReplyTokens {
		t.Fatalf("summary request max_tokens = %d, want 1..%d", summary.MaxTokens, summaryReplyTokens)
	}
}

// A max_tokens smaller than the summary cap is the user's limit and wins.
func TestHarnessReplyTokensRespectsSmallerMaxTokens(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.MaxTokens = 512 })
	if got := ag.harnessReplyTokens(summaryReplyTokens, nil); got != 512 {
		t.Fatalf("got %d, want 512", got)
	}
	// And it fits the window minus the prompt that goes with it.
	ag2, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 4096 })
	big := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", 3*3500)}}
	if got := ag2.harnessReplyTokens(summaryReplyTokens, big); got > 4096-3500 {
		t.Fatalf("reply cap %d does not fit beside a 3500-token prompt in 4096", got)
	}
}

// The handoff briefing and BECODE.md are notes, not the user's turn.
func TestHandoffAndInitRepliesAreBounded(t *testing.T) {
	var got provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return &provider.ChatResponse{Content: "HANDOFF"}, nil
	}}
	ag, _ := newTestAgent(t, p, ownerConfig)
	ag.Session = store.NewSession("func", "test-model", ag.Tools.Root)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "build the widget"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	if _, err := ag.WriteHandoff(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens <= 0 || got.MaxTokens > notesReplyTokens {
		t.Fatalf("handoff max_tokens = %d", got.MaxTokens)
	}
	got = provider.ChatRequest{}
	_, _, _ = ag.InitProject(context.Background(), factsFixture())
	if got.MaxTokens <= 0 || got.MaxTokens > notesReplyTokens {
		t.Fatalf("init max_tokens = %d", got.MaxTokens)
	}
}
