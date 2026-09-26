package tools

import (
	"context"
	"strings"
	"testing"
)

// TestProcessStartAfterUntrustedWebAlwaysAsks (fix round 1, item 1): the
// process tool's "start" action must go through the same shell_after_web
// suspension as the shell tool — an allow-listed command must still ask,
// not run silently, once the request has read an untrusted page.
func TestProcessStartAfterUntrustedWebAlwaysAsks(t *testing.T) {
	var log askLog
	reg, err := NewRegistry(t.TempDir(), log.approver(false))
	if err != nil {
		t.Fatal(err)
	}
	reg.ShellAllow = []string{"echo*"}
	pt := reg.byName["process"]
	t.Cleanup(reg.Close)
	reg.MarkUntrustedWeb()
	res := pt.Run(context.Background(), map[string]any{"action": "start", "command": "echo hi"})
	if log.count() != 1 || log.actions[0] != "shell_after_web" {
		t.Fatalf("asks %v", log.actions)
	}
	if !res.IsError || !strings.Contains(res.Content, "shell is not auto-approved after reading an untrusted web page in this request") {
		t.Fatalf("result %q", res.Content)
	}
	reg.ClearUntrustedWeb()
	if res := pt.Run(context.Background(), map[string]any{"action": "start", "command": "echo hi"}); res.IsError || log.count() != 1 {
		t.Fatalf("after Clear the allow list must apply again: %q, %d asks", res.Content, log.count())
	}
}

// TestProcessStartAfterUntrustedWebRunsWhenApproved (fix round 1, item 1).
func TestProcessStartAfterUntrustedWebRunsWhenApproved(t *testing.T) {
	var log askLog
	reg, err := NewRegistry(t.TempDir(), log.approver(true))
	if err != nil {
		t.Fatal(err)
	}
	pt := reg.byName["process"]
	t.Cleanup(reg.Close)
	reg.MarkUntrustedWeb()
	res := pt.Run(context.Background(), map[string]any{"action": "start", "command": "echo approved"})
	if res.IsError || log.count() != 1 || log.actions[0] != "shell_after_web" {
		t.Fatalf("result %q, asks %v", res.Content, log.actions)
	}
}

// TestProcessStartAfterUntrustedWebStillHonoursDeny (fix round 1, item 1).
func TestProcessStartAfterUntrustedWebStillHonoursDeny(t *testing.T) {
	var log askLog
	reg, err := NewRegistry(t.TempDir(), log.approver(true))
	if err != nil {
		t.Fatal(err)
	}
	reg.ShellDeny = []string{"rm*"}
	pt := reg.byName["process"]
	t.Cleanup(reg.Close)
	reg.MarkUntrustedWeb()
	res := pt.Run(context.Background(), map[string]any{"action": "start", "command": "rm -rf x"})
	if !res.IsError || !strings.Contains(res.Content, "deny list") || log.count() != 0 {
		t.Fatalf("result %q, %d asks", res.Content, log.count())
	}
}
