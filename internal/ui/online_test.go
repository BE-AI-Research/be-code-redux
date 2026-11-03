package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
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
	if got := OnlineCommand(r.Agent, r.Cfg, nil); !strings.Contains(got, "local") {
		t.Fatalf("local report %q", got)
	}
	r.Agent.SetOnline("openrouter", "K", agent.Pricing{})
	r.Agent.ApproveOnlineForRun()
	got := OnlineCommand(r.Agent, r.Cfg, nil)
	if !strings.Contains(got, "online: openrouter · m") || !strings.Contains(got, "not tracked") {
		t.Fatalf("report %q", got)
	}
	if got := OnlineCommand(r.Agent, r.Cfg, []string{"forget"}); !strings.Contains(got, "next session asks again") {
		t.Fatalf("forget %q", got)
	}
}

func TestREPLSharePageApprovalHasNoAlways(t *testing.T) {
	if got := approvePrompt("share_page"); got != "approve? [y/N] " {
		t.Fatalf("prompt %q", got)
	}
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	r.lines <- lineEvent{line: "a"}
	beforeShell, beforeWrites, beforeBrowser := r.Cfg.AutoApproveShell, r.Cfg.ApproveFileWrites, r.Cfg.AutoApproveBrowser
	var ok bool
	out := capture(t, func() {
		ok = r.approveCtx(context.Background(), "share_page", "Send what the agent reads on a.test to openrouter?")
	})
	if ok {
		t.Fatal(`"a" is not an answer to a share_page prompt`)
	}
	if r.Cfg.AutoApproveShell != beforeShell || r.Cfg.ApproveFileWrites != beforeWrites || r.Cfg.AutoApproveBrowser != beforeBrowser {
		t.Fatal(`"a" changed a standing approval`)
	}
	if !strings.Contains(out, "share with the online model:") {
		t.Fatalf("header:\n%s", out)
	}
}

func TestCredentialLineAndRevoke(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "BE_TEST_OR_KEY", Online: true}
	t.Setenv("BE_TEST_OR_KEY", "")
	if got := CredentialLine(cfg, "openrouter"); got != "credential: none (run /online add openrouter)" {
		t.Fatalf("none: %q", got)
	}
	s, _ := credentials.Open(filepath.Join(home, ".be-code"))
	s.Put("openrouter", credentials.Credential{Kind: credentials.KindAPIKey, Key: "sk-or-secret", Source: "oauth", Created: time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)})
	if got := CredentialLine(cfg, "openrouter"); got != "credential: stored (oauth, 2026-11-02)" || strings.Contains(got, "secret") {
		t.Fatalf("stored: %q", got)
	}
	t.Setenv("BE_TEST_OR_KEY", "env-secret")
	if got := CredentialLine(cfg, "openrouter"); got != "credential: from $BE_TEST_OR_KEY" {
		t.Fatalf("env: %q", got)
	}
	line := RevokeCredential(cfg, "openrouter")
	if !strings.Contains(line, "revoked locally; revoke it at https://openrouter.ai/docs too") {
		t.Fatalf("revoke: %q", line)
	}
	s2, _ := credentials.Open(filepath.Join(home, ".be-code"))
	if _, ok := s2.Key("openrouter"); ok {
		t.Fatal("still stored")
	}
	if got := RevokeCredential(cfg, "openrouter"); got != "no stored credential for openrouter" {
		t.Fatalf("second revoke: %q", got)
	}
}

func TestOnlineCommandAddAndRevokeRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	if got := OnlineCommand(r.Agent, r.Cfg, []string{"revoke"}); got != "usage: /online [forget | add [name] | revoke <name>]" {
		t.Fatalf("%q", got)
	}
	if got := OnlineCommand(r.Agent, r.Cfg, []string{"revoke", "nobody"}); got != "no stored credential for nobody" {
		t.Fatalf("%q", got)
	}
	r.Agent.SetOnline("openrouter", "K", agent.Pricing{})
	r.Cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "BE_TEST_OR_KEY", Online: true}
	if got := OnlineCommand(r.Agent, r.Cfg, nil); !strings.Contains(got, "credential: ") {
		t.Fatalf("report lacks the credential line: %q", got)
	}
}
