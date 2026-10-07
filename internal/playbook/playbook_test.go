package playbook

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func loadUE5(t *testing.T) []Entry {
	t.Helper()
	b, err := os.ReadFile("../packs/testdata/ue5/playbooks/errors.md")
	if err != nil {
		t.Fatal(err)
	}
	entries, warns, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("the shipped playbook should parse cleanly, got warnings: %v", warns)
	}
	return entries
}

func TestUE5PlaybookParses(t *testing.T) {
	entries := loadUE5(t)
	if len(entries) < 10 {
		t.Fatalf("only %d entries", len(entries))
	}
	for _, e := range entries {
		if e.Fix == "" || e.Next == "" || e.Cause == "" {
			t.Errorf("entry %q missing cause/fix/next", e.Title)
		}
	}
}

func TestUE5PlaybookRouting(t *testing.T) {
	entries := loadUE5(t)
	cases := []struct {
		kind, raw, want string
	}{
		{"tool", "Unable to build while Live Coding is active. Exit the editor and game", "Live Coding lock"},
		{"uht", "Weapon.h(18): Error: #include found after .generated.h file - the .generated.h file should always be the last #include in a header", ".generated.h is not the last include"},
		{"uht", "Pickup.h(9): Error: Expected a GENERATED_BODY() at the start of the struct", "Missing GENERATED_BODY"},
		{"uht", "Inv.h(20): Error: Unrecognized type 'FItemThing' - type must be a UCLASS, USTRUCT, UENUM, or global delegate.", "Unrecognized type in a reflected signature"},
		{"compile", "A.cpp(3): fatal error C1083: Cannot open include file: 'Pickup.generated.h': No such file or directory", "Generated header not found"},
		{"link", `Module.Shooter.cpp.obj : error LNK2019: unresolved external symbol "public: virtual void __cdecl AShooterCharacter::ServerFire_Implementation(void)"`, "Missing _Implementation for a native event or RPC"},
		{"link", `error LNK2001: unresolved external symbol "public: virtual void __cdecl AShooterCharacter::GetLifetimeReplicatedProps(class TArray<class FLifetimeProperty,class TSizedDefaultAllocator<32> > &)const "`, "GetLifetimeReplicatedProps missing"},
		{"link", `error LNK2019: unresolved external symbol UEnhancedInputComponent::StaticClass`, "Unresolved external from another module"},
		{"link", "ld.lld: error: undefined symbol: UGameplayAbility::UGameplayAbility()", "Unresolved external from another module"},
		{"compile", "A.cpp(42,10): error C2065: 'Healthh': undeclared identifier", "Undeclared identifier"},
		{"compile", "A.cpp:12:5: error: member access into incomplete type 'USphereComponent'", "Incomplete type"},
		{"compile", "A.cpp:57:3: error: no member named 'Fier' in 'AWeapon'", "Not a member"},
		{"compile", "A.cpp(8): error C2259: 'UMyAbility': cannot instantiate abstract class", "Cannot instantiate abstract class"},
		{"tool", "Could not find definition for module 'Shooterr'", "Module definition not found"},
		{"tool", "Unable to find plugin 'MissingPlugin'", "Plugin not found"},
		{"compile", "A.cpp:1:1: error: something nobody wrote an entry for", "Unknown failure"},
	}
	for _, c := range cases {
		got := Match(entries, c.kind, c.raw, c.raw)
		if got == nil {
			t.Errorf("no match for %q", c.raw)
			continue
		}
		if got.Title != c.want {
			t.Errorf("%q\n  routed to %q, want %q", c.raw, got.Title, c.want)
		}
	}
}

// A pack's playbook is a file a person edits. One typo used to discard every
// entry in it, which silently removed all routing; the rest must survive.
func TestBadRegexSkipsOnlyThatEntry(t *testing.T) {
	entries, warns, err := Parse("## A\nmatch: alpha\nfix: x\n\n## Broken\nmatch: (unclosed\nfix: y\n\n## B\nmatch: beta\nfix: z\n")
	if err != nil {
		t.Fatalf("a bad entry must not fail the document: %v", err)
	}
	if len(entries) != 2 || entries[0].Title != "A" || entries[1].Title != "B" {
		t.Fatalf("want A and B kept, got %+v", titles(entries))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "Broken") {
		t.Fatalf("want one warning naming the entry, got %v", warns)
	}
}

