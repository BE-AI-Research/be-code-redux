package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/verify"
)

const testPlaybook = `# test playbook

## Unresolved external from another module
match: LNK2019|undefined reference to
kind: link
cause: The symbol lives in a module this module does not depend on.
fix:
1. Find which module defines the symbol.
2. Add it to PublicDependencyModuleNames in this module's .Build.cs.
next: If the dependency is already there, the class may be missing its MODULE_API macro.

## Unknown failure
match: .
kind: any
cause: No specific entry matched.
fix:
1. Read the first error, fix only that, rebuild.
next: Fetch the most relevant chunk before trying again.
`

func playbookAgent(t *testing.T) *Agent {
	t.Helper()
	ag := packAgent(t)
	dir := filepath.Join(t.TempDir(), "ue5")
	if err := os.MkdirAll(filepath.Join(dir, "playbooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, s string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"schema":1,"name":"ue5","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500},"playbooks":["playbooks/errors.md"]}`)
	write("core.md", "rules")
	write("playbooks/errors.md", testPlaybook)
	p, warns, err := packs.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatal(warns)
	}
	p.Source = packs.SourceGlobal
	ag.EnablePacks([]*packs.Pack{p})
	return ag
}

func linkFailure() *verify.Report {
	d := verify.Diagnostic{
		Kind: "link", Severity: "error", Code: "LNK2019",
		Message: `unresolved external symbol "UEnhancedInputComponent::StaticClass"`,
		Raw:     `Module.obj : error LNK2019: unresolved external symbol "UEnhancedInputComponent::StaticClass"`,
	}
	return &verify.Report{Results: []verify.CheckResult{{
		Check:     verify.Check{Name: "build", Argv: []string{"ue-build"}, Parser: "ubt"},
		Output:    d.Raw,
		Diagnosis: &verify.Diagnosis{First: &d, Count: 1, Signature: verify.Signature(d)},
	}}}
}

// The repair turn carries the pack's cause and fix, so a weak model is told
// what this failure means rather than being handed a compiler line and left
// to guess.
func TestRepairNoteCarriesTheMatchingEntry(t *testing.T) {
	ag := playbookAgent(t)
	note := ag.playbookNote(linkFailure())
	for _, want := range []string{
		"playbook ue5",
		"Unresolved external from another module",
		"does not depend on",
		"PublicDependencyModuleNames",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note is missing %q:\n%s", want, note)
		}
	}
	// First time round it is the fix, not the fallback advice.
	if strings.Contains(note, "already there") {
		t.Errorf("the first failure should show fix, not next:\n%s", note)
	}
}

// The same failure coming back means the fix did not work, so repeating it
// is the one thing not to do: the entry's next step replaces it.
func TestSameSignatureSwitchesToNextThenSaysItWasTried(t *testing.T) {
	ag := playbookAgent(t)
	rep := linkFailure()

	first := ag.playbookNote(rep)
	if !strings.Contains(first, "PublicDependencyModuleNames") {
		t.Fatalf("first note:\n%s", first)
	}
	second := ag.playbookNote(rep)
	if !strings.Contains(second, "already there") {
		t.Errorf("the repeat should show next:\n%s", second)
	}
	if strings.Contains(second, "1. Find which module") {
		t.Errorf("the repeat should not repeat the fix:\n%s", second)
	}
	third := ag.playbookNote(rep)
	if !strings.Contains(third, "have both been tried") {
		t.Errorf("the third time must say the entry was tried:\n%s", third)
	}
	// And it must not hand back either piece of advice again.
	if strings.Contains(third, "1. Find which module") || strings.Contains(third, "already there") {
		t.Errorf("the third time must not repeat the advice:\n%s", third)
	}

	// A different failure starts again at its own fix.
	other := linkFailure()
	d := *other.Results[0].Diagnosis.First
	d.Code, d.Message = "C2065", "'Healthh': undeclared identifier"
	d.Raw = "A.cpp(42,10): error C2065: 'Healthh': undeclared identifier"
	d.Kind = "compile"
	other.Results[0].Diagnosis = &verify.Diagnosis{First: &d, Count: 1, Signature: verify.Signature(d)}
	if n := ag.playbookNote(other); !strings.Contains(n, "Unknown failure") {
		t.Errorf("a new failure should match afresh:\n%s", n)
	}
}

// No pack, no playbook, no entry: the repair turn is what it always was.
func TestNoPlaybookNoNote(t *testing.T) {
	ag := packAgent(t)
	if note := ag.playbookNote(linkFailure()); note != "" {
		t.Errorf("note without a playbook: %q", note)
	}
	// And a report with nothing parsed cannot match anything.
	ag2 := playbookAgent(t)
	plain := &verify.Report{Results: []verify.CheckResult{{
		Check: verify.Check{Name: "test", Command: "go test ./..."}, Output: "FAIL",
	}}}
	if note := ag2.playbookNote(plain); note != "" {
		t.Errorf("note for an unparsed failure: %q", note)
	}
}
