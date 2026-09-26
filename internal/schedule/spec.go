package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const unknownFormHelp = "use one of: in 20m · at 09:00 · at 2026-09-27 09:00 · every 30m · " +
	"daily 09:00 · weekdays 09:00 · mon,thu 14:30 · a five-field cron expression"

type kind int

const (
	kindIn kind = iota
	kindAt
	kindEvery
	kindCron
)

// Spec is a parsed `when`. Build one with Parse or ParseIn.
type Spec struct {
	src    string
	kind   kind
	loc    *time.Location
	due    time.Time     // kindIn, kindAt
	every  time.Duration // kindEvery
	anchor time.Time     // kindEvery: the grid's origin (creation)
	cron   *cronExpr     // kindCron; daily/weekdays/day lists become cron
}

// Parse reads a `when` in local time. created anchors relative forms, so
// re-parsing a stored schedule with its own Created gives the same times.
func Parse(s string, created time.Time) (Spec, error) { return ParseIn(s, created, time.Local) }

// ParseIn is Parse in an explicit location (tests).
func ParseIn(s string, created time.Time, loc *time.Location) (Spec, error) {
	src := strings.Join(strings.Fields(s), " ")
	f := strings.Fields(strings.ToLower(src))
	if len(f) == 0 {
		return Spec{}, fmt.Errorf("no time given; %s", unknownFormHelp)
	}
	base := Spec{src: src, loc: loc}
	switch f[0] {
	case "in", "every":
		if len(f) != 2 {
			return Spec{}, fmt.Errorf("use: %s <duration>, e.g. %s 30m", f[0], f[0])
		}
		d, err := time.ParseDuration(f[1])
		if err != nil || d <= 0 {
			return Spec{}, fmt.Errorf("bad duration %q (examples: 20m, 2h, 1h30m)", f[1])
		}
		if f[0] == "in" {
			base.kind, base.due = kindIn, created.Add(d)
		} else {
			base.kind, base.every, base.anchor = kindEvery, d, created
		}
		return base, nil
	case "at":
		return parseAt(base, f[1:], created)
	case "daily", "weekdays":
		if len(f) != 2 {
			return Spec{}, fmt.Errorf("use: %s HH:MM", f[0])
		}
		h, m, err := clockTime(f[1])
		if err != nil {
			return Spec{}, err
		}
		dow := "*"
		if f[0] == "weekdays" {
			dow = "1-5"
		}
		return cronSpec(base, fmt.Sprintf("%d %d * * %s", m, h, dow))
	}
	if days, ok := dayList(f[0]); ok {
		if len(f) != 2 {
			return Spec{}, fmt.Errorf("use: %s HH:MM", f[0])
		}
		h, m, err := clockTime(f[1])
		if err != nil {
			return Spec{}, err
		}
		return cronSpec(base, fmt.Sprintf("%d %d * * %s", m, h, days))
	} else if strings.ContainsAny(f[0], ",") && !strings.ContainsAny(f[0], "0123456789*") {
		return Spec{}, fmt.Errorf("unknown day in %q (use mon,tue,wed,thu,fri,sat,sun)", f[0])
	}
	if len(f) == 5 {
		return cronSpec(base, src)
	}
	return Spec{}, fmt.Errorf("%q is not a time I know; %s", src, unknownFormHelp)
}

func parseAt(base Spec, f []string, created time.Time) (Spec, error) {
	base.kind = kindAt
	switch len(f) {
	case 1:
		h, m, err := clockTime(f[0])
		if err != nil {
			return Spec{}, err
		}
		c := created.In(base.loc)
		t := wall(c.Year(), c.Month(), c.Day(), h, m, base.loc)
		if !t.After(created) {
			n := c.AddDate(0, 0, 1)
			t = wall(n.Year(), n.Month(), n.Day(), h, m, base.loc)
		}
		base.due = t
		return base, nil
	case 2:
		d, err := time.ParseInLocation("2006-01-02", f[0], base.loc)
		if err != nil {
			return Spec{}, fmt.Errorf("bad date %q (use YYYY-MM-DD)", f[0])
		}
		h, m, err := clockTime(f[1])
		if err != nil {
			return Spec{}, err
		}
		base.due = wall(d.Year(), d.Month(), d.Day(), h, m, base.loc)
		return base, nil
	}
	return Spec{}, fmt.Errorf("use: at HH:MM or at YYYY-MM-DD HH:MM")
}

