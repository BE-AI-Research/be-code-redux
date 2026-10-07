package verify

import (
	"runtime"
	"testing"
)

func TestMSVCCompileError(t *testing.T) {
	out := `Building 4 actions with 16 processes...
[1/4] Compile [x64] MyActor.cpp
C:\Projects\Shooter\Source\Shooter\MyActor.cpp(42,10): error C2065: 'Healthh': undeclared identifier
C:\Projects\Shooter\Source\Shooter\MyActor.cpp(43,5): error C2228: left of '.Num' must have class/struct/union
Result: Failed (OtherCompilationError)`
	r := parseUBT(out, 6, `C:\Projects\Shooter`)
	if r.OK || r.First == nil {
		t.Fatal("expected failure")
	}
	d := r.First
	if d.Kind != "compile" || d.Code != "C2065" || d.Line != 42 || d.Column != 10 {
		t.Fatalf("wrong first diagnostic: %+v", d)
	}
	if d.File != "Source/Shooter/MyActor.cpp" {
		t.Fatalf("path not made relative: %q", d.File)
	}
	if r.Count != 2 {
		t.Fatalf("count = %d, want 2", r.Count)
	}
}

func TestUHTWinsOverEverything(t *testing.T) {
	out := `Running UnrealHeaderTool "/home/dev/Shooter/Shooter.uproject"
/home/dev/Shooter/Source/Shooter/Weapon.h(18): Error: #include found after .generated.h file - the .generated.h file should always be the last #include in a header
ERROR: UBT ERROR: Failed to produce item: /home/dev/Shooter/Binaries/Linux/libUnrealEditor-Shooter.so
Total execution time: 4.21 seconds`
	r := parseUBT(out, 6, "/home/dev/Shooter")
	if r.First.Kind != "uht" || r.First.Line != 18 || r.First.File != "Source/Shooter/Weapon.h" {
		t.Fatalf("expected UHT root cause, got %+v", r.First)
	}
}

func TestClangLinux(t *testing.T) {
	out := `/home/dev/Shooter/Source/Shooter/Weapon.cpp:57:3: error: no member named 'Fier' in 'AWeapon'; did you mean 'Fire'?
   57 |         Fier();
      |         ^~~~
1 error generated.`
	r := parseUBT(out, 6, "/home/dev/Shooter")
	if r.First.Kind != "compile" || r.First.Line != 57 || r.First.Column != 3 {
		t.Fatalf("got %+v", r.First)
	}
}

func TestClangWindowsPathWithDrive(t *testing.T) {
	d, ok := parseUBTLine(`C:\Shooter\Source\A.cpp:9:2: error: expected ';' after expression`)
	if !ok || d.File != `C:\Shooter\Source\A.cpp` || d.Line != 9 {
		t.Fatalf("drive-letter path broke: %+v ok=%v", d, ok)
	}
}

func TestMSVCLinker(t *testing.T) {
	out := `Module.Shooter.cpp.obj : error LNK2019: unresolved external symbol "__declspec(dllimport) public: static class UClass * __cdecl UEnhancedInputComponent::StaticClass(void)" referenced in function "protected: virtual void __cdecl AShooterCharacter::SetupPlayerInputComponent(class UInputComponent *)"
UnrealEditor-Shooter.dll : fatal error LNK1120: 1 unresolved externals`
	r := parseUBT(out, 6, "")
	if r.First.Kind != "link" || r.First.Code != "LNK2019" {
		t.Fatalf("got %+v", r.First)
	}
}

func TestLLDUndefinedSymbol(t *testing.T) {
	out := `ld.lld: error: undefined symbol: UGameplayAbility::UGameplayAbility(FObjectInitializer const&)
>>> referenced by Module.Shooter.cpp`
	r := parseUBT(out, 6, "")
	if r.First.Kind != "link" {
		t.Fatalf("got %+v", r.First)
	}
}

func TestLiveCodingLock(t *testing.T) {
	out := `Unable to build while Live Coding is active. Exit the editor and game, or press Ctrl+Alt+F11 if iterating on code in the editor or game`
	r := parseUBT(out, 6, "")
	if r.First.Code != "UBT-LIVECODING" {
		t.Fatalf("got %+v", r.First)
	}
}

