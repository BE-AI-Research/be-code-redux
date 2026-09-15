package engine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderIsEmptyForAFreshStore(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	if out := s.Render(6144, func(string) bool { return false }); out != "" {
		t.Fatalf("fresh render %q", out)
	}
}

func TestRenderPriorityMarkersAndBudget(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.AddNoteLine("tests need go on PATH")
	s.SetPlan("add flag", []string{"parse", "wire"})
	s.SetStep(1, "done")
	s.SetStep(2, "doing")
	s.AddNote("cobra owns flags", "", true, false)
	writeFile(t, root, "cmd/root.go", "package cmd\nfunc Execute() {}\n")
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "cmd/root.go"}, Content: numbered("package cmd\nfunc Execute() {}\n", 1)})
	s.AddNote("flags are parsed here", "cmd/root.go", false, false)
	writeFile(t, root, "cmd/root.go", "package cmd\nfunc Execute() {}\n// changed\n")
	// glob sorts before pattern in the canonical key; the row must still
	// name the pattern, not the glob.
	s.Observe(Event{Tool: "search", Args: map[string]any{"pattern": "Execute", "glob": "*.go"}, Content: "cmd/root.go:2:func Execute() {}"})
	out := s.Render(6144, func(p string) bool { return p == "cmd/root.go" })
	want := []string{
		"tests need go on PATH",
		"Task: add flag",
		"doing: 2. wire",
		"done: 1. parse",
		"decisions:\n- cobra owns flags",
		"facts:\n- flags are parsed here",
		"cmd/root.go (lines 1–2) — flags are parsed here [changed since read]",
		`search "Execute": cmd/root.go:2`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("render lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "outline:") {
		t.Fatal("outline repeated although the repo map has the file")
	}
	// Outline appears when the map does not carry the file.
	out2 := s.Render(6144, func(string) bool { return false })
	if !strings.Contains(out2, "outline: Execute") {
		t.Fatalf("outline missing:\n%s", out2)
	}
	// Budget trims at a line boundary, notes and task first to survive.
	small := s.Render(120, func(string) bool { return false })
	if !strings.HasPrefix(small, "tests need go on PATH") || len(small) > 120 || strings.Contains(small, "search") {
		t.Fatalf("budgeted render:\n%s", small)
	}
	if idx := strings.Index(out, "Task:"); idx < strings.Index(out, "tests need") {
		t.Fatal("notes must come before the task")
	}
}

func TestLedgerTextStoppedAtAndFileNotes(t *testing.T) {
	s, root := openTest(t, "s1", false)
	if s.StoppedAt() != "" {
		t.Fatal("stopped-at on an empty ledger")
	}
	s.SetPlan("t", []string{"one", "two"})
	s.SetStep(2, "doing")
	if s.StoppedAt() != "two" {
		t.Fatalf("stopped at %q", s.StoppedAt())
	}
	if lt := s.LedgerText(); !strings.Contains(lt, "[ ] 1. one") || !strings.Contains(lt, "[>] 2. two") {
		t.Fatalf("ledger text:\n%s", lt)
	}
	writeFile(t, root, "a.go", "package a\n")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered("package a\n", 1)})
	s.ApplyFileNotes("- a.go — entry point\n- unknown.go — ignored\n")
	if s.Digests()[0].Note != "entry point" || len(s.Digests()) != 1 {
		t.Fatalf("file notes %+v", s.Digests())
	}
}

func TestTrimLinesIsUTF8Safe(t *testing.T) {
	// No newline anywhere, so trimLines must fall back to a rune-boundary
	// cut. Each "é" is 2 bytes; a byte-5 cut of 10 of them lands mid-rune.
	s := strings.Repeat("é", 10)
	out := trimLines(s, 5)
	if !utf8.ValidString(out) {
		t.Fatalf("trimmed string is not valid UTF-8: %q", out)
	}
	if len(out) > 5 {
		t.Fatalf("trimmed string exceeds budget: %q (%d bytes)", out, len(out))
	}
}

func TestSplitFilesBlockAndParsePlanSteps(t *testing.T) {
	body, files := SplitFilesBlock("Summary text.\nMore.\n\nfiles:\n- a.go — main\n- b.go — helper\n")
	if body != "Summary text.\nMore." || !strings.Contains(files, "a.go — main") {
		t.Fatalf("split: %q | %q", body, files)
	}
	if b, f := SplitFilesBlock("no block"); b != "no block" || f != "" {
		t.Fatalf("no block: %q %q", b, f)
	}
	steps := ParsePlanSteps("Plan:\n1. parse flags\n2) wire cobra\n- write tests\n* docs\nnot a step\n")
	if len(steps) != 4 || steps[0] != "parse flags" || steps[3] != "docs" {
		t.Fatalf("steps %v", steps)
	}
}
