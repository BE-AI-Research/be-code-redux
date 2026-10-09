package ui

import "testing"

// pack_install has no "always": it reaches the network and then writes rules
// into the model's own system prompt.
func TestPackInstallHasNoAlways(t *testing.T) {
	if !noAlwaysAction("pack_install") {
		t.Error("pack_install must be listed as having no \"always\"")
	}
}