func TestSuccess(t *testing.T) {
	r := parseUBT("Result: Succeeded\nTotal execution time: 12.30 seconds\n", 0, "")
	if !r.OK || r.First != nil {
		t.Fatalf("expected OK, got %+v", r)
	}
}

func TestWarningsAloneDoNotFail(t *testing.T) {
	r := parseUBT(`/p/Source/A.cpp:3:1: warning: unused variable 'x'`, 0, "")
	if !r.OK {
		t.Fatal("warnings must not fail the build")
	}
}

func TestUnrecognisedFailureFallsBack(t *testing.T) {
	r := parseUBT("something exploded\nin a way nobody predicted\n", 3, "")
	// "EXIT", not "UBT-EXIT": the fallback is shared by every pattern parser
	// now, and a UBT-specific code would be wrong on a cmake build parsed by
	// "gcc".
	if r.OK || r.First.Code != "EXIT" || r.First.Raw == "" {
		t.Fatalf("expected the exit-code fallback, got %+v", r.First)
	}
}

func TestSignatureIgnoresLineShift(t *testing.T) {
	a := Diagnostic{Kind: "compile", Code: "C2065", File: "Source/A.cpp", Line: 42, Message: "'x': undeclared identifier"}
	b := a
	b.Line = 47
	b.Message = "'x':   undeclared   identifier"
	if Signature(a) != Signature(b) {
		t.Fatalf("signatures differ:\n%s\n%s", Signature(a), Signature(b))
	}
	c := a
	c.Message = "'y': undeclared identifier"
	if Signature(a) == Signature(c) {
		t.Fatal("different identifiers must give different signatures")
	}
}

func TestFormat(t *testing.T) {
	d := Diagnostic{Kind: "compile", Code: "C2065", File: "Source/A.cpp", Line: 42, Column: 10, Message: "'x': undeclared identifier"}
	want := "Source/A.cpp:42:10: compile error C2065: 'x': undeclared identifier"
	if got := Format(d); got != want {
		t.Fatalf("got %q", got)
	}
}

// ---- Windows forms that silently produced no diagnostic ---------------------

// A drive letter contains a colon, and the linker patterns excluded ':' from
// the file. Every Windows link failure parsed as nothing at all and fell
// through to the exit-code fallback, losing the LNK code and the symbol.
func TestLinkerWindowsAbsolutePaths(t *testing.T) {
	for _, tc := range []struct{ name, line, file, code string }{
		{"LNK2019 with drive",
			`C:\Projects\Shooter\Intermediate\Build\Win64\Module.Shooter.cpp.obj : error LNK2019: unresolved external symbol "foo" referenced in function "bar"`,
			`C:\Projects\Shooter\Intermediate\Build\Win64\Module.Shooter.cpp.obj`, "LNK2019"},
		{"LNK1120 with drive",
			`C:\Projects\Shooter\Binaries\Win64\UnrealEditor-Shooter.dll : fatal error LNK1120: 1 unresolved externals`,
			`C:\Projects\Shooter\Binaries\Win64\UnrealEditor-Shooter.dll`, "LNK1120"},
		{"path with spaces and a drive",
			`C:\Unreal Projects\My Game\Module.cpp.obj : error LNK2001: unresolved external symbol baz`,
			`C:\Unreal Projects\My Game\Module.cpp.obj`, "LNK2001"},
	} {
		d, ok := parseUBTLine(tc.line)
		if !ok {
			t.Errorf("%s: dropped entirely", tc.name)
			continue
		}
		if d.Kind != "link" || d.Code != tc.code || d.File != tc.file {
			t.Errorf("%s: got kind=%q code=%q file=%q", tc.name, d.Kind, d.Code, d.File)
		}
	}
}

func TestUndefinedReferenceWindowsPath(t *testing.T) {
	d, ok := parseUBTLine(`C:\proj\A.o:(.text+0x10): undefined reference to 'UFoo::Bar()'`)
	if !ok {
		t.Fatal("dropped entirely")
	}
	if d.Kind != "link" || d.File != `C:\proj\A.o` {
		t.Fatalf("got kind=%q file=%q", d.Kind, d.File)
	}
}