func TestMissingMatchSkipsOnlyThatEntry(t *testing.T) {
	entries, warns, err := Parse("## No match\nfix: x\n\n## Fine\nmatch: ok\nfix: y\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "Fine" {
		t.Fatalf("got %v", titles(entries))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "No match") {
		t.Fatalf("want a warning naming the entry, got %v", warns)
	}
}

func titles(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Title)
	}
	return out
}

// A fenced block inside a body is prose, not fields. This used to rewrite the
// entry's routing regex from text the author never meant as a field, and
// throw away the rest of the body, with no error at all.
func TestFencedBlockDoesNotHijackFields(t *testing.T) {
	doc := "## E\nmatch: realpattern\nkind: compile\ncause: c\nfix:\n1. Write this:\n```cpp\nmatch: FString\nnext: not a field either\n```\n2. Then rebuild.\nnext: n\n"
	entries, warns, err := Parse(doc)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v warns=%v err=%v", titles(entries), warns, err)
	}
	e := entries[0]
	if e.Match.String() != "realpattern" {
		t.Errorf("match regex hijacked by the fenced text: %q", e.Match.String())
	}
	if e.Next != "n" {
		t.Errorf("next = %q, want the real one", e.Next)
	}
	for _, want := range []string{"1. Write this:", "match: FString", "2. Then rebuild."} {
		if !strings.Contains(e.Fix, want) {
			t.Errorf("fix body lost %q:\n%s", want, e.Fix)
		}
	}
}

// Outside a fence, a second key is a mistake worth saying out loud rather
// than letting the later line quietly win.
func TestDuplicateFieldWarnsAndKeepsTheFirst(t *testing.T) {
	entries, warns, err := Parse("## E\nmatch: first\nmatch: second\nfix: x\n")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", titles(entries), err)
	}
	if entries[0].Match.String() != "first" {
		t.Errorf("match = %q, want the first", entries[0].Match.String())
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "match") {
		t.Fatalf("want a warning about the repeated key, got %v", warns)
	}
}

// Markdown authors indent under a heading.
func TestIndentedFieldsAreAccepted(t *testing.T) {
	entries, _, err := Parse("## E\n  match: foo\n  kind: link\n  fix: do it\n")
	if err != nil || len(entries) != 1 {
		t.Fatalf("err=%v entries=%v", err, titles(entries))
	}
	if entries[0].Match.String() != "foo" || entries[0].Kind != "link" || entries[0].Fix != "do it" {
		t.Fatalf("got %+v", entries[0])
	}
}

// Match is exported and so is Entry.Match: a caller that merges playbooks or
// appends its own fallback can hand us an entry with no regex.
func TestMatchSkipsEntryWithNoRegex(t *testing.T) {
	es := []Entry{{Title: "handmade"}, {Title: "real", Match: regexp.MustCompile("boom")}}
	got := Match(es, "compile", "boom", "boom")
	if got == nil || got.Title != "real" {
		t.Fatalf("got %+v", got)
	}
}

// An unknown diagnostic kind must not be routed to an entry that declares a
// specific one; only kind-less and "any" entries may take it.
func TestUnknownKindDoesNotRouteToTypedEntry(t *testing.T) {
	es := []Entry{
		{Title: "typed", Kind: "link", Match: regexp.MustCompile("boom")},
		{Title: "catchall", Kind: "any", Match: regexp.MustCompile("boom")},
	}
	if got := Match(es, "", "boom", "boom"); got == nil || got.Title != "catchall" {
		t.Fatalf("got %+v", got)
	}
}

// A long line used to make the scanner stop, and flush() then reported the
// truncation as a missing match line while quietly dropping every entry after it.
func TestLongLineDoesNotTruncateTheDocument(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	entries, _, err := Parse("## A\nmatch: a\ncause: " + long + "\nfix: f\n\n## B\nmatch: b\nfix: g\n")
	if err != nil {
		t.Fatalf("a long body line must still parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want both entries, got %v", titles(entries))
	}
}

// Fields with no heading above them are a malformed document, not an empty one.
func TestFieldsBeforeAnyHeadingWarn(t *testing.T) {
	entries, warns, err := Parse("match: orphan\nfix: x\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %v", titles(entries))
	}
	if len(warns) == 0 {
		t.Fatal("a document with fields but no heading must say so")
	}
}
