package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

func outageAgent(t *testing.T, failErr error, yes bool) (*Agent, *recProvider, *recProvider, *safeLog, func() string) {
	t.Helper()
	online := &recProvider{name: "openrouter", fail: failErr}
	helper := &recProvider{name: "ollama", def: provider.ChatResponse{Content: "local answer"}}
	withHelper(t, helper, 0, nil)
	ag, _ := newTestAgent(t, online, helperCfg)
	ag.retryBase = time.Millisecond
	recordSleeps(ag)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.ApproveOnlineForRun()
	var l safeLog
	ag.Tools.Approve = l.approver(yes)
	return ag, online, helper, &l, noteSink(ag)
}

func asked(l *safeLog) []string {
	var out []string
	for _, e := range l.get() {
		if strings.HasPrefix(e, "switch_to_local|") {
			out = append(out, e)
		}
	}
	return out
}

func TestOutageOffersSwitchAndRetriesOnHelper(t *testing.T) {
	ag, online, helper, l, notes := outageAgent(t, &provider.HTTPError{Provider: "openrouter", Code: 503, Body: "down"}, true)
	answer, err := ag.Run(context.Background(), "do it")
	if err != nil || answer != "local answer" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	q := asked(l)
	if len(q) != 1 || !strings.Contains(q[0], "openrouter is not responding. Switch this session to your local model (helper-model)?") {
		t.Fatalf("asked %q", q)
	}
	if len(online.requests()) == 0 || len(helper.requests()) != 1 {
		t.Fatalf("online=%d helper=%d", len(online.requests()), len(helper.requests()))
	}
	if _, on := ag.Online(); on {
		t.Fatal("session should be local now")
	}
	if !strings.Contains(notes(), "/provider openrouter") {
		t.Fatalf("notes %q", notes())
	}
}

func TestOutageDeclinedReturnsError(t *testing.T) {
	ag, _, helper, l, _ := outageAgent(t, &provider.HTTPError{Provider: "openrouter", Code: 503, Body: "down"}, false)
	_, err := ag.Run(context.Background(), "do it")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err %v", err)
	}
	if len(asked(l)) != 1 || len(helper.requests()) != 0 {
		t.Fatalf("asked=%d helper=%d", len(asked(l)), len(helper.requests()))
	}
	// Once per outage: a second failing request does not ask again.
	if _, err := ag.Run(context.Background(), "again"); err == nil {
		t.Fatal("want error")
	}
	if len(asked(l)) != 1 {
		t.Fatalf("asked again: %d", len(asked(l)))
	}
	// A success ends the outage; the next one asks afresh.
	ag.noteOutageOver()
	ag.Run(context.Background(), "third")
	if len(asked(l)) != 2 {
		t.Fatalf("asked %d after recovery", len(asked(l)))
	}
}

func TestOutageNotOfferedForAuthError(t *testing.T) {
	for _, code := range []int{401, 403, 400, 404} {
		ag, _, helper, l, _ := outageAgent(t, &provider.HTTPError{Provider: "openrouter", Code: code, Body: "no"}, true)
		if _, err := ag.Run(context.Background(), "do it"); err == nil {
			t.Fatalf("%d: want error", code)
		}
		if len(asked(l)) != 0 || len(helper.requests()) != 0 {
			t.Fatalf("%d: offered", code)
		}
	}
}

func TestOutageNotOfferedWithoutReachableHelper(t *testing.T) {
	online := &recProvider{name: "openrouter", fail: &provider.HTTPError{Provider: "openrouter", Code: 503}}
	withHelper(t, nil, 0, context.DeadlineExceeded)
	ag, _ := newTestAgent(t, online, helperCfg)
	ag.retryBase = time.Millisecond
	recordSleeps(ag)
	ag.SetOnline("openrouter", "K", Pricing{Known: true})
	ag.ApproveOnlineForRun()
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if _, err := ag.Run(context.Background(), "x"); err == nil || len(asked(&l)) != 0 {
		t.Fatalf("err=%v asked=%v", err, asked(&l))
	}
}

func TestOutageInFiredTurnStops(t *testing.T) {
	ag, _, helper, l, notes := outageAgent(t, &provider.HTTPError{Provider: "openrouter", Code: 503, Body: "down"}, true)
	ag.Tools.SetAllowance(schedule.Allowance{}, time.Second)
	defer ag.Tools.ClearAllowance()
	if _, err := ag.Run(context.Background(), "do it"); err == nil {
		t.Fatal("want error")
	}
	if len(asked(l)) != 0 || len(helper.requests()) != 0 {
		t.Fatal("a fired turn must not ask or switch")
	}
	if !strings.Contains(notes(), "openrouter unreachable; the scheduled event stopped") {
		t.Fatalf("notes %q", notes())
	}
}