// MSBuild and Visual Studio prefix every line of a build with "N>". Left in
// place it lands inside File, which then never resolves against the workspace
// and so cannot be read or edited.
func TestMSBuildLinePrefixStripped(t *testing.T) {
	r := parseUBT(`1>Building 4 actions with 16 processes...
1>C:\Projects\Shooter\Source\Shooter\MyActor.cpp(42,10): error C2065: 'Healthh': undeclared identifier
12>Result: Failed`, 6, `C:\Projects\Shooter`)
	if r.First == nil {
		t.Fatal("no diagnostic")
	}
	if r.First.File != "Source/Shooter/MyActor.cpp" {
		t.Fatalf("file = %q, want it relative with the 1> prefix gone", r.First.File)
	}
	if d, ok := parseUBTLine(`1>Module.Shooter.cpp.obj : error LNK2019: unresolved external symbol foo`); !ok || d.File != "Module.Shooter.cpp.obj" {
		t.Fatalf("linker prefix: file = %q ok=%v", d.File, ok)
	}
}

// ---- the first root cause, not the first line ------------------------------

// A fatal error is the root cause; the plain errors in other translation
// units are its consequences. Ranking by kind alone picked whichever came
// first in the log.
func TestFatalErrorOutranksPlainErrorOfSameKind(t *testing.T) {
	r := parseUBT(`A.cpp(9,1): error C2065: 'a': undeclared identifier
B.cpp(3,1): fatal error C1083: Cannot open include file: 'Weapon.generated.h': No such file or directory`, 6, "")
	if r.First == nil || r.First.Code != "C1083" {
		t.Fatalf("want the fatal error as root cause, got %+v", r.First)
	}
	if r.Count != 2 {
		t.Fatalf("count = %d, want 2", r.Count)
	}
}

// ---- the fallback must not collapse every failure into one signature -------

func TestFallbackSignatureDistinguishesFailures(t *testing.T) {
	a := parseUBT("ShooterEditorTarget.cs is malformed\nin some way\n", 6, "")
	b := parseUBT("a completely unrelated catastrophe\n", 6, "")
	if a.Signature == b.Signature {
		t.Fatalf("different failures share a signature:\n%s", a.Signature)
	}
	// ...but the same failure twice must still look the same, even when the
	// noise around it carries different timings.
	c := parseUBT("the same failure\nTotal execution time: 4.21 seconds\n", 6, "")
	d := parseUBT("the same failure\nTotal execution time: 17.90 seconds\n", 6, "")
	if c.Signature != d.Signature {
		t.Fatalf("one failure gave two signatures:\n%s\n%s", c.Signature, d.Signature)
	}
}

// ---- one pass, no per-line retention ---------------------------------------

// A unity build failing in a widely included header emits thousands of error
// lines. Only the best one and the count are ever used, so nothing else may
// be kept.
func TestLargeFailingLogStaysBounded(t *testing.T) {
	var b []byte
	for i := 0; i < 200000; i++ {
		b = append(b, []byte("A.cpp(1,1): error C2065: 'x': undeclared identifier\r\n")...)
	}
	r := parseUBT(string(b), 6, "")
	if r.Count != 200000 {
		t.Fatalf("count = %d, want 200000", r.Count)
	}
	if r.First == nil || r.First.Code != "C2065" {
		t.Fatalf("got %+v", r.First)
	}
	// Bytes, not allocation count: the hazard was keeping a Diagnostic (and
	// its raw line) per error line, which cost about twelve times the log's
	// own size. One pass retaining only the best diagnostic, and submatches
	// read as slices of the line rather than copied into a map per line,
	// brings it to roughly two. Four leaves room for a Go version that
	// allocates differently without hiding a regression of that kind.
	log := string(b)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	parseUBT(log, 6, "")
	runtime.ReadMemStats(&after)
	used := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(4 * len(log)); used > limit {
		t.Errorf("Parse allocated %d bytes for a %d-byte log (%.1fx); it must not keep or copy the log per line",
			used, len(log), float64(used)/float64(len(log)))
	}
}

// parseUBT and parseUBTLine keep the ported tests reading as they did in the
// reference package, where the ubt parser was the package's own Parse.
func parseUBT(output string, exitCode int, workspace string) Diagnosis {
	return parseLines(ubtLines, output, exitCode, workspace)
}

func parseUBTLine(line string) (Diagnostic, bool) { return parseLine(ubtLines, line) }
