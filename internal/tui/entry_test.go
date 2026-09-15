package tui

import (
	"strings"
	"testing"
)

func TestRenderEntryUsesTheGivenStylesAndWidth(t *testing.T) {
	// Render actually differs across themes only under a colour-capable
	// profile; outside a real terminal (as in this test run) lipgloss
	// degrades to no colour and every Render call looks identical
	// regardless of style (see TestNewStylesResolvesEveryThemeAndRejectsUnknown).
	defer pinColorProfile()()
	dark := stylesOr("dark")
	nord := stylesOr("nord")
	e := entry{Kind: entryDim, Text: "attached: phone"}
	if renderEntry(e, dark, 80, false, true) == renderEntry(e, nord, 80, false, true) {
		t.Fatal("dim entry renders identically under two themes")
	}
	user := entry{Kind: entryUser, Label: "you> ", Text: "hello"}
	if got := renderEntry(user, dark, 80, false, true); !strings.Contains(got, "hello") || !strings.Contains(got, "you> ") {
		t.Fatalf("user entry: %q", got)
	}
	tool := entry{Kind: entryTool, Label: "read_file", Text: strings.Repeat("a", 200)}
	full := renderEntry(tool, dark, 120, false, true)
	narrow := renderEntry(tool, dark, 40, true, true)
	if !strings.Contains(full, "● read_file") || len(narrow) >= len(full) {
		t.Fatalf("tool args not truncated for the compact width:\nfull   %q\nnarrow %q", full, narrow)
	}
	md := entry{Kind: entryAssistant, Text: "# Title\n\nsome *text*"}
	if renderEntry(md, dark, 80, false, true) == renderEntry(md, dark, 80, false, false) {
		t.Fatal("assistant entry ignores richText")
	}
	verdict := entry{Kind: entryVerdict, Label: "file_write", Text: "approved"}
	if got := renderEntry(verdict, dark, 80, false, true); !strings.Contains(got, "approved") {
		t.Fatalf("verdict: %q", got)
	}
}

func TestTranscriptRebuildsFromEntriesOnThemeChange(t *testing.T) {
	m := newTestModel(t)
	m.appendEntry(entry{Kind: entryOK, Text: "model set to x"})
	before := m.wrapped
	m.applyTheme("nord", false)
	if m.wrapped == before {
		t.Fatal("theme change did not re-render the transcript")
	}
	if !strings.Contains(m.wrapped, "model set to x") {
		t.Fatalf("entry lost on rebuild: %q", m.wrapped)
	}
	// The theme confirmation is a local note, not a shared entry (another
	// terminal must never see it) — appended after the rebuild, so it is
	// still on screen alongside the rebuilt entry above.
	if len(m.entries) != 1 {
		t.Fatalf("entries = %d, want 1 (theme confirmation must not be a shared entry)", len(m.entries))
	}
	if !strings.Contains(m.wrapped, "theme set to nord") {
		t.Fatalf("theme confirmation missing from this terminal's own transcript: %q", m.wrapped)
	}
}
