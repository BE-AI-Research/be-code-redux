package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronExpr is a five-field cron expression: minute hour day-of-month month
// day-of-week. Each field is a bit set.
type cronExpr struct {
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
}

func parseCron(s string) (*cronExpr, error) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("a cron expression has five fields, got %d", len(f))
	}
	var e cronExpr
	var err error
	if e.min, err = cronField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if e.hour, err = cronField(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if e.dom, err = cronField(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if e.mon, err = cronField(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if e.dow, err = cronField(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if e.dow&(1<<7) != 0 { // 7 is Sunday too
		e.dow |= 1
	}
	e.domStar, e.dowStar = f[2] == "*", f[4] == "*"
	return &e, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			rng, step = part[:i], n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var e1, e2 error
			a, e1 = strconv.Atoi(x)
			b, e2 = strconv.Atoi(y)
			if e1 != nil || e2 != nil || a > b {
				return 0, fmt.Errorf("bad range %q", rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("bad value %q", rng)
			}
			a = n
			if step == 1 {
				b = n
			}
		}
		if a < lo || b > hi {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func (e *cronExpr) dayMatches(t time.Time) bool {
	if e.mon&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom := e.dom&(1<<uint(t.Day())) != 0
	dow := e.dow&(1<<uint(t.Weekday())) != 0
	if !e.domStar && !e.dowStar {
		return dom || dow
	}
	return dom && dow
}

// next is the first matching minute strictly after `after`, in loc.
func (e *cronExpr) next(after time.Time, loc *time.Location) (time.Time, bool) {
	a := after.In(loc)
	day := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 5*366; i++ {
		d := day.AddDate(0, 0, i)
		if !e.dayMatches(d) {
			continue
		}
		for h := 0; h < 24; h++ {
			if e.hour&(1<<uint(h)) == 0 {
				continue
			}
			for m := 0; m < 60; m++ {
				if e.min&(1<<uint(m)) == 0 {
					continue
				}
				if t := wall(d.Year(), d.Month(), d.Day(), h, m, loc); t.After(after) {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}
