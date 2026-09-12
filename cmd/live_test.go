package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// testFlags mirrors the root command's persistent flags, so the argument
// builder is exercised over the same shapes (string and bool) it sees in
// production without mutating the real, global flag set.
func testFlags() *pflag.FlagSet {
	fs := pflag.NewFlagSet("root", pflag.ContinueOnError)
	fs.String("provider", "", "")
	fs.String("model", "", "")
	fs.String("dir", ".", "")
	fs.Bool("yes", false, "")
	fs.String("resume", "", "")
	fs.Bool("ide", false, "")
	fs.Bool("no-ide", false, "")
	fs.Bool("no-host", false, "")
	fs.String("session-host", "", "")
	return fs
}

func TestHostArgsForwardsOnlyChangedFlags(t *testing.T) {
	fs := testFlags()
	got := hostArgs("A1B2C3", "/work/space", fs)
	want := []string{"A1B2C3", "--dir=/work/space"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestHostArgsForwardsEveryChangedFlag(t *testing.T) {
	fs := testFlags()
	// The flags a served session would otherwise lose: approvals and the
	// editor bridge are decided in the host, not the launcher.
	for name, val := range map[string]string{
		"yes":      "true",
		"no-ide":   "true",
		"provider": "lan",
		"model":    "qwen3:8b",
		"resume":   "ZZ9QQ9",
	} {
		if err := fs.Set(name, val); err != nil {
			t.Fatal(err)
		}
	}
	got := hostArgs("A1B2C3", "/work/space", fs)
	if got[0] != "A1B2C3" {
		t.Fatalf("first arg %q, want the session code", got[0])
	}
	for _, want := range []string{
		"--dir=/work/space", "--yes=true", "--no-ide=true",
		"--provider=lan", "--model=qwen3:8b", "--resume=ZZ9QQ9",
	} {
		if !contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if len(got) != 7 {
		t.Errorf("got %d args %q, want 7 (code + dir + 5 changed)", len(got), got)
	}
}

func TestHostArgsNeverForwardsLauncherOnlyFlags(t *testing.T) {
	for _, name := range []string{"session-host", "no-host", "dir"} {
		fs2 := testFlags()
		val := "true"
		if fs2.Lookup(name).Value.Type() == "string" {
			val = "LAUNCHER"
		}
		if err := fs2.Set(name, val); err != nil {
			t.Fatal(err)
		}
		for _, arg := range hostArgs("A1B2C3", "/work/space", fs2) {
			if strings.HasPrefix(arg, "--"+name+"=") && arg != "--dir=/work/space" {
				t.Errorf("%s forwarded as %q", name, arg)
			}
		}
	}
}

// TestRootHasTheFlagsHostArgsSkips guards the skip list against a rename:
// if one of these flags disappears, the skip is silently meaningless.
func TestRootHasTheFlagsHostArgsSkips(t *testing.T) {
	fs := rootCmd.PersistentFlags()
	for _, name := range []string{"dir", "session-host", "no-host"} {
		if fs.Lookup(name) == nil {
			t.Errorf("root has no persistent flag %q, but hostArgs skips it", name)
		}
	}
	// And the flags the finding was about must exist to be forwarded.
	for _, name := range []string{"yes", "ide", "no-ide"} {
		if fs.Lookup(name) == nil {
			t.Errorf("root has no persistent flag %q", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The attach command's fallback to a saved session must reach the spawned
// host: assigning flagResume leaves the flag unchanged, and hostArgs forwards
// only changed flags, so the host would resume nothing while the launcher
// derived the live record's code from the saved session — two saved sessions
// then share one resume code.
func TestSetResumeIsForwardedToTheHost(t *testing.T) {
	fs := testFlags()
	if contains(hostArgs("A1B2C3", "/work/space", fs), "--resume=ZZ9QQ9") {
		t.Fatal("resume forwarded while the flag is unchanged")
	}
	for _, arg := range hostArgs("A1B2C3", "/work/space", fs) {
		if strings.HasPrefix(arg, "--resume") {
			t.Fatalf("unchanged resume flag forwarded as %q", arg)
		}
	}
	if err := setResume(fs, "ZZ9QQ9"); err != nil {
		t.Fatal(err)
	}
	if !fs.Lookup("resume").Changed {
		t.Error("setResume did not mark the flag changed")
	}
	if got := fs.Lookup("resume").Value.String(); got != "ZZ9QQ9" {
		t.Errorf("resume value %q, want ZZ9QQ9", got)
	}
	if !contains(hostArgs("A1B2C3", "/work/space", fs), "--resume=ZZ9QQ9") {
		t.Errorf("--resume=ZZ9QQ9 not forwarded: %q", hostArgs("A1B2C3", "/work/space", fs))
	}
}

// setResume goes through the real root flag set in production, so the flag it
// names must exist there and its binding must write flagResume.
func TestSetResumeBindsTheRootFlagVariable(t *testing.T) {
	fs := rootCmd.PersistentFlags()
	f := fs.Lookup("resume")
	if f == nil {
		t.Fatal("root has no persistent --resume flag")
	}
	prev, prevChanged := flagResume, f.Changed
	defer func() {
		f.Value.Set(prev)
		f.Changed = prevChanged
		flagResume = prev
	}()
	if err := setResume(fs, "QQ2222"); err != nil {
		t.Fatal(err)
	}
	if flagResume != "QQ2222" {
		t.Errorf("flagResume is %q, want QQ2222 (the flag is not bound to it)", flagResume)
	}
	if !f.Changed {
		t.Error("root --resume not marked changed")
	}
}
