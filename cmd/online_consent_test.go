package cmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestHeadlessApproverRefusesOnlineProject(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveBrowser, cfg.AutoApproveShell = true, true
	if headlessApprover(cfg)("online_project", "Allow for this project?") {
		t.Fatal("-y answered online_project")
	}
	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = oldTTY })
	out := captureStderr(t, func() {
		if headlessApprover(config.Default())("online_project", "x") {
			t.Error("non-interactive approved online_project")
		}
	})
	if !strings.Contains(out, "this action always asks a person") {
		t.Fatalf("message:\n%s", out)
	}
}

func TestHeadlessOnlineGate(t *testing.T) {
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)
	t.Setenv("BE_TEST_ONLINE_KEY", "k")
	ag := callBuildAgent(t, cfg)
	err := headlessOnlineGate(ag, false)
	if err == nil || err.Error() != "this project is not approved for or; run interactively once, or pass -y for this run" {
		t.Fatalf("without -y: %v", err)
	}
	if err := headlessOnlineGate(ag, true); err != nil || !ag.OnlineApproved() {
		t.Fatalf("-y: %v", err)
	}
	home, _ := config.Dir()
	if _, err := os.Stat(home + "/engine"); err == nil {
		entries, _ := os.ReadDir(home + "/engine")
		for _, e := range entries {
			if _, err := os.Stat(home + "/engine/" + e.Name() + "/online.json"); err == nil {
				t.Fatal("-y wrote online.json")
			}
		}
	}
}

// Ruling 1: the online state follows every switch of the main provider.
func TestOnlineStateFollowsProviderSwitch(t *testing.T) {
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)
	cfg.Providers["lan"] = config.ProviderConfig{Type: "openai", BaseURL: "http://127.0.0.1:9/v1"}
	t.Setenv("BE_TEST_ONLINE_KEY", "k")
	ag := callBuildAgent(t, cfg)
	if _, on := ag.Online(); !on {
		t.Fatal("starts online")
	}
	lan, err := provider.FromConfig(cfg, "lan")
	if err != nil {
		t.Fatal(err)
	}
	ag.SetProvider(lan)
	ag.SetModelNow(context.Background(), "qwen3")
	if _, on := ag.Online(); on || ag.KeyEnv() != "" {
		t.Fatalf("still online after /provider lan (key %q)", ag.KeyEnv())
	}
	or, _ := provider.FromConfig(cfg, "or")
	ag.SetProvider(or)
	ag.SetModelNow(context.Background(), "vendor/m")
	if name, on := ag.Online(); !on || name != "or" || ag.KeyEnv() != "BE_TEST_ONLINE_KEY" {
		t.Fatalf("back online: %q %v %q", name, on, ag.KeyEnv())
	}
}

// M6: init sends to the main model too, so it takes run's consent rule.
func TestInitOnlineGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BE_TEST_ONLINE_KEY", "k")
	prevDir, prevProvider, prevModel, prevYes, prevResume := flagDir, flagProvider, flagModel, flagYes, flagResume
	t.Cleanup(func() {
		flagDir, flagProvider, flagModel, flagYes, flagResume = prevDir, prevProvider, prevModel, prevYes, prevResume
	})
	flagDir, flagProvider, flagModel, flagYes, flagResume = t.TempDir(), "", "", false, ""
	initCmd.SetContext(context.Background())
	err := initCmd.RunE(initCmd, nil)
	if err == nil || err.Error() != "this project is not approved for or; run interactively once, or pass -y for this run" {
		t.Fatalf("init without -y: %v", err)
	}
	flagYes = true
	captureStderr(t, func() {
		if err := initCmd.RunE(initCmd, nil); err != nil && strings.Contains(err.Error(), "not approved") {
			t.Errorf("init -y refused: %v", err)
		}
	})
	home, _ := config.Dir()
	if entries, _ := os.ReadDir(home + "/engine"); len(entries) > 0 {
		for _, e := range entries {
			if _, err := os.Stat(home + "/engine/" + e.Name() + "/online.json"); err == nil {
				t.Fatal("init -y wrote online.json")
			}
		}
	}
}

// The share_page gate follows the main provider: set while it is online,
// cleared on a switch to a local one, set again on the way back.
func TestShareGateFollowsProviderSwitch(t *testing.T) {
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)
	cfg.Providers["lan"] = config.ProviderConfig{Type: "openai", BaseURL: "http://127.0.0.1:9/v1"}
	t.Setenv("BE_TEST_ONLINE_KEY", "k")
	ag := callBuildAgent(t, cfg)
	if got := ag.Tools.ShareProvider(); got != "or" {
		t.Fatalf("online at startup: gate %q", got)
	}
	lan, err := provider.FromConfig(cfg, "lan")
	if err != nil {
		t.Fatal(err)
	}
	ag.SetProvider(lan)
	ag.SetModelNow(context.Background(), "qwen3")
	if got := ag.Tools.ShareProvider(); got != "" {
		t.Fatalf("a local main model keeps the gate %q", got)
	}
	or, _ := provider.FromConfig(cfg, "or")
	ag.SetProvider(or)
	ag.SetModelNow(context.Background(), "vendor/m")
	if got := ag.Tools.ShareProvider(); got != "or" {
		t.Fatalf("back online: gate %q", got)
	}
}

