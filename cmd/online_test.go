package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// runRoot drives rootCmd with args and puts its output and args back
// afterwards, so no other test inherits them.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestOnlineAddRefusesNonTTY(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := runRoot(t, "online", "add", "groq")
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestOnlineRevokeNeedsNoTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := runRoot(t, "online", "revoke", "groq")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no stored credential for groq") {
		t.Fatalf("%s", out)
	}
}

func TestOnlineHasNoKeyFlag(t *testing.T) {
	if len(onlineCmd.Commands()) == 0 {
		t.Fatal("online has no subcommands")
	}
	for _, c := range onlineCmd.Commands() {
		if c.Flags().Lookup("key") != nil {
			t.Fatalf("%s has a --key flag", c.Name())
		}
	}
}
