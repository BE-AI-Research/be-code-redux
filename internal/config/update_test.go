package config

import (
	"encoding/json"
	"testing"
)

func TestUpdateCheckDefaultOn(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{}`), &c); err != nil || !c.UpdateCheckOn() {
		t.Fatalf("absent key must mean on (err %v)", err)
	}
	if err := json.Unmarshal([]byte(`{"update_check": false}`), &c); err != nil || c.UpdateCheckOn() {
		t.Fatalf("false must mean off (err %v)", err)
	}
	if !Default().UpdateCheckOn() {
		t.Fatal("default is on")
	}
}
