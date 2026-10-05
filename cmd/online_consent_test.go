package cmd

import (
	"context"
	"os"
	"strings"
	"testing"

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
