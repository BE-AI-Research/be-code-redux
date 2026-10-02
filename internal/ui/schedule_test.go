package ui

import (
	"strings"
	"testing"
)

func TestParseScheduleAdd(t *testing.T) {
	r, err := ParseScheduleAdd("nightly weekdays 09:00 -- pull, run the tests and allow nothing odd allow shell: go test ./...; write: docs")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "nightly" || r.When != "weekdays 09:00" {
		t.Fatalf("%+v", r)
	}
	if r.Instruction != "pull, run the tests and allow nothing odd" {
		t.Fatalf("instruction %q", r.Instruction)
	}
	if strings.Join(r.Allow, "|") != "shell: go test ./...|write: docs" {
		t.Fatalf("allow %q", r.Allow)
	}
	r, err = ParseScheduleAdd("check in 20m -- look at the build")
	if err != nil || r.When != "in 20m" || len(r.Allow) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "name", "name in 20m", "name -- no time"} {
		if _, err := ParseScheduleAdd(bad); err == nil {
			t.Errorf("%q: want usage error", bad)
		}
	}
}

func TestIsScheduleBlocking(t *testing.T) {
	for args, want := range map[string]bool{"": false, "show x": false, "add a in 5m -- x": true,
		"resume x": true, "pause x": true, "cancel x": true, "run x": true} {
		if IsScheduleBlocking(strings.Fields(args)) != want {
			t.Errorf("%q: want %v", args, want)
		}
	}
}

func TestScheduleInSlashTableAndBusySafe(t *testing.T) {
	found := false
	for _, c := range SlashCommandTable {
		found = found || c.Name == "/schedule"
	}
	if !found || !busySafe["/schedule"] {
		t.Fatal("/schedule is listed and busy-safe")
	}
}

func TestParseScheduleAddToolGrant(t *testing.T) {
	r, err := ParseScheduleAdd("triage daily 09:00 -- read the open issues allow tool: mcp_github_*; shell: go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	if r.Instruction != "read the open issues" || len(r.Allow) != 2 || r.Allow[0] != "tool: mcp_github_*" {
		t.Fatalf("%+v", r)
	}
}
