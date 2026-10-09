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
//
// A denied check is *skipped*, not a refusal of the whole verification. The
// first version of this treated it like a declined shell_after_web prompt and
// returned no report at all, which a whole-branch review caught: a deny glob
// is a standing rule, so with one denied pack check the project's own passing
// checks were discarded, no repair round ever ran, and it recurred on every
// single request. A one-off "no" to a prompt and a permanent rule are not the
// same answer and no longer share a code path.
func TestDeniedArgvCheckIsSkippedAndTheRestStillRuns(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}, nil)
	ag.Tools.ShellDeny = []string{"*curl*"}

	proj := verify.Project{Kind: "pack", Checks: []verify.Check{
		{Name: "ok", Command: "true", Timeout: 10 * time.Second},
		{Name: "fetch", Argv: []string{"/usr/bin/curl", "http://example.com"}, Timeout: time.Second},
	}}
	rep := ag.runChecks(context.Background(), proj)
	if rep == nil {
		t.Fatal("a denied check must not throw the whole report away")
	}
	var sawOK, sawSkip bool
	for _, r := range rep.Results {
		switch r.Check.Name {
		case "ok":
			sawOK = r.Passed
		case "fetch":
			sawSkip = r.Skipped
			if !r.Passed {
				t.Error("a skipped check must not read as a failure, or it starts a repair round")
			}
			if !strings.Contains(r.Note, "deny") {
				t.Errorf("skipped check does not say why: %q", r.Note)
			}
		}
	}
	if !sawOK {
		t.Error("the project's own passing check was lost")
	}
	if !sawSkip {
		t.Error("the denied check is not reported as skipped")
	}
}

// A built-in shell check is untouched by the same rule.
func TestDetectedShellCheckIsNotDeniedByShellDeny(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}, nil)
	ag.Tools.ShellDeny = []string{"*curl*"}
	shell := verify.Project{Kind: "go", Checks: []verify.Check{{
		Name: "build", Command: "curl --version", Timeout: 10 * time.Second,
	}}}
	if rep := ag.runChecks(context.Background(), shell); rep == nil {
		t.Fatal("a detected shell check must not be denied by shell_deny")
	}
}

// The notice has to say which of the two reasons applied, or the user is
// told a web page was read when nothing of the sort happened — and it must
// not repeat on every request, because a standing rule does not change.
func TestDeniedCheckNoticeNamesTheDenyListAndSaysItOnce(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}, nil)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	ag.Tools.ShellDeny = []string{"*curl*"}

	proj := verify.Project{Kind: "pack", Checks: []verify.Check{{
		Name: "fetch", Argv: []string{"/usr/bin/curl", "http://x"}, Timeout: time.Second,
	}}}
	ag.runChecks(context.Background(), proj)
	ag.runChecks(context.Background(), proj)

	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "deny") {
		t.Fatalf("notice does not explain the refusal: %q", joined)
	}
	if strings.Contains(joined, "web page") {
		t.Errorf("notice blames the browser for a deny-list refusal: %q", joined)
	}
	if n := strings.Count(joined, "deny"); n != 1 {
		t.Errorf("want the deny notice once per session, got %d: %q", n, joined)
	}
}