func TestHeadlessApproverRefusesSharePage(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveBrowser, cfg.AutoApproveShell = true, true
	if headlessApprover(cfg)("share_page", "Send what the agent reads on a.test to or?") {
		t.Fatal("-y answered share_page")
	}
	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = oldTTY })
	out := captureStderr(t, func() {
		if headlessApprover(config.Default())("share_page", "x") {
			t.Error("non-interactive approved share_page")
		}
	})
	if !strings.Contains(out, "this action always asks a person") {
		t.Fatalf("message:\n%s", out)
	}
}

// Final fix 5: a switch to an online model nobody knows the window of
// applies the online default, never the previous model's window; a
// configured context_window wins over that default.
func TestOnlineSwitchWindowNeverInherited(t *testing.T) {
	cfg := config.Default()
	cfg.DefaultProvider = "odd"
	cfg.Providers = map[string]config.ProviderConfig{
		"odd": {Type: "openai", BaseURL: "https://llm.example.com/v1", APIKeyEnv: "BE_TEST_ODD_KEY", Online: true},
	}
	t.Setenv("BE_TEST_ODD_KEY", "")
	ag, _ := testAgentFor(t, cfg, nil, "vendor/x")
	ag.ApplyWindow(131072) // the previous model's
	resolveOnline(context.Background(), cfg, nil, ag, "odd", "vendor/x", false)
	waitUntil(t, func() bool { return ag.Window() == onlineSwitchWindow })

	pc := cfg.Providers["odd"]
	pc.ContextWindow = 65536
	cfg.Providers["odd"] = pc
	resolveOnline(context.Background(), cfg, nil, ag, "odd", "vendor/x", false)
	waitUntil(t, func() bool { return ag.Window() == 65536 })
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// A scripted --resume (or any piped/CI session) on an online provider the
// project never approved must not reach the interactive gate: nobody can
// answer it, so the session would park forever. unattendedOnlineGate
// applies the run command's headless consent instead.
func TestUnattendedOnlineGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)
	t.Setenv("BE_TEST_ONLINE_KEY", "k")

	oldTTY, oldYes := stdinIsTTY, flagYes
	t.Cleanup(func() { stdinIsTTY, flagYes = oldTTY, oldYes })

	// On a terminal nothing changes: the gate stays the UI's question.
	stdinIsTTY = func() bool { return true }
	flagYes = false
	ag := callBuildAgent(t, cfg)
	if err := unattendedOnlineGate(ag); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	if ag.OnlineApproved() {
		t.Fatal("terminal pre-approved the gate")
	}

	// Unattended without -y: the run command's error, not a hang.
	stdinIsTTY = func() bool { return false }
	flagYes = false
	ag = callBuildAgent(t, cfg)
	err := unattendedOnlineGate(ag)
	if err == nil || err.Error() != "this project is not approved for or; run interactively once, or pass -y for this run" {
		t.Fatalf("unattended without -y: %v", err)
	}

	// Unattended with -y: approved for this run, nothing remembered.
	ag = callBuildAgent(t, cfg)
	flagYes = true
	if err := unattendedOnlineGate(ag); err != nil {
		t.Fatalf("unattended -y: %v", err)
	}
	if !ag.OnlineApproved() {
		t.Fatal("unattended -y did not approve the run")
	}
	home, _ := config.Dir()
	if entries, _ := os.ReadDir(home + "/engine"); len(entries) > 0 {
		for _, e := range entries {
			if _, err := os.Stat(home + "/engine/" + e.Name() + "/online.json"); err == nil {
				t.Fatal("unattended -y wrote online.json")
			}
		}
	}
}

// Final fix 10: run --json carries spend_usd while online (null when the
// price is unknown) and leaves a local run's object as it was.
func TestRunJSONSpend(t *testing.T) {
	ag, _ := testAgentFor(t, config.Default(), nil, "m")
	out := map[string]any{}
	addSpendJSON(out, ag)
	if _, ok := out["spend_usd"]; ok {
		t.Fatalf("local run carries spend_usd: %v", out)
	}
	ag.SetOnline("openrouter", "K", agent.Pricing{Prompt: 1e-6, Completion: 2e-6, Known: true})
	addSpendJSON(out, ag)
	if v, ok := out["spend_usd"].(float64); !ok || v != 0 {
		t.Fatalf("online priced: %v", out)
	}
	ag.SetOnline("openrouter", "K", agent.Pricing{})
	out = map[string]any{}
	addSpendJSON(out, ag)
	if v, ok := out["spend_usd"]; !ok || v != nil {
		t.Fatalf("online unpriced: %v", out)
	}
}
