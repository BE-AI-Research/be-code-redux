package agent

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// The shipped ue5 playbook, against real UnrealBuildTool output, through the
// real parser. Three things written in different places have to agree for a
// repair turn to be useful — the parser's idea of a diagnostic, the pack's
// match patterns, and the kinds the two share — and nothing else checks that
// they still do.
func TestRealPlaybookRoutesRealOutput(t *testing.T) {
	ag := packAgent(t)
	p, warns, err := packs.Load("../packs/testdata/ue5")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	p.Source = packs.SourceGlobal
	ag.EnablePacks([]*packs.Pack{p})
	if len(p.Playbook()) < 10 {
		t.Fatalf("only %d playbook entries loaded", len(p.Playbook()))
	}

	parse, _ := verify.ParserFor("ubt")
	for _, tc := range []struct{ name, output, wantTitle string }{
		{"a Windows link failure",
			`C:\P\Intermediate\Build\Win64\Module.Shooter.cpp.obj : error LNK2019: unresolved external symbol "public: static class UClass * __cdecl UEnhancedInputComponent::StaticClass(void)"`,
			"Unresolved external from another module"},
		{"generated.h out of order",
			`/p/Source/Shooter/Weapon.h(18): Error: #include found after .generated.h file - the .generated.h file should always be the last #include in a header`,
			".generated.h is not the last include"},
		{"a missing generated header",
			`A.cpp(3): fatal error C1083: Cannot open include file: 'Pickup.generated.h': No such file or directory`,
			"Generated header not found"},
		{"Live Coding",
			`Unable to build while Live Coding is active. Exit the editor and game`,
			"Live Coding lock"},
	} {
		d := parse(tc.output, 6, "")
		if d.First == nil {
			t.Errorf("%s: the parser found nothing", tc.name)
			continue
		}
		rep := &verify.Report{Results: []verify.CheckResult{{
			Check: verify.Check{Name: "build", Argv: []string{"ue-build"}, Parser: "ubt"}, Output: tc.output, Diagnosis: &d,
		}}}
		ag.verifySigs = nil // each case is its own first failure
		note := ag.playbookNote(rep)
		if !strings.Contains(note, tc.wantTitle) {
			t.Errorf("%s routed wrong:\n%s", tc.name, note)
		}
		if !strings.Contains(note, "cause:") || !strings.Contains(note, "fix:") {
			t.Errorf("%s: note has no cause or fix:\n%s", tc.name, note)
		}
	}
}
