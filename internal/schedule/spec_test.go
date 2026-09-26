package schedule

import (
	"strings"
	"testing"
	"time"
)

func la(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	return loc
}

func at(loc *time.Location, s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseForms(t *testing.T) {
	loc := la(t)
	created := at(loc, "2026-09-26 10:00") // a Saturday
	cases := []struct {
		in        string
		recurring bool
		next      string // first Next(created), local "2006-01-02 15:04"
	}{
		{"in 20m", false, "2026-09-26 10:20"},
		{"in 1h30m", false, "2026-09-26 11:30"},
		{"at 09:00", false, "2026-09-27 09:00"},
		{"at 11:00", false, "2026-09-26 11:00"},
		{"at 2026-09-28 08:15", false, "2026-09-28 08:15"},
		{"every 30m", true, "2026-09-26 10:30"},
		{"every 2h", true, "2026-09-26 12:00"},
		{"daily 09:00", true, "2026-09-27 09:00"},
		{"weekdays 09:00", true, "2026-09-28 09:00"},
		{"mon,thu 14:30", true, "2026-09-28 14:30"},
		{"0 9 * * 1-5", true, "2026-09-28 09:00"},
		{"*/15 * * * *", true, "2026-09-26 10:15"},
	}
	for _, c := range cases {
		sp, err := ParseIn(c.in, created, loc)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if sp.Recurring() != c.recurring {
			t.Errorf("%q: recurring=%v", c.in, sp.Recurring())
		}
		n, ok := sp.Next(created)
		if !ok || n.Format("2006-01-02 15:04") != c.next {
			t.Errorf("%q: next=%v ok=%v, want %s", c.in, n.Format("2006-01-02 15:04"), ok, c.next)
		}
		if sp.String() != c.in {
			t.Errorf("%q: String()=%q", c.in, sp.String())
		}
	}
}

func TestParseErrors(t *testing.T) {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	for _, in := range []string{"", "soon", "in", "in 0m", "in -5m", "every x", "daily 25:00",
		"daily 9", "at 24:00", "mon,funday 10:00", "61 * * * *", "* * * *", "0 9 * * 8"} {
		if _, err := ParseIn(in, created, time.UTC); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestOneOffIsSpent(t *testing.T) {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	sp, _ := ParseIn("in 20m", created, time.UTC)
	due, ok := sp.Due()
	if !ok || !due.Equal(created.Add(20*time.Minute)) {
		t.Fatalf("due=%v ok=%v", due, ok)
	}
	if _, ok := sp.Next(due); ok {
		t.Fatal("a one-off has no time after its own")
	}
	rec, _ := ParseIn("every 30m", created, time.UTC)
	if _, ok := rec.Due(); ok {
		t.Fatal("recurring has no single due time")
	}
}

func TestEveryStaysOnGrid(t *testing.T) {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	sp, _ := ParseIn("every 30m", created, time.UTC)
	// A run that started late (10:47) does not drift the grid.
	n, _ := sp.Next(time.Date(2026, 9, 26, 10, 47, 0, 0, time.UTC))
	if n.Format("15:04") != "11:00" {
		t.Fatalf("next=%s want 11:00", n.Format("15:04"))
	}
	// Exactly on a grid point: the next one, never the same instant.
	n, _ = sp.Next(time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC))
	if n.Format("15:04") != "11:30" {
		t.Fatalf("next=%s want 11:30", n.Format("15:04"))
	}
}

func TestDSTGapRunsAtGapEnd(t *testing.T) {
	loc := la(t)
	// 2026-03-08: 02:00 PST jumps to 03:00 PDT. 02:30 does not exist.
	sp, err := ParseIn("daily 02:30", at(loc, "2026-03-01 12:00"), loc)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := sp.Next(at(loc, "2026-03-07 23:00"))
	if got := n.Format("2006-01-02 15:04 MST"); got != "2026-03-08 03:00 PDT" {
		t.Fatalf("got %s", got)
	}
	// And it runs once that day, not again at 03:30 or any other time.
	n2, _ := sp.Next(n)
	if got := n2.Format("2006-01-02 15:04"); got != "2026-03-09 02:30" {
		t.Fatalf("second=%s", got)
	}
}

func TestDSTOverlapRunsOnce(t *testing.T) {
	loc := la(t)
	// 2026-11-01: 01:00–02:00 happens twice.
	sp, _ := ParseIn("daily 01:30", at(loc, "2026-10-25 12:00"), loc)
	first, _ := sp.Next(at(loc, "2026-10-31 23:00"))
	if got := first.Format("15:04 MST"); got != "01:30 PDT" {
		t.Fatalf("first=%s want the earlier 01:30", got)
	}
	second, _ := sp.Next(first)
	if second.Format("2006-01-02") != "2026-11-02" {
		t.Fatalf("ran twice: second=%s", second)
	}
}

func TestMonthEndsAndCronOr(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sp, _ := ParseIn("0 12 31 * *", created, time.UTC)
	n, _ := sp.Next(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if n.Format("2006-01-02") != "2026-03-31" {
		t.Fatalf("31st after Feb: %s", n)
	}
	// Both day-of-month and day-of-week restricted: either matches (cron rule).
	sp, _ = ParseIn("0 12 1 * 1", created, time.UTC)
	n, _ = sp.Next(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)) // Saturday
	if n.Format("2006-01-02") != "2026-09-28" { // the Monday comes before Oct 1
		t.Fatalf("dom|dow: %s", n)
	}
	// 7 is Sunday, like 0.
	sp, _ = ParseIn("0 12 * * 7", created, time.UTC)
	n, _ = sp.Next(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	if n.Weekday() != time.Sunday {
		t.Fatalf("dow 7: %s", n.Weekday())
	}
}

func TestShortestGap(t *testing.T) {
	created := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"every 2m": 2 * time.Minute, "* * * * *": time.Minute, "*/10 * * * *": 10 * time.Minute,
		"daily 09:00": 24 * time.Hour, "in 5m": 0,
	} {
		sp, err := ParseIn(in, created, time.UTC)
		if err != nil {
			t.Fatal(in, err)
		}
		if got := sp.ShortestGap(created); got != want {
			t.Errorf("%q: gap %v want %v", in, got, want)
		}
	}
}

func TestParseCollapsesWhitespace(t *testing.T) {
	sp, err := ParseIn("  daily   09:00 ", time.Now(), time.UTC)
	if err != nil || sp.String() != "daily 09:00" {
		t.Fatalf("%q %v", sp.String(), err)
	}
	if !strings.Contains(unknownFormHelp, "every 30m") {
		t.Fatal("help text should list the forms")
	}
}
