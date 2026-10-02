package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestParseGrant(t *testing.T) {
	ok := map[string]string{
		"shell: go test ./...": "shell: go test ./...",
		"SHELL:git pull":       "shell: git pull",
		"write: docs/":         "write: docs",
		"write: ./docs/api":    "write: docs/api",
		"write: .":             "write: .",
		"browser: Example.com": "browser: example.com",
		"browser: *.example.com": "browser: *.example.com",
	}
	for in, want := range ok {
		g, err := ParseGrant(in)
		if err != nil || g.String() != want {
			t.Errorf("%q → %q, %v; want %q", in, g.String(), err, want)
		}
	}
	for _, in := range []string{"shell: *", "shell:", "write: /etc", "write: ../x", "write: a/../../x",
		"browser: *", "net: x", "go test"} {
		if _, err := ParseGrant(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestAllowanceMatching(t *testing.T) {
	a, err := ParseAllowance([]string{"write: docs", "browser: *.example.com", "browser: localhost"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{"docs/a.md": true, "docs": true, "docs2/a.md": false, "src/x.go": false} {
		if a.WriteAllowed(rel) != want {
			t.Errorf("write %q: want %v", rel, want)
		}
	}
	for host, want := range map[string]bool{"api.example.com": true, "example.com": false, "localhost": true, "": false} {
		if a.HostAllowed(host) != want {
			t.Errorf("host %q: want %v", host, want)
		}
	}
	if a.Broad() {
		t.Error("not broad")
	}
	all, _ := ParseAllowance([]string{"write: ."})
	if !all.WriteAllowed("any/where.txt") || !all.Broad() {
		t.Error("write: . covers the workspace and is broad")
	}
}

func TestScheduleHashCoversApprovedContent(t *testing.T) {
	base := Schedule{ID: "a1", Name: "n", When: "daily 09:00", Instruction: "run tests",
		Allow: Allowance{{Kind: "shell", Value: "go test ./..."}}, State: Active, Created: time.Now()}
	h := base.Hash()
	changed := []func(*Schedule){
		func(s *Schedule) { s.When = "daily 08:00" },
		func(s *Schedule) { s.Instruction = "run tests and push" },
		func(s *Schedule) { s.Task = "3" },
		func(s *Schedule) { s.Allow = append(s.Allow, Grant{Kind: "write", Value: "."}) },
		// final review I3(b): the limits a person approved, and Created,
		// which places an every-N grid (shifting it runs the event earlier).
		func(s *Schedule) { s.MaxRuntime = 3 * time.Hour },
		func(s *Schedule) { s.AskTimeout = time.Hour },
		func(s *Schedule) { s.Created = s.Created.Add(-20 * time.Minute) },
	}
	for i, ch := range changed {
		c := base
		c.Allow = append(Allowance(nil), base.Allow...)
		ch(&c)
		if c.Hash() == h {
			t.Errorf("change %d did not change the hash", i)
		}
	}
	same := base
	same.State, same.LastRun, same.Failures = Paused, time.Now(), 2
	if same.Hash() != h {
		t.Error("run bookkeeping must not change the hash")
	}
	// Created is stored in schedules.md to the second, in some zone: a
	// round trip through the file must not read as a change.
	trunc := base
	trunc.Created = base.Created.Truncate(time.Second).In(time.FixedZone("x", 3600))
	if trunc.Hash() != h {
		t.Error("Created's sub-second part and zone must not change the hash")
	}
}

func TestNextDue(t *testing.T) {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)
	one := Schedule{When: "in 20m", Created: created, State: Active}
	if d, ok := one.NextDue(); !ok || !d.Equal(created.Add(20*time.Minute)) {
		t.Fatalf("one-off due %v %v", d, ok)
	}
	one.LastRun = created.Add(21 * time.Minute)
	if _, ok := one.NextDue(); ok {
		t.Fatal("a one-off that ran is not due again")
	}
	rec := Schedule{When: "every 30m", Created: created, State: Active}
	rec.LastRun = created.Add(95 * time.Minute) // ran at 11:35
	if d, _ := rec.NextDue(); d.Format("15:04") != "12:00" {
		t.Fatalf("recurring next %s", d.Format("15:04"))
	}
	rec.State = Paused
	if _, ok := rec.NextDue(); ok {
		t.Fatal("paused is never due")
	}
}

func TestValidName(t *testing.T) {
	for n, want := range map[string]bool{"nightly-tests": true, "a": true, "Nightly": false, "-x": false, "a b": false, "": false} {
		if ValidName(n) != want {
			t.Errorf("%q: want %v", n, want)
		}
	}
}

// TestGrantRejectsControlCharacters is final review minor: a newline in a
// grant value would render as an extra line of schedules.md.
func TestGrantRejectsControlCharacters(t *testing.T) {
	for _, g := range []string{"shell: go test\nallow: write: .", "write: docs\r", "browser: a\x00b.com", "shell: go\ttest"} {
		if _, err := ParseGrant(g); err == nil {
			t.Errorf("%q accepted", g)
		}
	}
	if _, err := ParseGrant("shell: go test ./..."); err != nil {
		t.Fatal(err)
	}
}

func TestValidTask(t *testing.T) {
	for _, ok := range []string{"", "3", "3.2", "12.1.4"} {
		if !ValidTask(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"a", "3.", ".3", "3..2", "3.2\nallow: write: .", " 3", "3 ", "-1"} {
		if ValidTask(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ParseStanding reads schedules.allow: each entry on its own, so one bad
// entry is reported and dropped while the rest still apply.
func TestParseStanding(t *testing.T) {
	a, errs := ParseStanding([]string{"shell: go test*", "shell: *", "", "write: ../x", "write: docs/", "browser: *.example.com"})
	if len(errs) != 2 {
		t.Fatalf("two refused entries: %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "shell: *") || !strings.Contains(errs[1].Error(), "outside the workspace") {
		t.Fatalf("errors name the entry: %v", errs)
	}
	got := []string{}
	for _, g := range a {
		got = append(got, g.String())
	}
	if strings.Join(got, ",") != "shell: go test*,write: docs,browser: *.example.com" {
		t.Fatalf("kept: %v", got)
	}
}
