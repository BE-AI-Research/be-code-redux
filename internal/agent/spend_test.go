package agent

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func usageResp(p, c int) provider.ChatResponse {
	return provider.ChatResponse{Content: "ok", Usage: provider.Usage{PromptTokens: p, CompletionTokens: c}}
}

func spendNotices(ag *Agent) *safeLog {
	l := &safeLog{}
	ag.Events.OnNotice = func(s string) { l.mu.Lock(); l.log = append(l.log, s); l.mu.Unlock() }
	return l
}

func TestSpendAccumulatesFromUsage(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{usageResp(1000, 500)}}, nil)
	ag.SetOnline("openrouter", "K", Pricing{Prompt: 3e-6, Completion: 15e-6, Known: true})
	if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := ag.Usage().SpendUSD; math.Abs(got-0.0105) > 1e-9 {
		t.Fatalf("spend %v", got)
	}
	if !strings.Contains(ag.StatsReport(nil), "$0.01 (est.)") {
		t.Fatalf("report:\n%s", ag.StatsReport(nil))
	}
}

func TestUnpricedModelNoticeOnce(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{usageResp(10, 5), usageResp(10, 5)}}, nil)
	l := spendNotices(ag)
	ag.SetOnline("openrouter", "K", Pricing{})
	for i := 0; i < 2; i++ {
		if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, s := range l.log {
		if strings.Contains(s, "spend is not tracked for this model") {
			n++
		}
	}
	if n != 1 || ag.Usage().SpendUSD != 0 {
		t.Fatalf("notices %d spend %v", n, ag.Usage().SpendUSD)
	}
	if !strings.Contains(ag.StatsReport(nil), "not tracked") {
		t.Fatal("stats should say not tracked")
	}
}

func TestLocalSessionNoSpend(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{usageResp(1000, 500)}}, func(c *config.Config) { c.MaxSpendUSD = 0.0001 })
	l := spendNotices(ag)
	if _, _, err := ag.RunFull(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if ag.Usage().SpendUSD != 0 {
		t.Fatal("local session spent")
	}
	for _, s := range l.log {
		if strings.Contains(s, "spend") {
			t.Fatalf("notice %q", s)
		}
	}
	if strings.Contains(ag.StatsReport(nil), "spend") {
		t.Fatal("stats mention spend")
	}
}

func TestSpendCapAsksAndRaises(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.MaxSpendUSD = 0.01 })
	ag.SetOnline("openrouter", "K", Pricing{Prompt: 1e-5, Completion: 1e-5, Known: true})
	ag.addStats(Stats{SpendUSD: 0.012})
	var mu sync.Mutex
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, action+"|"+detail)
		return true
	}
	if err := ag.checkSpendCap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ag.checkSpendCap(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "spend_cap|This session has spent $0.01 of your $0.01 cap. Continue (raises the cap by $0.01)?"
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("asked %q", asked)
	}
	if got := ag.SpendCap(); math.Abs(got-0.02) > 1e-12 {
		t.Fatalf("cap %v", got)
	}
	if ag.Cfg.MaxSpendUSD != 0.01 {
		t.Fatal("config changed")
	}
}

func TestSpendCapRefusesWithNobody(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.MaxSpendUSD = 1 })
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.addStats(Stats{SpendUSD: 1.5})
	ag.Tools.Approve = nil
	err := ag.checkSpendCap(context.Background())
	if err == nil || err.Error() != "spend cap reached ($1.50 of $1.00); raise max_spend_usd or continue when asked" {
		t.Fatalf("err %v", err)
	}
	// And through a run: the request is never sent.
	p := &scriptedProvider{}
	ag2, _ := newTestAgent(t, p, func(c *config.Config) { c.MaxSpendUSD = 1 })
	ag2.SetOnline("openrouter", "K", Pricing{Known: true})
	ag2.addStats(Stats{SpendUSD: 1.5})
	ag2.Tools.Approve = func(string, string) bool { return false }
	if _, _, err := ag2.RunFull(context.Background(), "hi"); err == nil || p.i != 0 {
		t.Fatalf("err %v sent %d", err, p.i)
	}
}

func TestSpendCapInFiredTurnRefusesOnTimeout(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.MaxSpendUSD = 1 })
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.addStats(Stats{SpendUSD: 2})
	ag.Tools.SetAllowance(nil, 30*time.Millisecond)
	marked := false
	ag.Tools.ApproveCtx = func(ctx context.Context, action, _ string) bool {
		marked = action == "spend_cap" && toolsFiredAsk(ctx)
		<-ctx.Done()
		return false
	}
	err := ag.checkSpendCap(context.Background())
	if err == nil || !strings.Contains(err.Error(), "spend cap reached") || !marked {
		t.Fatalf("err %v marked %v", err, marked)
	}
	if refused, timedOut := ag.Tools.ClearAllowance(); refused != "spend_cap" || !timedOut {
		t.Fatalf("refused %q timedOut %v", refused, timedOut)
	}
}

func toolsFiredAsk(ctx context.Context) bool { return tools.FiredAsk(ctx) }
