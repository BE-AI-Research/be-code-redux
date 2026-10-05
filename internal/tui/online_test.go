package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
)

func TestBottomLineShowsSpendWhenOnline(t *testing.T) {
	s, a, _ := twoViews(t)
	a.mu.Lock()
	local := a.bottomLine()
	a.mu.Unlock()
	if strings.Contains(local, "$") {
		t.Fatalf("local line shows spend: %q", local)
	}
	s.ag.SetOnline("openrouter", "K", agent.Pricing{Prompt: 1e-6, Completion: 1e-6, Known: true})
	a.mu.Lock()
	line := a.bottomLine()
	a.mu.Unlock()
	if !strings.Contains(line, " · $0.00") {
		t.Fatalf("online line: %q", line)
	}
}

func TestOnlineBadge(t *testing.T) {
	s, a, _ := twoViews(t)
	a.mu.Lock()
	local := a.bottomLine()
	a.mu.Unlock()
	if strings.Contains(local, "online:") {
		t.Fatalf("local line has the badge: %q", local)
	}
	s.ag.SetOnline("openrouter", "K", agent.Pricing{})
	a.mu.Lock()
	line := a.bottomLine()
	a.mu.Unlock()
	want := "online: openrouter · " + shortModel(s.ag.CurrentModel())
	if !strings.Contains(line, want) {
		t.Fatalf("online line %q lacks %q", line, want)
	}
	if rest := strings.Replace(line, want, "", 1); strings.Contains(rest, "· "+shortModel(s.ag.CurrentModel())+" ·") {
		t.Fatalf("the badge replaces the plain model segment: %q", line)
	}
}

// online_project has no "always": "a" leaves the modal open and changes no
// standing approval, and the question is not coloured as a diff.
func TestOnlineProjectAskHasNoAlways(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeCfg, beforeReg, beforeShell := s.cfg.ApproveFileWrites, s.ag.Tools.ApproveWrites, s.cfg.AutoApproveShell
	decided := make(chan bool, 1)
	go func() {
		decided <- s.approveFromAgent("online_project", "This project's files, command output and conversation will be sent to openrouter (m). Allow for this project?")
	}()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
	view := a.View()
	if !strings.Contains(view, "Send this project to an online model") || !strings.Contains(view, "y allow for this project · n decline") {
		t.Fatalf("title/hint:\n%s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk || s.cfg.ApproveFileWrites != beforeCfg || s.ag.Tools.ApproveWrites != beforeReg || s.cfg.AutoApproveShell != beforeShell {
		t.Fatal(`"a" must do nothing on an online_project prompt`)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	select {
	case ok := <-decided:
		if ok {
			t.Fatal("n declines")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("n never answered")
	}
}

// A typed request is not accepted while the startup question waits.
func TestSubmitRefusedWhileOnlineGatePending(t *testing.T) {
	s, a, _ := twoViews(t)
	s.ag.SetOnline("openrouter", "K", agent.Pricing{})
	release := make(chan struct{})
	s.ag.Tools.Approve = func(string, string) bool { <-release; return false }
	s.ag.Tools.ApproveCtx = nil
	done := make(chan bool, 1)
	s.ag.StartOnlineGateAsync(func(ok bool) { done <- ok })
	defer func() { close(release); <-done }()
	started := false
	s.startTurnHook = func(string) { started = true }
	s.mu.Lock()
	s.Submit("do it", a.id)
	s.mu.Unlock()
	if started {
		t.Fatal("a turn started before the gate passed")
	}
	found := false
	for _, e := range s.Entries() {
		found = found || strings.Contains(e.Text, "waiting for your answer about sending this project to openrouter")
	}
	if !found {
		t.Fatal("no explanation in the transcript")
	}
}

// share_page has no "always" either: "a" leaves the modal open and changes
// no standing approval, and the question is not coloured as a diff.
func TestSharePageAskHasNoAlways(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeCfg, beforeReg, beforeShell, beforeBrowser := s.cfg.ApproveFileWrites, s.ag.Tools.ApproveWrites, s.cfg.AutoApproveShell, s.cfg.AutoApproveBrowser
	decided := make(chan bool, 1)
	go func() {
		decided <- s.approveFromAgent("share_page", "Send what the agent reads on a.test to openrouter?")
	}()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
	view := a.View()
	if !strings.Contains(view, "Send page text to an online model") || !strings.Contains(view, "y send it · n withhold it") {
		t.Fatalf("title/hint:\n%s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk || s.cfg.ApproveFileWrites != beforeCfg || s.ag.Tools.ApproveWrites != beforeReg ||
		s.cfg.AutoApproveShell != beforeShell || s.cfg.AutoApproveBrowser != beforeBrowser {
		t.Fatal(`"a" must do nothing on a share_page prompt`)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	select {
	case ok := <-decided:
		if ok {
			t.Fatal("n declines")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("n never answered")
	}
}
