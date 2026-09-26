package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() Schedule {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	return Schedule{ID: "3f2a9c1b", Name: "nightly-tests", When: "weekdays 09:00",
		Instruction: "pull, run the tests\nand summarise what broke", Task: "3.2",
		Allow:       Allowance{{Kind: "shell", Value: "go test ./..."}, {Kind: "write", Value: "docs"}},
		State:       Active, CreatedBy: "person", Created: created,
		LastRun:     created.Add(23 * time.Hour), LastOutcome: "ok", AskTimeout: 5 * time.Minute}
}

func TestDocGolden(t *testing.T) {
	var d Doc
	d.Put(sample())
	got := d.Render()
	golden := filepath.Join("testdata", "schedules.golden.md")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		os.WriteFile(golden, []byte(got), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("render differs from golden:\n%s", got)
	}
}

func TestDocRoundTripKeepsForeignLines(t *testing.T) {
	var d Doc
	d.Put(sample())
	text := d.Render()
	text = strings.Replace(text, "state: active\n", "state: active\nmy-note: keep me\n", 1)
	text += "\n## broken-one\nwhen: whenever\n"
	d2 := ParseDoc(text)
	if len(d2.Sections) != 2 {
		t.Fatalf("sections %d", len(d2.Sections))
	}
	if d2.Sections[1].Err == nil || d2.Sections[1].Sched != nil {
		t.Fatal("the unparseable section is reported, not dropped")
	}
	s := d2.Schedules()
	if len(s) != 1 || s[0].Instruction != sample().Instruction || s[0].Hash() != sample().Hash() {
		t.Fatalf("round trip lost content: %+v", s)
	}
	out := d2.Render()
	if !strings.Contains(out, "my-note: keep me") || !strings.Contains(out, "## broken-one\nwhen: whenever") {
		t.Fatalf("foreign lines lost:\n%s", out)
	}
}

func TestDocPutRemove(t *testing.T) {
	var d Doc
	a := sample()
	d.Put(a)
	a.State = Paused
	d.Put(a)
	if len(d.Schedules()) != 1 || d.Schedules()[0].State != Paused {
		t.Fatal("Put replaces by ID")
	}
	if !d.Remove(a.ID) || len(d.Schedules()) != 0 {
		t.Fatal("Remove")
	}
}

func TestLoadSaveAndBroken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".be-code", "schedules.md")
	d, err := LoadFile(p)
	if err != nil || len(d.Sections) != 0 {
		t.Fatalf("missing file is empty: %v", err)
	}
	d.Put(sample())
	if err := SaveFile(p, d); err != nil {
		t.Fatal(err)
	}
	back, err := LoadFile(p)
	if err != nil || len(back.Schedules()) != 1 {
		t.Fatalf("reload: %v", err)
	}
	os.WriteFile(p, []byte{0xff, 0xfe, 'x'}, 0o644)
	if _, err := LoadFile(p); !errors.Is(err, ErrBroken) {
		t.Fatalf("invalid UTF-8 is broken, got %v", err)
	}
	aside, err := RenameBroken(p, time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	if err != nil || !strings.Contains(aside, "schedules.broken-") {
		t.Fatalf("aside %q %v", aside, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the broken file is moved, not copied")
	}
}

func TestApprovals(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engine", "k", "schedules.json")
	a := LoadApprovals(p)
	if len(a) != 0 {
		t.Fatal("missing is empty")
	}
	a["x"] = "h"
	if err := a.Save(p); err != nil {
		t.Fatal(err)
	}
	if LoadApprovals(p)["x"] != "h" {
		t.Fatal("round trip")
	}
	os.WriteFile(p, []byte("{nope"), 0o600)
	if len(LoadApprovals(p)) != 0 {
		t.Fatal("corrupt is empty")
	}
}
