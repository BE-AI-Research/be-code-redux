package config

import "testing"

func TestPacksSourceDefault(t *testing.T) {
	c := Default()
	if c.Packs.Source != "https://github.com/BE-AI-Research/be-skill-packs" {
		t.Errorf("default packs.source = %q", c.Packs.Source)
	}
}
