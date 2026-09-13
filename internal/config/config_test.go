package config

import "testing"

func TestIDEDefaults(t *testing.T) {
	c := Default()
	if !c.IDE.Enabled || !c.IDE.AutoContext {
		t.Fatalf("ide defaults = %+v", c.IDE)
	}
}

func TestStallNoticeSecondsDefault(t *testing.T) {
	if got := Default().StallNoticeSeconds; got != 45 {
		t.Fatalf("stall_notice_seconds default = %d, want 45", got)
	}
}
