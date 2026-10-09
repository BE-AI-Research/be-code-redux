package config

import "testing"

func TestWorkDefaults(t *testing.T) {
	c := Default()
	if !c.Work.Enabled {
		t.Error("the work ledger should be on by default")
	}
	if c.Work.KeepOverwritten != "168h" {
		t.Errorf("keep_overwritten = %q, want 168h", c.Work.KeepOverwritten)
	}
}
