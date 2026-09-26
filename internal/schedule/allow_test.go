package schedule

import (
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
