package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// A pack's verify command is an argument vector the harness runs, and it
// honours shell_deny (packs spec §10). The project's own detected checks
// deliberately do not — the harness chose those, not a pack that arrived
// with somebody's repository — so the deny rules apply to argv checks only.
func TestRunChecksDeniesAnArgvCheckByGlob(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}, nil)
	ag.Tools.ShellDeny = []string{"*curl*"}

	proj := verify.Project{Kind: "pack", Checks: []verify.Check{{
		Name: "fetch", Argv: []string{"/usr/bin/curl", "http://example.com"}, Timeout: time.Second,
	}}}
	if rep := ag.runChecks(context.Background(), proj); rep != nil {
		t.Fatalf("a denied check must stop verification, got %+v", rep.Results)
	}

	// A built-in shell check is untouched by the same rule.
	shell := verify.Project{Kind: "go", Checks: []verify.Check{{
		Name: "build", Command: "curl --version", Timeout: 10 * time.Second,
	}}}
	if rep := ag.runChecks(context.Background(), shell); rep == nil {
		t.Fatal("a detected shell check must not be denied by shell_deny")
	}
}

// The notice has to say which of the two reasons applied, or the user is
// told a web page was read when nothing of the sort happened.
func TestDeniedCheckNoticeNamesTheDenyList(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}, nil)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	ag.Tools.ShellDeny = []string{"*curl*"}

	proj := verify.Project{Kind: "pack", Checks: []verify.Check{{
		Name: "fetch", Argv: []string{"/usr/bin/curl", "http://x"}, Timeout: time.Second,
	}}}
	ag.runChecks(context.Background(), proj)

	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "deny") {
		t.Fatalf("notice does not explain the refusal: %q", joined)
	}
	if strings.Contains(joined, "web page") {
		t.Errorf("notice blames the browser for a deny-list refusal: %q", joined)
	}
}
