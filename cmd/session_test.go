package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/store"
)

// resetSessionFlags clears the token and rotate flags a previous test may
// have set; cobra keeps flag values across tests in one process.
func resetSessionFlags(t *testing.T) {
	t.Helper()
	_ = sessionTokenCmd.Flags().Set("stdout", "false")
	_ = sessionRotateCmd.Flags().Set("stdout", "false")
	_ = sessionRotateCmd.Flags().Set("grace", "")
}

// savedSession points HOME at a scratch dir and saves one session there, so
// the real ~/.be-code/sessions is never written.
func savedSession(t *testing.T) *store.Session {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	resetSessionFlags(t)
	t.Cleanup(func() { resetSessionFlags(t) })
	s := store.NewSession("ollama", "m", t.TempDir())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

// Tests run with a pipe for stdout, so --stdout is what a script would pass.
func TestSessionTokenMintsAndPersists(t *testing.T) {
	s := savedSession(t)
	_ = sessionTokenCmd.Flags().Set("stdout", "true")
	out, err := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{s.ResumeCode()}) })
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "sess_1_") {
		t.Fatalf("%v %q", err, out)
	}
	again, _ := store.Load(s.ID)
	if again.Token != strings.TrimSpace(out) {
		t.Fatal("the minted token is saved")
	}
	out2, _ := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{s.ResumeCode()}) })
	if out2 != out {
		t.Fatal("a second call shows the same token")
	}
}

func TestSessionTokenRefusesANonTTYWithoutStdout(t *testing.T) {
	s := savedSession(t)
	_ = sessionTokenCmd.Flags().Set("stdout", "false")
	out, err := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{s.ResumeCode()}) })
	if err == nil || !strings.Contains(err.Error(), "--stdout") || strings.Contains(out, "sess_1_") {
		t.Fatalf("a pipe without --stdout is refused and nothing is printed: %v %q", err, out)
	}
	if again, _ := store.Load(s.ID); again.Token != "" {
		t.Fatal("a refused token call mints nothing and saves nothing")
	}
}

func TestSessionRotateWithGrace(t *testing.T) {
	s := savedSession(t)
	_ = sessionRotateCmd.Flags().Set("stdout", "true")
	_ = sessionRotateCmd.Flags().Set("grace", "10m")
	first := s.AccessToken()
	_ = s.Save()
	out, err := captureAppsStdout(t, func() error { return sessionRotateCmd.RunE(sessionRotateCmd, []string{s.ResumeCode()}) })
	if err != nil {
		t.Fatal(err)
	}
	again, _ := store.Load(s.ID)
	if again.Token == first || again.PrevToken != first || !again.Accepts(first) || strings.TrimSpace(out) != again.Token {
		t.Fatalf("rotate keeps the old token in grace and prints the new one: %+v %q", again, out)
	}
}

func TestSessionRotateRefusesGraceOver24h(t *testing.T) {
	s := savedSession(t)
	before, _ := store.Load(s.ID)
	_ = sessionRotateCmd.Flags().Set("stdout", "true")
	_ = sessionRotateCmd.Flags().Set("grace", "25h")
	_, err := captureAppsStdout(t, func() error { return sessionRotateCmd.RunE(sessionRotateCmd, []string{s.ResumeCode()}) })
	if err == nil {
		t.Fatal("a grace over 24h is refused")
	}
	after, _ := store.Load(s.ID)
	if after.Token != before.Token {
		t.Fatal("a refused rotate changes nothing")
	}
}

func TestSessionTokenRefusesALiveSession(t *testing.T) {
	s := savedSession(t)
	dir, _ := live.Dir()
	_ = os.MkdirAll(dir, 0o700)
	rec := live.Record{Code: s.ResumeCode(), PID: os.Getpid(), Socket: "x", Workspace: s.Workspace}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	_ = sessionTokenCmd.Flags().Set("stdout", "true")
	_, err := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{s.ResumeCode()}) })
	if err == nil || !strings.Contains(err.Error(), "/session token") {
		t.Fatalf("a live session's file is the host's: %v", err)
	}
}

func TestSessionSaveDoesNotStampHostPID(t *testing.T) {
	s := savedSession(t)
	_ = sessionTokenCmd.Flags().Set("stdout", "true")
	if _, err := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{s.ResumeCode()}) }); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Load(s.ID)
	if again.HostPID != 0 {
		t.Fatalf("a command-line save must not stamp host_pid, got %d", again.HostPID)
	}
}

// A host's session exists in memory before its first autosave, so a live
// code may have no file at all; the refusal must not become a not-found.
func TestSessionTokenRefusesALiveCodeWithNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetSessionFlags(t)
	t.Cleanup(func() { resetSessionFlags(t) })
	dir, _ := live.Dir()
	_ = os.MkdirAll(dir, 0o700)
	rec := live.Record{Code: "LIVE01", PID: os.Getpid(), Socket: "x", Workspace: t.TempDir()}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	_ = sessionTokenCmd.Flags().Set("stdout", "true")
	_, err := captureAppsStdout(t, func() error { return sessionTokenCmd.RunE(sessionTokenCmd, []string{"LIVE01"}) })
	if err == nil || !strings.Contains(err.Error(), "/session token") || !strings.Contains(err.Error(), "be-code attach LIVE01") {
		t.Fatalf("a live code without a file is refused, not not-found: %v", err)
	}
}
