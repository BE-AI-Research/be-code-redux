package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install.cmd is one launcher doing two jobs: run the install.ps1 beside it in
// a checkout, or fetch the published installer from GitHub when it was
// downloaded on its own (the cmd.exe one-line install). GitHub serves it with
// LF line endings, which rules out labels and ( ) blocks — and rules out `&`
// on an `if` line, because cmd.exe treats `&` as an unconditional separator:
// everything after it runs whether the `if` matched or not. `if exist X cmd &
// exit /b` therefore exits even when X is missing, which is exactly the
// GitHub case, so the fallback below it was never reached.
func TestInstallCmdDetectsLocalOrGitHub(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if l == "" || strings.EqualFold(l, "@echo off") || strings.HasPrefix(strings.ToLower(l), "rem") {
			continue
		}
		code = append(code, l)
	}
	if len(code) == 0 {
		t.Fatal("install.cmd has no executable lines")
	}
	for _, l := range code {
		if strings.HasPrefix(strings.ToLower(l), "if ") && strings.Contains(l, "&") {
			t.Errorf("`&` on an `if` line runs unconditionally in cmd.exe, so this fires even when the condition fails:\n  %s", l)
		}
		if strings.Contains(l, "(") || strings.Contains(l, ")") {
			t.Errorf("( ) block in an LF-served batch file:\n  %s", l)
		}
		if strings.HasPrefix(l, ":") || strings.Contains(strings.ToLower(l), "goto ") {
			t.Errorf("label or goto in an LF-served batch file:\n  %s", l)
		}
	}
	// Both jobs must still be there: the local script, and the GitHub fallback.
	joined := strings.Join(code, "\n")
	if !strings.Contains(joined, "install.ps1") {
		t.Error("install.cmd never runs the install.ps1 beside it (the checkout case)")
	}
	if !strings.Contains(joined, "irm https://raw.githubusercontent.com/") {
		t.Error("install.cmd never falls back to the published installer (the download-alone case)")
	}
}
