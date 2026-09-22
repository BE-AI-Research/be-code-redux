package tools

import (
	"strings"
	"testing"
)

// api_key_env holds the NAME of a variable. A key pasted there instead was
// printed into a session host's log on every start; whatever does not look
// like a conventional variable name is never echoed.
func TestEnvNameForDisplayNeverEchoesWhatMayBeAKey(t *testing.T) {
	for _, name := range []string{"GOOGLE_PSE_API_KEY", "BE_AI_ENGINE_KEY", "_X1"} {
		if got := EnvNameForDisplay(name); got != name {
			t.Errorf("%q should be shown as it is, got %q", name, got)
		}
	}
	for _, secret := range []string{"AIzaSyExample-_key0123456789", "e3f5109d7eecd41e4", "sk-abc123", "Be-Code-Search", "lower_case", ""} {
		got := EnvNameForDisplay(secret)
		if secret != "" && strings.Contains(got, secret) {
			t.Errorf("%q was echoed: %q", secret, got)
		}
		if !strings.Contains(got, "name of an environment variable") {
			t.Errorf("no explanation for %q: %q", secret, got)
		}
	}
}
