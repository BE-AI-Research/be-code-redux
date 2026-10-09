package ui

import "testing"

// Each conflict is about a specific pair of changes, so a session-wide
// "always keep mine" is a licence to lose work silently — the thing this
// feature exists to stop.
func TestWorkConflictHasNoAlways(t *testing.T) {
	if !noAlwaysAction("work_conflict") {
		t.Error("work_conflict must be listed as having no \"always\"")
	}
}