func cronSpec(base Spec, expr string) (Spec, error) {
	e, err := parseCron(expr)
	if err != nil {
		return Spec{}, err
	}
	base.kind, base.cron = kindCron, e
	return base, nil
}

func clockTime(s string) (int, int, error) {
	hs, ms, ok := strings.Cut(s, ":")
	h, e1 := strconv.Atoi(hs)
	m, e2 := strconv.Atoi(ms)
	if !ok || e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(ms) != 2 {
		return 0, 0, fmt.Errorf("bad time %q (use HH:MM, 24-hour)", s)
	}
	return h, m, nil
}

var dayNums = map[string]string{"sun": "0", "mon": "1", "tue": "2", "wed": "3", "thu": "4", "fri": "5", "sat": "6"}

func dayList(s string) (string, bool) {
	var out []string
	for _, d := range strings.Split(s, ",") {
		n, ok := dayNums[d]
		if !ok {
			return "", false
		}
		out = append(out, n)
	}
	return strings.Join(out, ","), true
}

// wall is the instant a local clock reads y-mo-d h:mi. A time inside a
// spring-forward gap does not exist: it becomes the first instant after the
// gap. A time that occurs twice (fall-back) is the first occurrence.
func wall(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, mi, 0, 0, loc)
	if t.Hour() != h || t.Minute() != mi {
		return transitionNear(t)
	}
	if e := t.Add(-time.Hour); e.Hour() == h && e.Minute() == mi && e.Day() == d {
		return e
	}
	return t
}

// transitionNear finds the zone transition within three hours of t by
// bisection: the first instant carrying the later offset.
func transitionNear(t time.Time) time.Time {
	lo, hi := t.Add(-3*time.Hour), t.Add(3*time.Hour)
	_, offLo := lo.Zone()
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, o := mid.Zone(); o == offLo {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi.Truncate(time.Second)
}

func (s Spec) Recurring() bool { return s.kind == kindEvery || s.kind == kindCron }
func (s Spec) String() string   { return s.src }

// Due is a one-off's fixed time.
func (s Spec) Due() (time.Time, bool) {
	if s.Recurring() {
		return time.Time{}, false
	}
	return s.due, true
}

// Next is the first due time strictly after `after`.
func (s Spec) Next(after time.Time) (time.Time, bool) {
	switch s.kind {
	case kindIn, kindAt:
		if s.due.After(after) {
			return s.due, true
		}
		return time.Time{}, false
	case kindEvery:
		if after.Before(s.anchor) {
			return s.anchor.Add(s.every), true
		}
		k := after.Sub(s.anchor)/s.every + 1
		return s.anchor.Add(k * s.every), true
	case kindCron:
		return s.cron.next(after, s.loc)
	}
	return time.Time{}, false
}

// ShortestGap is the smallest distance between two consecutive due times
// over the next hundred, from `from`; 0 for a one-off.
func (s Spec) ShortestGap(from time.Time) time.Duration {
	if !s.Recurring() {
		return 0
	}
	if s.kind == kindEvery {
		return s.every
	}
	var gap time.Duration
	prev, ok := s.Next(from)
	for i := 0; ok && i < 100; i++ {
		n, ok2 := s.Next(prev)
		if !ok2 {
			break
		}
		if d := n.Sub(prev); gap == 0 || d < gap {
			gap = d
		}
		prev = n
	}
	return gap
}
