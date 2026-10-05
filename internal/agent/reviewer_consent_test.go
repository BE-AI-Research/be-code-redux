package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Final fix 6: a separately configured reviewer on an online provider asks
// the consent an online co-worker asks, once per session, before any review
// request; declined, the review is skipped with a notice.
func reviewerConsentAgent(t *testing.T, online bool, mut func(*config.Config)) (*Agent, *int, *[]string, *safeLog) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "a"}, {Content: "b"}, {Content: "c"}}}, func(c *config.Config) {
		c.ReviewOnDone = true
		c.Reviewer = config.ReviewerConfig{Provider: "cloud", Model: "big"}
		url := "http://127.0.0.1:9/v1"
		if online {
			url = "https://api.example.com/v1"
		}
		c.Providers["cloud"] = config.ProviderConfig{Type: "openai", BaseURL: url}
		if mut != nil {
			mut(c)
		}
	})
	built := 0
	old := ReviewerFactory
	ReviewerFactory = func(cfg *config.Config) (provider.Provider, string, error) {
		built++
		return &scriptedProvider{responses: []provider.ChatResponse{{Content: "APPROVED"}, {Content: "APPROVED"}}}, "big", nil
	}
	t.Cleanup(func() { ReviewerFactory = old })
	var mu sync.Mutex
	asks := &[]string{}
	ag.Tools.Approve = func(action, detail string) bool {
		mu.Lock()
		defer mu.Unlock()
		if action == "consult" {
			*asks = append(*asks, detail)
			return strings.Contains(detail, "(reviewer)") && !strings.Contains(detail, "NO")
		}
		return true
	}
	return ag, &built, asks, spendNotices(ag)
}

func TestOnlineReviewerAsksOncePerSession(t *testing.T) {
	ag, built, asks, _ := reviewerConsentAgent(t, true, nil)
	ag.RunFull(context.Background(), "one")
	ag.RunFull(context.Background(), "two")
	if len(*asks) != 1 || !strings.HasPrefix((*asks)[0], "coworker: (reviewer) (cloud/big)") {
		t.Fatalf("asks %q", *asks)
	}
	if *built != 2 {
		t.Fatalf("reviewer built %d times", *built)
	}
}

func TestOnlineReviewerDeclinedSkipsReview(t *testing.T) {
	ag, built, asks, l := reviewerConsentAgent(t, true, nil)
	ag.Tools.Approve = func(action, detail string) bool {
		if action == "consult" {
			*asks = append(*asks, detail)
			return false
		}
		return true
	}
	_, rep, err := ag.RunFull(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if *built != 0 || rep.Reviewed || len(*asks) != 1 {
		t.Fatalf("built %d reviewed %v asks %d", *built, rep.Reviewed, len(*asks))
	}
	found := false
	for _, s := range l.log {
		if strings.Contains(s, "review skipped: the reviewer cloud/big is online") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notices %q", l.log)
	}
}

func TestOnlineReviewerYesFlagAndFiredTurn(t *testing.T) {
	ag, built, asks, _ := reviewerConsentAgent(t, true, func(c *config.Config) { c.AutoApproveConsult = true })
	ag.RunFull(context.Background(), "one")
	if len(*asks) != 0 || *built != 1 {
		t.Fatalf("-y: asks %d built %d", len(*asks), *built)
	}

	ag2, built2, asks2, _ := reviewerConsentAgent(t, true, func(c *config.Config) { c.AutoApproveConsult = true })
	ag2.Tools.SetAllowance(nil, 0)
	defer ag2.Tools.ClearAllowance()
	if ag2.reviewerConsent() || len(*asks2) != 0 || *built2 != 0 {
		t.Fatal("a fired turn consented to an online reviewer")
	}

	ag3, _, _, _ := reviewerConsentAgent(t, true, nil)
	ag3.Tools.Approve = nil // headless without -y
	if ag3.reviewerConsent() {
		t.Fatal("no approver consented")
	}
}

func TestLocalReviewerNeverAsks(t *testing.T) {
	ag, built, asks, _ := reviewerConsentAgent(t, false, nil)
	ag.RunFull(context.Background(), "one")
	if len(*asks) != 0 || *built != 1 {
		t.Fatalf("asks %d built %d", len(*asks), *built)
	}
}
