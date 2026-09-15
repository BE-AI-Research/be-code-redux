package agent

import (
	"os"
	"testing"
)

// TestMain points HOME (and USERPROFILE, for Windows) at a throwaway
// directory for the whole package. Code under test reaches ~/.be-code
// through config.Dir(): a test that saves config, a session, a live record
// or the input history must never touch the developer's real dotdir —
// once, the theme tests overwrote a real config.json with defaults.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "be-code-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
