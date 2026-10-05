package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
)

func TestMainPromptSaysOnline(t *testing.T) {
	r := newTestREPL(t)
	if got := MainPrompt(r.Agent); got != "be-code> " {
		t.Fatalf("local prompt %q", got)
	}
	r.Agent.SetOnline("openrouter", "K", agent.Pricing{})
	if got := MainPrompt(r.Agent); got != "be-code (online)> " {
		t.Fatalf("online prompt %q", got)
	}
}

func TestREPLOnlineProjectApprovalHasNoAlways(t *testing.T) {
	if got := approvePrompt("online_project"); got != "approve? [y/N] " {
		t.Fatalf("prompt %q", got)
	}
	if got := approvePrompt("shell"); got != "approve? [y/N/a(lways)] " {
		t.Fatalf("shell prompt %q", got)
	}
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	r.lines <- lineEvent{line: "a"}
	beforeShell, beforeWrites := r.Cfg.AutoApproveShell, r.Cfg.ApproveFileWrites
	var ok bool
	out := capture(t, func() {
		ok = r.approveCtx(context.Background(), "online_project", "This project's files ... Allow for this project?")
	})
	if ok {
		t.Fatal(`"a" is not an answer to an online_project prompt`)
	}
	if r.Cfg.AutoApproveShell != beforeShell || r.Cfg.ApproveFileWrites != beforeWrites {
		t.Fatal(`"a" changed a standing approval`)
	}
	if !strings.Contains(out, "online model:") {
		t.Fatalf("header:\n%s", out)
	}
}

func TestOnlineCommandListedBusySafeAndReports(t *testing.T) {
	found := false
	for _, c := range SlashCommandTable {
		if c.Name == "/online" {
			found = c.Args
		}
	}
	if !found || !BusySafeCommand("/online forget") {
		t.Fatal("/online is listed (with an argument) and busy-safe")
	}
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	if got := OnlineCommand(r.Agent, nil); !strings.Contains(got, "local") {
		t.Fatalf("local report %q", got)
	}
	r.Agent.SetOnline("openrouter", "K", agent.Pricing{})
	r.Agent.ApproveOnlineForRun()
	got := OnlineCommand(r.Agent, nil)
	if !strings.Contains(got, "online: openrouter · m") || !strings.Contains(got, "not tracked") {
		t.Fatalf("report %q", got)
	}
	if got := OnlineCommand(r.Agent, []string{"forget"}); !strings.Contains(got, "next session asks again") {
		t.Fatalf("forget %q", got)
	}
}
