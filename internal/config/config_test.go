package config

import "testing"

func TestIDEDefaults(t *testing.T) {
	c := Default()
	if !c.IDE.Enabled || !c.IDE.AutoContext {
		t.Fatalf("ide defaults = %+v", c.IDE)
	}
}
