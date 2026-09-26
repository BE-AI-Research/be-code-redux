# Scheduled Events Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a running BE-Code session wake the model at chosen times to do a pre-decided piece of work, under an allowance a person approved up front.

**Architecture:** A new stdlib-only leaf package `internal/schedule` parses times, matches allowances and reads/writes the project's `schedules.md`. An agent-owned `scheduler` goroutine queues due events into the existing inbox as `Scheduled` items; the UIs' leftover-queue drain hands one out per turn, and `RunFull` runs it under `Registry.SetAllowance`, which the shell, process, file-write and browser tools consult just before they would prompt.

**Tech Stack:** Go (stdlib only in the new package), Bubble Tea TUI, the existing `tools.Registry` approval seam.

**Spec:** `docs/superpowers/specs/2026-09-26-schedules-design.md`

## Global Constraints

- All commands run from `be-code/` (the Go module root: `/home/sbrown/Documents/Development/BE-CodeRedux/be-code/be-code`). Branch `feature/schedules`.
- `internal/schedule` imports the standard library only. `internal/tools`, `internal/store` and `internal/agent` may import it; it imports none of them.
- Before and after every test run: `stat -c %y ~/.be-code/config.json` must be unchanged (tests must never touch the real `~/.be-code`). Never run `pkill -f be-code`.
- `make -f build.mk verify` passes at the end of every task.
- Every commit message ends with these two lines:
  ```
  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9
  ```
- New approval action name: `schedule`. It has no "always": the TUI's `a` does nothing on it, the REPL prompts `approve? [y/N] `, and `-y` never approves it.
- Config keys and defaults, verbatim: `schedules.enabled` true, `min_interval` "5m", `max_active` 20, `ask_timeout` "10m", `max_runtime` "30m", `pause_after_failures` 3.
- Project file: `<workspace>/.be-code/schedules.md`. Approval hashes: `~/.be-code/engine/<engine.Key(root)>/schedules.json`. One-off timers: `store.Session.Timers` (JSON `timers`).
- Fired request header, verbatim format: `[Scheduled event "<name>" — <when>, set by <you|the model> <YYYY-MM-DD>]` then a newline and the instruction.
- Transcript entry `⏰ <name>`. Status segment `next: <name> <HH:MM>`.
- Version becomes 1.2.0 (`build.mk` VERSION, CHANGELOG).

**Rulings recorded while planning (deviations from the spec's letter):**
- Spec §6 asks for an e2e scenario against the mock backend. `test/e2e` drives only headless `run`, and the spec itself forbids a scheduler there, so the end-to-end path (fake clock → queue → drained turn → allowance → outcome) is an agent-level integration test in Task 5 instead, plus live-checklist items in Task 10. Cost if wrong: a wiring bug between cmd and the UIs could slip past tests; Task 8's UI tests and the live checklist cover that seam.
- Spec §2.4's withdrawal wording: the shared-ask modal already closes with its own `noAnswerNote` when a deadline passes, so the schedule-specific sentence goes out as an agent notice (`scheduled event "<name>": nobody answered <action> within <n>; refused`) rather than as the modal's note. Cost if wrong: a wording change in one place.
- The model-facing guidance for the tool lives in the tool's `Description` (it "rides with the tool"), not in `prompt.go`.

## Review Focus

1. A one-off timer whose time passed while the session was closed — should run once, right after the startup prompt, and then be `done` (Task 5 test `TestMissedOneOffRunsOnce`).
2. A person drops a queued scheduled event from the queue popup — the schedule must fire again next time, not be stuck "already queued" forever (Task 5 test `TestDroppedQueuedEventFiresAgain`).
3. `schedules.md` hand-edited to add a grant mid-session — must not run with the new grant; it pauses with a notice (Task 5 test `TestHandEditedSchedulePausesAtFire`).
4. A model that passes `allow` as one string instead of an array (small local models do) — accepted and split on `;` / newlines (Task 7 test `TestScheduleToolAcceptsAllowAsString`).
5. A spring-forward day with `daily 02:30` — runs once at 03:00, not twice and not skipped (Task 1 test `TestDSTGapRunsAtGapEnd`).

---

## File structure

| File | Responsibility |
| --- | --- |
| `internal/schedule/spec.go` | `Spec`, `Parse`/`ParseIn`, `Next`, `Recurring`, `ShortestGap`, DST handling |
| `internal/schedule/cron.go` | five-field cron parsing and matching |
| `internal/schedule/clock.go` | `Clock`, `Timer`, `RealClock`, `FakeClock` |
| `internal/schedule/allow.go` | `Grant`, `Allowance`, parsing and matching |
| `internal/schedule/schedule.go` | `Schedule`, `State`, `Request`, `Hash`, `NextDue`, `NewID`, `ValidName` |
| `internal/schedule/doc.go` | `schedules.md` parse/render, `LoadFile`/`SaveFile`/`RenameBroken`, `Approvals` |
| `internal/config/config.go` | `SchedulesConfig` + defaults |
| `internal/store/sessions.go` | `Session.Timers` |
| `internal/tools/allowance.go` | `Registry.SetAllowance`/`ClearAllowance`/`ask` and matchers |
| `internal/tools/shell.go`, `process.go`, `fs.go`, `browser.go` | consult the allowance; ask via `r.ask` |
| `internal/tools/schedule.go` | the `schedule` tool |
| `internal/agent/inbox.go` | `Scheduled` items, `DrainForTurn`, `EnqueueScheduled` |
| `internal/agent/schedules.go` | the scheduler: stores, loop, fire, `runFired`, outcomes |
| `internal/agent/schedules_api.go` | `AddSchedule`, `ScheduleAction`, `ScheduleLines`, `ScheduleShow`, `NextSchedule`, startup gate |
| `internal/agent/loop.go` | `RunFull` split; `Events.OnScheduleFire` |
| `internal/tui/*` | `entrySchedule`, theme colour, modal, queue drain, status segment, `/schedule` |
| `internal/ui/*` | REPL drain/prompt/wake, `/schedule`, `ScheduleLines`, add-command parser, slash table |
| `cmd/root.go`, `cmd/live.go`, `cmd/commands.go` | enable, start, stop; headless refusal |

---

### Task 1: `internal/schedule` — times

**Files:**
- Create: `internal/schedule/spec.go`, `internal/schedule/cron.go`, `internal/schedule/clock.go`
- Test: `internal/schedule/spec_test.go`, `internal/schedule/clock_test.go`

**Interfaces:**
- Produces:
  - `func Parse(s string, created time.Time) (Spec, error)` (uses `time.Local`)
  - `func ParseIn(s string, created time.Time, loc *time.Location) (Spec, error)`
  - `func (s Spec) Next(after time.Time) (time.Time, bool)` — false for a one-off whose time is not after `after`
  - `func (s Spec) Due() (time.Time, bool)` — the fixed time of a one-off; false for recurring
  - `func (s Spec) Recurring() bool`, `func (s Spec) String() string`
  - `func (s Spec) ShortestGap(from time.Time) time.Duration` — 0 for a one-off
  - `type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }`, `type Timer interface { C() <-chan time.Time; Stop() bool }`, `type RealClock struct{}`
  - `type FakeClock` with `NewFakeClock(t time.Time) *FakeClock`, `Advance(d time.Duration)`, `Set(t time.Time)`

- [ ] **Step 1: Write the failing tests**

`internal/schedule/spec_test.go`:

```go
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
```

`internal/schedule/clock_test.go`:

```go
package schedule

import (
	"testing"
	"time"
)

func TestFakeClockFiresOnAdvance(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	tm := c.NewTimer(10 * time.Minute)
	c.Advance(9 * time.Minute)
	select {
	case <-tm.C():
		t.Fatal("fired early")
	default:
	}
	c.Advance(time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("did not fire")
	}
	stopped := c.NewTimer(time.Minute)
	stopped.Stop()
	c.Advance(time.Hour)
	select {
	case <-stopped.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if immediate := c.NewTimer(0); len(immediate.C()) != 1 {
		t.Fatal("a zero timer fires at once")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schedule/`
Expected: FAIL — `undefined: ParseIn` (package does not compile).

- [ ] **Step 3: Write the implementation**

`internal/schedule/clock.go`:

```go
// Package schedule is the vocabulary of scheduled events: when a schedule
// is due, what it is allowed to do, and how it is written in a project's
// schedules.md. It knows nothing of the agent, the tools or the UI and
// imports the standard library only.
package schedule

import (
	"sync"
	"time"
)

// Timer is the part of *time.Timer the scheduler uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock is time for the scheduler; tests use FakeClock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer {
	return realTimer{time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool           { return r.t.Stop() }

// FakeClock is a hand-driven clock: timers fire only on Advance or Set.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	ch      chan time.Time
	stopped bool
	fired   bool
}

func (f *fakeTimer) C() <-chan time.Time { return f.ch }
func (f *fakeTimer) Stop() bool {
	was := !f.stopped && !f.fired
	f.stopped = true
	return was
}

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.fireLocked()
	return t
}

// Advance moves the clock forward and fires every timer now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
}

// Set jumps the clock (either way) and fires every timer now due.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	c.fireLocked()
}

func (c *FakeClock) fireLocked() {
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.stopped {
			continue
		}
		if !t.at.After(c.now) {
			t.fired = true
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}
```

`internal/schedule/cron.go`:

```go
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
```

`internal/schedule/spec.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schedule/ -v`
Expected: PASS for every test (`TestDST*` may SKIP only on a machine with no tzdata).

- [ ] **Step 5: Commit**

```bash
git add internal/schedule/
git commit -m "schedule: parse times, next due, cron, DST, fake clock

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 2: `internal/schedule` — allowances, schedules, `schedules.md`, approvals

**Files:**
- Create: `internal/schedule/allow.go`, `internal/schedule/schedule.go`, `internal/schedule/doc.go`
- Create: `internal/schedule/testdata/schedules.golden.md`
- Test: `internal/schedule/allow_test.go`, `internal/schedule/doc_test.go`

**Interfaces:**
- Consumes: `Parse`, `Spec` (Task 1).
- Produces:
  - `type Grant struct { Kind, Value string }` (JSON `kind`, `value`), `func ParseGrant(s string) (Grant, error)`, `func (g Grant) String() string` → `"shell: go test ./..."`
  - `type Allowance []Grant`, `func ParseAllowance(lines []string) (Allowance, error)`, `func (a Allowance) Values(kind string) []string`, `func (a Allowance) WriteAllowed(rel string) bool`, `func (a Allowance) HostAllowed(host string) bool`, `func (a Allowance) Broad() bool`
  - `type State string` with `Active = "active"`, `Paused = "paused"`, `Done = "done"`
  - `type Schedule struct` (fields below), `func (s Schedule) Spec() (Spec, error)`, `func (s Schedule) Hash() string`, `func (s Schedule) NextDue() (time.Time, bool)`, `func NewID() string`, `func ValidName(n string) bool`
  - `type Request struct { Name, When, Instruction, Task string; Allow []string }`
  - `type Section struct { Raw string; Sched *Schedule; Err error }`, `type Doc struct { Preamble string; Sections []Section }`
  - `func ParseDoc(text string) Doc`, `func (d Doc) Render() string`, `func (d Doc) Schedules() []Schedule`, `func (d *Doc) Put(s Schedule)` (replace by ID or append), `func (d *Doc) Remove(id string) bool`
  - `var ErrBroken`, `func LoadFile(path string) (Doc, error)`, `func SaveFile(path string, d Doc) error`, `func RenameBroken(path string, now time.Time) (string, error)`
  - `type Approvals map[string]string`, `func LoadApprovals(path string) Approvals`, `func (a Approvals) Save(path string) error`

- [ ] **Step 1: Write the failing tests**

`internal/schedule/allow_test.go`:

```go
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
```

`internal/schedule/doc_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schedule/`
Expected: FAIL — `undefined: ParseGrant` etc.

- [ ] **Step 3: Write the implementation**

`internal/schedule/allow.go`:

```go
package schedule

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Grant is one thing a fired event may do without asking.
type Grant struct {
	Kind  string `json:"kind"`  // shell | write | browser
	Value string `json:"value"` // a shell glob, a workspace path prefix, a host glob
}

func (g Grant) String() string { return g.Kind + ": " + g.Value }

// ParseGrant reads "kind: value".
func ParseGrant(s string) (Grant, error) {
	k, v, ok := strings.Cut(s, ":")
	k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
	if !ok || v == "" {
		return Grant{}, fmt.Errorf("a grant is written kind: value, e.g. shell: go test ./... (got %q)", s)
	}
	switch k {
	case "shell":
		if strings.Trim(v, "* ") == "" {
			return Grant{}, fmt.Errorf("shell: %s would allow every command; name the command", v)
		}
	case "write":
		c := path.Clean(filepath.ToSlash(v))
		if filepath.IsAbs(v) || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") {
			return Grant{}, fmt.Errorf("write: %s is outside the workspace", v)
		}
		v = c
	case "browser":
		v = strings.ToLower(v)
		if strings.Trim(v, "*. ") == "" {
			return Grant{}, fmt.Errorf("browser: %s would allow every site; name the host", v)
		}
	default:
		return Grant{}, fmt.Errorf("unknown grant kind %q (use shell, write or browser)", k)
	}
	return Grant{Kind: k, Value: v}, nil
}

// Allowance is what a fired event may do without asking. Empty is valid:
// the event can read, and anything else asks.
type Allowance []Grant

func ParseAllowance(lines []string) (Allowance, error) {
	var a Allowance
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		g, err := ParseGrant(l)
		if err != nil {
			return nil, err
		}
		a = append(a, g)
	}
	return a, nil
}

func (a Allowance) Values(kind string) []string {
	var out []string
	for _, g := range a {
		if g.Kind == kind {
			out = append(out, g.Value)
		}
	}
	return out
}

// WriteAllowed reports whether a workspace-relative path is under a write grant.
func (a Allowance) WriteAllowed(rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	for _, p := range a.Values("write") {
		if p == "." || rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// HostAllowed reports whether a host matches a browser grant (path.Match globs).
func (a Allowance) HostAllowed(host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	for _, h := range a.Values("browser") {
		if ok, _ := path.Match(h, host); ok || h == host {
			return true
		}
	}
	return false
}

// Broad reports a write grant over the whole workspace.
func (a Allowance) Broad() bool {
	for _, p := range a.Values("write") {
		if p == "." {
			return true
		}
	}
	return false
}
```

`internal/schedule/schedule.go`:

```go
package schedule

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

type State string

const (
	Active State = "active"
	Paused State = "paused"
	Done   State = "done"
)

// Schedule is one scheduled event.
type Schedule struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	When        string        `json:"when"`
	Instruction string        `json:"instruction"`
	Task        string        `json:"task,omitempty"`
	Allow       Allowance     `json:"allow,omitempty"`
	State       State         `json:"state"`
	CreatedBy   string        `json:"created_by"` // "person" | "agent"
	Created     time.Time     `json:"created"`
	LastRun     time.Time     `json:"last_run,omitempty"`
	LastOutcome string        `json:"last_outcome,omitempty"`
	Failures    int           `json:"failures,omitempty"`
	AskTimeout  time.Duration `json:"ask_timeout,omitempty"`
	MaxRuntime  time.Duration `json:"max_runtime,omitempty"`
	// Extra holds lines of this section of schedules.md that are not ours,
	// kept verbatim on rewrite.
	Extra []string `json:"-"`
}

// Request is a schedule as asked for, before validation.
type Request struct {
	Name, When, Instruction, Task string
	Allow                         []string
}

func (s Schedule) Spec() (Spec, error) { return Parse(s.When, s.Created) }

// Hash covers exactly what a person approved: time, instruction, task and
// allowance — never the run bookkeeping.
func (s Schedule) Hash() string {
	var b strings.Builder
	b.WriteString(strings.Join(strings.Fields(s.When), " "))
	b.WriteByte(0)
	b.WriteString(s.Instruction)
	b.WriteByte(0)
	b.WriteString(s.Task)
	for _, g := range s.Allow {
		b.WriteByte(0)
		b.WriteString(g.String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// NextDue is when this schedule should next fire. A one-off is due at its
// own time until it has run; a recurring one at the first time after its
// last run (or creation) — so a missed time is due now, once.
func (s Schedule) NextDue() (time.Time, bool) {
	if s.State != Active {
		return time.Time{}, false
	}
	sp, err := s.Spec()
	if err != nil {
		return time.Time{}, false
	}
	if !sp.Recurring() {
		if !s.LastRun.IsZero() {
			return time.Time{}, false
		}
		return sp.Due()
	}
	base := s.Created
	if !s.LastRun.IsZero() {
		base = s.LastRun
	}
	return sp.Next(base)
}

func NewID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func ValidName(n string) bool { return nameRe.MatchString(n) }
```

`internal/schedule/doc.go`:

```go
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrBroken is a schedules.md that cannot be read as text at all.
var ErrBroken = errors.New("schedules.md is not readable text")

const defaultPreamble = "# Schedules\n\n" +
	"Recurring BE-Code schedules for this project. Each `##` section is one schedule; " +
	"BE-Code runs it only while a session is open in this workspace. You may edit a " +
	"schedule here, but BE-Code asks again before running one whose time, instruction, " +
	"task or allowance changed.\n"

// Section is one `## name` block: parsed, or kept as raw text with the
// reason it could not be read.
type Section struct {
	Raw   string
	Sched *Schedule
	Err   error
}

type Doc struct {
	Preamble string
	Sections []Section
}

func ParseDoc(text string) Doc {
	var d Doc
	parts := strings.Split("\n"+text, "\n## ")
	d.Preamble = strings.TrimPrefix(parts[0], "\n")
	for _, p := range parts[1:] {
		raw := "## " + strings.TrimRight(p, "\n")
		s, err := parseSection(raw)
		if err != nil {
			d.Sections = append(d.Sections, Section{Raw: raw, Err: err})
			continue
		}
		d.Sections = append(d.Sections, Section{Raw: raw, Sched: &s})
	}
	return d
}

func parseSection(raw string) (Schedule, error) {
	lines := strings.Split(raw, "\n")
	s := Schedule{Name: strings.TrimSpace(strings.TrimPrefix(lines[0], "## "))}
	var instr []string
	inInstr := false
	for _, l := range lines[1:] {
		if inInstr && strings.HasPrefix(l, "  ") {
			instr = append(instr, strings.TrimPrefix(l, "  "))
			continue
		}
		inInstr = false
		k, v, ok := strings.Cut(l, ":")
		v = strings.TrimSpace(v)
		if !ok {
			s.Extra = append(s.Extra, l)
			continue
		}
		var err error
		switch strings.TrimSpace(k) {
		case "id":
			s.ID = v
		case "when":
			s.When = v
		case "instruction":
			instr, inInstr = []string{v}, true
		case "task":
			s.Task = v
		case "allow":
			var g Grant
			if g, err = ParseGrant(v); err == nil {
				s.Allow = append(s.Allow, g)
			}
		case "state":
			s.State = State(v)
		case "created-by":
			s.CreatedBy = v
		case "created":
			s.Created, err = time.Parse(time.RFC3339, v)
		case "last-run":
			s.LastRun, err = time.Parse(time.RFC3339, v)
		case "last-outcome":
			s.LastOutcome = v
		case "failures":
			s.Failures, err = strconv.Atoi(v)
		case "ask-timeout":
			s.AskTimeout, err = time.ParseDuration(v)
		case "max-runtime":
			s.MaxRuntime, err = time.ParseDuration(v)
		default:
			s.Extra = append(s.Extra, l)
		}
		if err != nil {
			return Schedule{}, fmt.Errorf("%s: %v", strings.TrimSpace(k), err)
		}
	}
	s.Instruction = strings.Join(instr, "\n")
	switch {
	case !ValidName(s.Name):
		return Schedule{}, fmt.Errorf("name %q must be lowercase letters, digits and dashes", s.Name)
	case s.ID == "" || s.When == "" || s.Instruction == "" || s.Created.IsZero():
		return Schedule{}, fmt.Errorf("id, when, instruction and created are required")
	case s.State != Active && s.State != Paused && s.State != Done:
		return Schedule{}, fmt.Errorf("state %q is not active, paused or done", s.State)
	}
	if _, err := s.Spec(); err != nil {
		return Schedule{}, fmt.Errorf("when: %v", err)
	}
	return s, nil
}

func renderSection(s Schedule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\nid: %s\nwhen: %s\n", s.Name, s.ID, s.When)
	il := strings.Split(s.Instruction, "\n")
	fmt.Fprintf(&b, "instruction: %s\n", il[0])
	for _, l := range il[1:] {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	if s.Task != "" {
		fmt.Fprintf(&b, "task: %s\n", s.Task)
	}
	for _, g := range s.Allow {
		fmt.Fprintf(&b, "allow: %s\n", g)
	}
	fmt.Fprintf(&b, "state: %s\ncreated-by: %s\ncreated: %s\n", s.State, s.CreatedBy, s.Created.Format(time.RFC3339))
	if !s.LastRun.IsZero() {
		fmt.Fprintf(&b, "last-run: %s\n", s.LastRun.Format(time.RFC3339))
	}
	if s.LastOutcome != "" {
		fmt.Fprintf(&b, "last-outcome: %s\n", s.LastOutcome)
	}
	if s.Failures > 0 {
		fmt.Fprintf(&b, "failures: %d\n", s.Failures)
	}
	if s.AskTimeout > 0 {
		fmt.Fprintf(&b, "ask-timeout: %s\n", s.AskTimeout)
	}
	if s.MaxRuntime > 0 {
		fmt.Fprintf(&b, "max-runtime: %s\n", s.MaxRuntime)
	}
	for _, l := range s.Extra {
		b.WriteString(l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (d Doc) Render() string {
	pre := d.Preamble
	if strings.TrimSpace(pre) == "" {
		pre = defaultPreamble
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(pre, "\n") + "\n")
	for _, s := range d.Sections {
		b.WriteString("\n")
		if s.Sched != nil {
			b.WriteString(renderSection(*s.Sched))
		} else {
			b.WriteString(s.Raw)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (d Doc) Schedules() []Schedule {
	var out []Schedule
	for _, s := range d.Sections {
		if s.Sched != nil {
			out = append(out, *s.Sched)
		}
	}
	return out
}

func (d *Doc) Put(s Schedule) {
	for i := range d.Sections {
		if d.Sections[i].Sched != nil && d.Sections[i].Sched.ID == s.ID {
			c := s
			d.Sections[i].Sched = &c
			return
		}
	}
	c := s
	d.Sections = append(d.Sections, Section{Sched: &c})
}

func (d *Doc) Remove(id string) bool {
	for i := range d.Sections {
		if d.Sections[i].Sched != nil && d.Sections[i].Sched.ID == id {
			d.Sections = append(d.Sections[:i], d.Sections[i+1:]...)
			return true
		}
	}
	return false
}

// LoadFile reads schedules.md; a missing file is an empty document.
func LoadFile(p string) (Doc, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return Doc{}, nil
	}
	if err != nil {
		return Doc{}, err
	}
	if !utf8.Valid(b) {
		return Doc{}, ErrBroken
	}
	return ParseDoc(string(b)), nil
}

// SaveFile writes atomically (temp file, then rename).
func SaveFile(p string, d Doc) error {
	return writeAtomic(p, []byte(d.Render()), 0o644)
}

// RenameBroken moves an unreadable file aside and returns where it went.
func RenameBroken(p string, now time.Time) (string, error) {
	aside := filepath.Join(filepath.Dir(p), "schedules.broken-"+now.Format("20060102-150405")+".md")
	return aside, os.Rename(p, aside)
}

// Approvals maps a schedule ID to the hash of the content a person approved.
type Approvals map[string]string

func LoadApprovals(p string) Approvals {
	a := Approvals{}
	if b, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(b, &a) != nil {
			return Approvals{}
		}
	}
	return a
}

func (a Approvals) Save(p string) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(p, b, 0o600)
}

func writeAtomic(p string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
```

- [ ] **Step 4: Generate the golden file, read it, and run the tests**

Run: `UPDATE_GOLDEN=1 go test ./internal/schedule/ -run TestDocGolden && cat internal/schedule/testdata/schedules.golden.md`
Expected: the file starts with `# Schedules`, then `## nightly-tests`, `id: 3f2a9c1b`, `when: weekdays 09:00`, `instruction: pull, run the tests`, `  and summarise what broke`, `task: 3.2`, two `allow:` lines, `state: active`, `created-by: person`, `created: 2026-09-26T10:00:00-07:00`, `last-run: …`, `last-outcome: ok`, `ask-timeout: 5m0s`. Check it reads as intended before accepting it.

Run: `go test ./internal/schedule/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/schedule/
git commit -m "schedule: allowances, schedules.md round trip, approval hashes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 3: Config block and session timers

**Files:**
- Modify: `internal/config/config.go` (new `SchedulesConfig` near `CoworkConfig` ~line 52; field in `Config` near `Browser` ~line 286; default in `Default()` ~line 396)
- Modify: `internal/store/sessions.go:21-43` (`Session.Timers`)
- Modify: `README.md` (config reference: one line per key)
- Test: `internal/config/config_test.go`, `internal/store/sessions_test.go` (append)

**Interfaces:**
- Produces: `cfg.Schedules` of type `config.SchedulesConfig{Enabled bool; MinInterval string; MaxActive int; AskTimeout string; MaxRuntime string; PauseAfterFailures int}`, `func (s SchedulesConfig) Durations() (minInterval, askTimeout, maxRuntime time.Duration)`; `store.Session.Timers []schedule.Schedule`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestSchedulesDefaults(t *testing.T) {
	c := Default()
	s := c.Schedules
	if !s.Enabled || s.MinInterval != "5m" || s.MaxActive != 20 || s.AskTimeout != "10m" ||
		s.MaxRuntime != "30m" || s.PauseAfterFailures != 3 {
		t.Fatalf("defaults: %+v", s)
	}
	mi, at, mr := s.Durations()
	if mi != 5*time.Minute || at != 10*time.Minute || mr != 30*time.Minute {
		t.Fatalf("durations %v %v %v", mi, at, mr)
	}
	bad := SchedulesConfig{MinInterval: "soon", AskTimeout: "-1m", MaxRuntime: ""}
	mi, at, mr = bad.Durations()
	if mi != 5*time.Minute || at != 10*time.Minute || mr != 30*time.Minute {
		t.Fatalf("unusable values fall back to defaults: %v %v %v", mi, at, mr)
	}
}

func TestSchedulesAbsentKeyKeepsDefaults(t *testing.T) {
	c := Default()
	if err := json.Unmarshal([]byte(`{"model":"m"}`), c); err != nil {
		t.Fatal(err)
	}
	if !c.Schedules.Enabled || c.Schedules.MaxActive != 20 {
		t.Fatalf("an older config file keeps the defaults: %+v", c.Schedules)
	}
}
```

(Add `"encoding/json"` and `"time"` to the test file's imports if not present.)

Append to `internal/store/sessions_test.go`:

```go
func TestSessionTimersRoundTrip(t *testing.T) {
	s := Session{ID: "x", Timers: []schedule.Schedule{{ID: "a", Name: "check", When: "in 20m",
		Instruction: "look", State: schedule.Active, CreatedBy: "agent",
		Created: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"timers":[`) {
		t.Fatalf("json: %s", b)
	}
	var back Session
	if err := json.Unmarshal(b, &back); err != nil || len(back.Timers) != 1 || back.Timers[0].Name != "check" {
		t.Fatalf("back: %+v %v", back.Timers, err)
	}
	empty, _ := json.Marshal(Session{ID: "y"})
	if strings.Contains(string(empty), "timers") {
		t.Fatal("omitted when empty")
	}
}
```

(Imports: `encoding/json`, `strings`, `time`, `github.com/brown-enterprises/be-code/internal/schedule`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ ./internal/store/`
Expected: FAIL — `c.Schedules undefined`, `unknown field Timers`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, beside `CoworkConfig`:

```go
// SchedulesConfig tunes scheduled events (schedules spec §4.4). Durations
// are Go duration strings; an unusable one falls back to its default.
type SchedulesConfig struct {
	Enabled            bool   `json:"enabled"`
	MinInterval        string `json:"min_interval"`
	MaxActive          int    `json:"max_active"`
	AskTimeout         string `json:"ask_timeout"`
	MaxRuntime         string `json:"max_runtime"`
	PauseAfterFailures int    `json:"pause_after_failures"`
}

// Durations parses the three duration keys, each falling back to its default.
func (s SchedulesConfig) Durations() (minInterval, askTimeout, maxRuntime time.Duration) {
	parse := func(v string, def time.Duration) time.Duration {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		return def
	}
	return parse(s.MinInterval, 5*time.Minute), parse(s.AskTimeout, 10*time.Minute), parse(s.MaxRuntime, 30*time.Minute)
}
```

In `Config`, after `Browser`:

```go
	// Schedules tunes scheduled events: timed requests a running session
	// queues for the model under an allowance a person approved.
	Schedules SchedulesConfig `json:"schedules"`
```

In `Default()`, beside `Browser:`:

```go
		Schedules: SchedulesConfig{Enabled: true, MinInterval: "5m", MaxActive: 20,
			AskTimeout: "10m", MaxRuntime: "30m", PauseAfterFailures: 3},
```

(Add `"time"` to config.go's imports if absent.)

In `internal/store/sessions.go`, after `Chat`:

```go
	// Timers are the session's one-off scheduled events (schedules spec
	// §1.2); recurring schedules live in the project's schedules.md.
	Timers []schedule.Schedule `json:"timers,omitempty"`
```

and import `github.com/brown-enterprises/be-code/internal/schedule`.

In `README.md`'s config reference (find the `browser` keys with `grep -n "browser\." README.md`), add after the browser block:

```markdown
- `schedules.enabled` (true) — scheduled events: `/schedule` and the model's `schedule` tool.
- `schedules.min_interval` ("5m") — a recurring schedule may not run more often than this.
- `schedules.max_active` (20) — active schedules and timers across the project and the session.
- `schedules.ask_timeout` ("10m") — how long a fired event's prompt waits for a person before it is withdrawn and refused.
- `schedules.max_runtime` ("30m") — a fired event's turn is cancelled after this long.
- `schedules.pause_after_failures` (3) — a recurring schedule that fails this many runs in a row pauses itself.
```

(Match the surrounding bullet style exactly if the README uses a table instead.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/store/ README.md
git commit -m "config: schedules block; store: session timers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 4: The allowance in the tools

**Files:**
- Create: `internal/tools/allowance.go`
- Modify: `internal/tools/tool.go:64-80` (Registry field `fired`)
- Modify: `internal/tools/shell.go:68-88` (main path only; the scoped path at 44-66 is unchanged)
- Modify: `internal/tools/process.go:174-205` (rename `_ context.Context` to `ctx`)
- Modify: `internal/tools/fs.go:29-73` (`approveWrite`)
- Modify: `internal/tools/browser.go:270-335` (`gate` default tier; `approve` takes `ctx`)
- Test: `internal/tools/allowance_test.go`, and one test in `internal/tools/browser_test.go`

**Interfaces:**
- Consumes: `schedule.Allowance` (Task 2).
- Produces: `func (r *Registry) SetAllowance(al schedule.Allowance, askTimeout time.Duration)`, `func (r *Registry) ClearAllowance() (refused string, timedOut bool)`, unexported `r.ask(ctx, action, detail string, nilApproves bool) bool`, `r.allowShell(cmd) bool`, `r.allowWrite(rel) bool`, `r.allowHost(host) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/tools/allowance_test.go`:

```go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

type askLog struct {
	mu      sync.Mutex
	actions []string
	answer  bool
}

func (l *askLog) approve(action, detail string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.actions = append(l.actions, action)
	return l.answer
}

func (l *askLog) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.actions...)
}

func allowReg(t *testing.T, answer bool) (*Registry, *askLog) {
	t.Helper()
	log := &askLog{answer: answer}
	r, err := NewRegistry(t.TempDir(), log.approve)
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveWrites = true
	return r, log
}

func call(r *Registry, name, args string) Result {
	return r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: name, Arguments: args})
}

func grants(t *testing.T, g ...string) schedule.Allowance {
	a, err := schedule.ParseAllowance(g)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAllowanceShellSkipsPrompt(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	res := call(r, "shell", `{"command":"echo hi"}`)
	if res.IsError || !strings.Contains(res.Content, "hi") || len(log.asked()) != 0 {
		t.Fatalf("covered command ran unasked: %+v asked=%v", res, log.asked())
	}
	res = call(r, "shell", `{"command":"ls"}`)
	if !res.IsError || strings.Join(log.asked(), ",") != "shell" {
		t.Fatalf("uncovered command asks: %+v asked=%v", res, log.asked())
	}
	refused, timedOut := r.ClearAllowance()
	if refused != "shell" || timedOut {
		t.Fatalf("first refusal recorded: %q %v", refused, timedOut)
	}
	// Cleared: the grant no longer applies.
	call(r, "shell", `{"command":"echo hi"}`)
	if len(log.asked()) != 2 {
		t.Fatalf("after clear it asks again: %v", log.asked())
	}
}

func TestAllowanceCompoundStillAsks(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	call(r, "shell", `{"command":"echo hi && rm -rf x"}`)
	if len(log.asked()) != 1 {
		t.Fatal("a compound command is never covered by a glob")
	}
}

func TestAllowanceDenyWins(t *testing.T) {
	r, log := allowReg(t, true)
	r.ShellDeny = []string{"rm *"}
	r.SetAllowance(grants(t, "shell: rm *"), time.Minute)
	res := call(r, "shell", `{"command":"rm x"}`)
	if !res.IsError || len(log.asked()) != 0 {
		t.Fatalf("deny list refuses outright: %+v %v", res, log.asked())
	}
}

func TestAllowanceUntrustedWebStillAsks(t *testing.T) {
	r, log := allowReg(t, true)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	r.MarkUntrustedWeb()
	call(r, "shell", `{"command":"echo hi"}`)
	if strings.Join(log.asked(), ",") != "shell_after_web" {
		t.Fatalf("asked %v", log.asked())
	}
}

func TestAllowanceWrites(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "write: docs"), time.Minute)
	res := call(r, "write_file", `{"path":"docs/a.md","content":"x"}`)
	if res.IsError || len(log.asked()) != 0 {
		t.Fatalf("covered write: %+v %v", res, log.asked())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "docs", "a.md")); err != nil {
		t.Fatal(err)
	}
	res = call(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if !res.IsError || strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("uncovered write asks: %+v %v", res, log.asked())
	}
}

func TestAllowanceNotInherited(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *", "write: ."), time.Minute)
	sub := r.Subset("shell", "write_file")
	sub.ApproveWrites = true
	call(sub, "shell", `{"command":"echo hi"}`)
	scoped := r.Scoped([]string{"docs"}, []string{"go vet ./..."}, "s1")
	call(scoped, "write_file", `{"path":"docs/a.md","content":"x"}`)
	if got := strings.Join(log.asked(), ","); got != "shell,file_write" {
		t.Fatalf("Subset and Scoped must ask: %q", got)
	}
}

func TestAllowanceAskTimeoutWithdraws(t *testing.T) {
	r, _ := allowReg(t, true)
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		<-ctx.Done()
		return false
	}
	r.SetAllowance(nil, 20*time.Millisecond)
	start := time.Now()
	res := call(r, "shell", `{"command":"ls"}`)
	if !res.IsError || time.Since(start) > 2*time.Second {
		t.Fatalf("refused after the deadline: %+v", res)
	}
	if refused, timedOut := r.ClearAllowance(); refused != "shell" || !timedOut {
		t.Fatalf("%q %v", refused, timedOut)
	}
}

func TestNoAllowanceBehaviourUnchanged(t *testing.T) {
	r, log := allowReg(t, true)
	call(r, "shell", `{"command":"echo hi"}`)
	if strings.Join(log.asked(), ",") != "shell" {
		t.Fatalf("asked %v", log.asked())
	}
	if refused, _ := r.ClearAllowance(); refused != "" {
		t.Fatal("nothing to clear")
	}
}
```

For the browser, open `internal/tools/browser_test.go`, find the test asserting that a click on a host in the default tier asks action `browser` (`grep -n '"browser"' internal/tools/browser_test.go`), and add a sibling test `TestBrowserAllowanceGrantsHost` built the same way, which calls `reg.SetAllowance(grants(t, "browser: <that test's host>"), time.Minute)` before the click and asserts that no `browser` approval was requested and the click succeeded. Add a second assertion in the same test: with `browser.sites` setting that host to `watch`, the click still asks `browser_watch` despite the grant.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tools/ -run 'Allowance|BrowserAllowance'`
Expected: FAIL — `r.SetAllowance undefined`.

- [ ] **Step 3: Implement**

`internal/tools/allowance.go`:

```go
package tools

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
)

// firedPolicy is a scheduled event's allowance for the turn it runs in
// (schedules spec §2.3). It lives on the primary registry only: Subset and
// Scoped build fresh registries, and a scoped registry's Approve is a
// closure over the parent's seam, so neither ever sees it. That is why the
// allowance is consulted inside the tools rather than by wrapping Approve.
type firedPolicy struct {
	allow      schedule.Allowance
	askTimeout time.Duration

	mu       sync.Mutex
	refused  string
	timedOut bool
}

// SetAllowance starts a fired turn's allowance; askTimeout bounds every
// prompt raised during it (0: no bound).
func (r *Registry) SetAllowance(al schedule.Allowance, askTimeout time.Duration) {
	r.fired.Store(&firedPolicy{allow: al, askTimeout: askTimeout})
}

// ClearAllowance ends it and reports the first action refused during the
// turn, and whether that refusal was a prompt nobody answered in time.
func (r *Registry) ClearAllowance() (refused string, timedOut bool) {
	p := r.fired.Swap(nil)
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused, p.timedOut
}

func (r *Registry) allowShell(command string) bool {
	p := r.fired.Load()
	if p == nil {
		return false
	}
	globs := p.allow.Values("shell")
	return len(globs) > 0 && classifyCommand(command, globs, nil) == cmdAllowed
}

func (r *Registry) allowWrite(rel string) bool {
	p := r.fired.Load()
	return p != nil && p.allow.WriteAllowed(rel)
}

func (r *Registry) allowHost(host string) bool {
	p := r.fired.Load()
	return p != nil && p.allow.HostAllowed(host)
}

// ask raises a prompt through the seam. nilApproves is what a registry with
// no approver means at the call site: shell and file writes have always run
// unasked without one, the browser and shell_after_web refuse. During a
// fired turn the prompt goes through ApproveCtx under the allowance's
// deadline, so a question nobody is there to answer is withdrawn and
// refused rather than holding the event forever (spec §2.4).
func (r *Registry) ask(ctx context.Context, action, detail string, nilApproves bool) bool {
	p := r.fired.Load()
	if p == nil {
		if r.Approve == nil {
			return nilApproves
		}
		return r.Approve(action, detail)
	}
	var ok, timedOut bool
	switch {
	case p.askTimeout > 0 && r.ApproveCtx != nil:
		actx, cancel := context.WithTimeout(ctx, p.askTimeout)
		ok = r.ApproveCtx(actx, action, detail)
		timedOut = !ok && errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
	case r.Approve != nil:
		ok = r.Approve(action, detail)
	default:
		ok = nilApproves
	}
	if !ok {
		p.mu.Lock()
		if p.refused == "" {
			p.refused, p.timedOut = action, timedOut
		}
		p.mu.Unlock()
	}
	return ok
}
```

In `internal/tools/tool.go`, in the `Registry` struct (after `ApproveCtx`):

```go
	// fired is a scheduled event's allowance while its turn runs
	// (allowance.go). Never copied by Subset or Scoped.
	fired atomic.Pointer[firedPolicy]
```

In `internal/tools/shell.go`, replace the main-path switch (lines ~69-88) with:

```go
	switch {
	case class == cmdDenied:
		return Result{IsError: true, Content: "this command matches the deny list and will never run; use a safer alternative"}
	case t.r.UntrustedWeb():
		// This request has read a page from a host the user has not
		// allowed: every automatic approval — the allow list, -y, "a",
		// auto_approve_shell, a scheduled event's allowance — is suspended,
		// and the command asks as its own action, which offers no "always"
		// (spec §3.6, amended). With nobody to ask, it is refused.
		if !t.r.ask(ctx, "shell_after_web", command, false) {
			return Result{IsError: true, Content: "shell is not auto-approved after reading an untrusted web page in this request, and this command was not approved; say what you wanted to run and why"}
		}
	case class == cmdAllowed:
		// pre-approved by allowlist; no prompt
	case t.r.allowShell(command):
		// covered by the scheduled event's allowance (schedules spec §2.3)
	default:
		if !t.r.ask(ctx, "shell", command, true) {
			return Result{IsError: true, Content: "user denied this command; propose an alternative or ask what to do"}
		}
	}
```

In `internal/tools/process.go`, change `func (t *processTool) Run(_ context.Context, …` to `Run(ctx context.Context, …` and replace its switch the same way:

```go
		switch {
		case class == cmdDenied:
			return Result{IsError: true, Content: "this command matches the deny list"}
		case t.r.UntrustedWeb():
			if !t.r.ask(ctx, "shell_after_web", command+"  (background)", false) {
				return Result{IsError: true, Content: "shell is not auto-approved after reading an untrusted web page in this request, and this command was not approved; say what you wanted to run and why"}
			}
		case class == cmdAllowed:
			// pre-approved by allowlist; no prompt
		case t.r.allowShell(command):
			// covered by the scheduled event's allowance (schedules spec §2.3)
		default:
			if !t.r.ask(ctx, "shell", command+"  (background)", true) {
				return Result{IsError: true, Content: "user denied this background command"}
			}
		}
```

In `internal/tools/fs.go` `approveWrite`, directly after the `rejected := …` assignment and before `if r.ReviewWrite != nil {`:

```go
	if r.allowWrite(rel) {
		// Covered by the scheduled event's allowance: no editor diff, no
		// prompt. The checkpoint snapshot (OnBeforeWrite) still runs.
		return Result{}, true
	}
```

and replace `if r.Approve("file_write", preview) {` with `if r.ask(ctx, "file_write", preview, true) {`.

In `internal/tools/browser.go`: change `func (t *BrowserTool) approve(action, detail string) bool` to

```go
func (t *BrowserTool) approve(ctx context.Context, action, detail string) bool {
	return t.r != nil && t.r.ask(ctx, action, detail, false)
}
```

update its three call sites in `gate` to pass `ctx`, and in `gate`'s `default:` branch insert after the `Granted` check:

```go
		if t.r != nil && t.r.allowHost(host) {
			// Covered by the scheduled event's allowance (schedules spec §2.3);
			// never the watch tier or a page with no address, which ask above.
			return "", host, judgedURL
		}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tools/ -count=1`
Expected: PASS (all existing tools tests too).

- [ ] **Step 5: Commit**

```bash
git add internal/tools/
git commit -m "tools: scheduled-event allowance checked where a tool would prompt

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 5: The scheduler, queued events and fired turns

**Files:**
- Modify: `internal/agent/inbox.go` (InboxItem fields; `DrainItems`; `DrainForTurn`; `EnqueueScheduled`; `scheduledQueued`)
- Create: `internal/agent/schedules.go`
- Modify: `internal/agent/loop.go` (Agent fields; `Events.OnScheduleFire`; split `RunFull` at line ~2014)
- Modify: `internal/agent/handoff.go:211` (`Resume` reloads timers)
- Test: `internal/agent/schedules_test.go`

**Interfaces:**
- Consumes: `schedule.*` (Tasks 1–2), `cfg.Schedules` (Task 3), `Registry.SetAllowance/ClearAllowance` (Task 4).
- Produces:
  - `InboxItem.Scheduled string` (schedule ID) and `InboxItem.ScheduleName string`
  - `func (a *Agent) DrainForTurn() []InboxItem`, `func (a *Agent) EnqueueScheduled(id, name, text string)`
  - `func (a *Agent) EnableSchedules(clock schedule.Clock)` — creates the scheduler (does not start it)
  - `func (a *Agent) StopSchedules()`
  - `Events.OnScheduleFire func(name string)`
  - unexported, used by Task 6: `a.sched *scheduler`; `s.reloadLocked()`, `s.allLocked() []schedule.Schedule`, `s.findLocked(key string) (schedule.Schedule, bool, bool)` (by ID or name; returns schedule, found, isProject), `s.putLocked(sc schedule.Schedule, project bool) error`, `s.removeLocked(id string) bool`, `s.approveLocked(sc)`, `s.startLoop()`, `s.kickLoop()`, `a.fireNow(sc schedule.Schedule)`, `fireText(sc) string`

- [ ] **Step 1: Write the failing tests**

`internal/agent/schedules_test.go`:

```go
package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
)

// withEngine (engine_test.go) attaches a working-memory store under t.TempDir().

var t0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)

// schedAgent is a test agent with a scheduler on a fake clock, its approval
// store under the test's own temp dir.
func schedAgent(t *testing.T, p provider.Provider) (*Agent, *schedule.FakeClock, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // the approvals file goes under ~/.be-code
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.Schedules.PauseAfterFailures = 2 })
	ag.Session = &store.Session{ID: "s1"}
	clock := schedule.NewFakeClock(t0)
	ag.EnableSchedules(clock)
	t.Cleanup(ag.StopSchedules)
	return ag, clock, dir
}

// seed puts an approved schedule straight into a store.
func seed(t *testing.T, ag *Agent, sc schedule.Schedule, project bool) {
	t.Helper()
	s := ag.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	if err := s.putLocked(sc, project); err != nil {
		t.Fatal(err)
	}
	s.approveLocked(sc)
}

func mk(name, when string) schedule.Schedule {
	return schedule.Schedule{ID: schedule.NewID(), Name: name, When: when, Instruction: "do " + name,
		State: schedule.Active, CreatedBy: "person", Created: t0}
}

// waitQueued polls (the loop runs on its own goroutine) for n scheduled items.
func waitQueued(t *testing.T, ag *Agent, n int) []InboxItem {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got []InboxItem
		for _, it := range ag.PeekItems() {
			if it.Scheduled != "" {
				got = append(got, it)
			}
		}
		if len(got) == n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d scheduled items, have %d", n, len(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFiresInDueOrderAndOnlyOnce(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("later", "in 20m"), false)
	seed(t, ag, mk("sooner", "in 10m"), false)
	ag.sched.startLoop()
	clock.Advance(25 * time.Minute)
	items := waitQueued(t, ag, 2)
	if items[0].ScheduleName != "sooner" || items[1].ScheduleName != "later" {
		t.Fatalf("order %s, %s", items[0].ScheduleName, items[1].ScheduleName)
	}
	if !items[0].Harness {
		t.Fatal("a scheduled item is not typed by a person")
	}
	if !strings.HasPrefix(items[0].Text, `[Scheduled event "sooner" — in 10m, set by you 2026-09-26]`) {
		t.Fatalf("header: %q", items[0].Text)
	}
	clock.Advance(time.Minute)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	waitQueued(t, ag, 2) // not queued a second time
}

func TestScheduledItemsNeverDeliveredMidRun(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "x", "scheduled text")
	ag.Enqueue("typed text")
	if got := ag.DrainInbox(); len(got) != 1 || got[0] != "typed text" {
		t.Fatalf("mid-run delivery takes only unscheduled lines: %v", got)
	}
	if ag.Pending() != 1 {
		t.Fatal("the scheduled item stays queued")
	}
}

func TestDrainForTurnPersonFirstThenOneScheduled(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "a", "sched a")
	ag.EnqueueScheduled("id2", "b", "sched b")
	ag.Enqueue("typed")
	first := ag.DrainForTurn()
	if len(first) != 1 || first[0].Text != "typed" {
		t.Fatalf("person's line first, alone: %+v", first)
	}
	second := ag.DrainForTurn()
	if len(second) != 1 || second[0].Scheduled != "id1" {
		t.Fatalf("then one scheduled event: %+v", second)
	}
	if ag.Pending() != 1 {
		t.Fatal("the other waits for its own turn")
	}
}

func TestFiredTurnRunsUnderAllowanceAndRecordsOutcome(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"docs/r.md","content":"x"}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "2", Name: "write_file", Arguments: `{"path":"src/y.go","content":"x"}`}}},
		{Content: "done"},
	}}
	ag, clock, dir := schedAgent(t, p)
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action); return false }
	ag.Tools.ApproveWrites = true
	sc := mk("report", "in 1m")
	sc.Allow = schedule.Allowance{{Kind: "write", Value: "docs"}}
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	items := ag.DrainForTurn()
	if _, _, err := ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "r.md")); err != nil {
		t.Fatal("covered write landed without a prompt")
	}
	if strings.Join(asked, ",") != "file_write" {
		t.Fatalf("only the uncovered write asked: %v", asked)
	}
	if refused, _ := ag.Tools.ClearAllowance(); refused != "" {
		t.Fatal("the allowance is cleared when the turn ends")
	}
	got, _, _ := ag.sched.lookup(sc.ID)
	if got.LastOutcome != "refused: file_write" || got.State != schedule.Done || got.LastRun.IsZero() {
		t.Fatalf("outcome: %+v", got)
	}
	if len(ag.Session.Timers) != 1 || ag.Session.Timers[0].State != schedule.Done {
		t.Fatalf("timers mirrored into the session: %+v", ag.Session.Timers)
	}
}

func TestOrdinaryRequestGetsNoAllowance(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "x", "scheduled")
	ag.DrainForTurn() // arms "scheduled"
	// A different request (a person typed something else) must not inherit it.
	if f := ag.takeFiring("something else"); f != nil {
		t.Fatal("the arm matches only its own text")
	}
	if f := ag.takeFiring("scheduled"); f != nil {
		t.Fatal("and is spent by the first take")
	}
}

func TestMissedOneOffRunsOnce(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("missed", "in 5m"), false)
	clock.Set(t0.Add(3 * time.Hour)) // the session was closed past its time
	ag.sched.startLoop()
	waitQueued(t, ag, 1)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	clock.Advance(time.Hour)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("a one-off runs once")
	}
}

func TestMissedRecurringRunsOnceNotPerMissedTime(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("half-hourly", "every 30m"), true)
	clock.Set(t0.Add(5 * time.Hour)) // ten missed times
	ag.sched.startLoop()
	waitQueued(t, ag, 1)
	time.Sleep(50 * time.Millisecond)
	waitQueued(t, ag, 1)
}

func TestDroppedQueuedEventFiresAgain(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("tick", "every 30m"), true)
	ag.sched.startLoop()
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
	ag.Remove(0) // a person dropped it from the queue popup
	clock.Advance(30 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestHandEditedSchedulePausesAtFire(t *testing.T) {
	ag, clock, dir := schedAgent(t, &scriptedProvider{})
	sc := mk("nightly", "every 30m")
	seed(t, ag, sc, true)
	p := filepath.Join(dir, ".be-code", "schedules.md")
	b, _ := os.ReadFile(p)
	edited := strings.Replace(string(b), "state: active", "allow: write: .\nstate: active", 1)
	os.WriteFile(p, []byte(edited), 0o644)
	var notes []string
	ag.Events.OnNotice = func(m string) { notes = append(notes, m) }
	ag.sched.startLoop()
	clock.Advance(31 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("an edited schedule must not fire")
	}
	got, _, _ := ag.sched.lookup(sc.ID)
	if got.State != schedule.Paused || !strings.Contains(strings.Join(notes, "\n"), "changed since approved") {
		t.Fatalf("paused with a notice: %+v %v", got.State, notes)
	}
}

func TestPauseAfterFailures(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("flaky", "every 30m")
	seed(t, ag, sc, true)
	ag.sched.finish(sc.ID, "checks failed")
	if got, _, _ := ag.sched.lookup(sc.ID); got.State != schedule.Active || got.Failures != 1 {
		t.Fatalf("one failure: %+v", got)
	}
	ag.sched.finish(sc.ID, "checks failed")
	if got, _, _ := ag.sched.lookup(sc.ID); got.State != schedule.Paused {
		t.Fatalf("paused after 2 (config): %+v", got)
	}
}

func TestOkResetsFailures(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("flaky", "every 30m")
	seed(t, ag, sc, true)
	ag.sched.finish(sc.ID, "checks failed")
	ag.sched.finish(sc.ID, "ok")
	if got, _, _ := ag.sched.lookup(sc.ID); got.Failures != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestTaskLinkSetsDoing(t *testing.T) {
	var st *engine.Store
	var root, doingAtCall string
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		doingAtCall = st.DoingUnderID(root)
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, clock, _ := schedAgent(t, p)
	st = withEngine(t, ag)
	root = st.Plan("nightly work", []string{"step a", "step b"})
	sc := mk("linked", "in 1m")
	sc.Task = root + ".2"
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if doingAtCall != root+".2" {
		t.Fatalf("the linked step was doing when the model was called: %q", doingAtCall)
	}
}

func TestMissingTaskLinkStillRuns(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	withEngine(t, ag)
	var notes []string
	ag.Events.OnNotice = func(m string) { notes = append(notes, m) }
	sc := mk("orphan", "in 1m")
	sc.Task = "9.9"
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "running without it") {
		t.Fatalf("notice: %v", notes)
	}
	if got, _, _ := ag.sched.lookup(sc.ID); got.LastOutcome != "ok" {
		t.Fatalf("outcome %q", got.LastOutcome)
	}
}

func TestMaxRuntimeCancelsTheTurn(t *testing.T) {
	block := make(chan struct{})
	p := &funcProvider{fn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
			return &provider.ChatResponse{Content: "x"}, nil
		}
	}}
	ag, clock, _ := schedAgent(t, p)
	sc := mk("slow", "in 1m")
	sc.MaxRuntime = 30 * time.Millisecond
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	_, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if err == nil || !strings.Contains(err.Error(), "max runtime") {
		t.Fatalf("err %v", err)
	}
	if got, _, _ := ag.sched.lookup(sc.ID); got.LastOutcome != "timed out" {
		t.Fatalf("outcome %q", got.LastOutcome)
	}
}
```

`funcProvider.fn` takes no context (`func(req provider.ChatRequest) (*provider.ChatResponse, error)`), so `TestMaxRuntimeCancelsTheTurn` needs a provider of its own that sees the context; add this beside the test:

```go
type ctxBlockProvider struct{}

func (ctxBlockProvider) Name() string { return "block" }
func (ctxBlockProvider) Chat(ctx context.Context, _ provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (ctxBlockProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (ctxBlockProvider) Ping(context.Context) (string, error)                     { return "ok", nil }
```

and use `ctxBlockProvider{}` in place of the `funcProvider` literal (drop the `block` channel). (`Agent.Remove(i int) (string, bool)` is the queue popup's remove-by-index.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'Fires|Scheduled|DrainForTurn|FiredTurn|Ordinary|Missed|Dropped|HandEdited|Pause|OkResets|MaxRuntime|TaskLink'`
Expected: FAIL — `ag.EnableSchedules undefined`.

- [ ] **Step 3: Implement the inbox changes**

In `internal/agent/inbox.go`, add to `InboxItem`:

```go
	// Scheduled is the ID of the schedule that queued this line (schedules
	// spec §2.1); empty for everything else. Such a line is never delivered
	// into a running turn and always runs as a turn of its own.
	Scheduled    string
	ScheduleName string
```

Replace `DrainItems` and add the new functions:

```go
// DrainItems removes and returns every queued message except scheduled
// events, which wait for a turn of their own (DrainForTurn).
func (a *Agent) DrainItems() []InboxItem {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	var out, keep []InboxItem
	for _, it := range a.inbox.items {
		if it.Scheduled != "" {
			keep = append(keep, it)
		} else {
			out = append(out, it)
		}
	}
	a.inbox.items = keep
	return out
}

// DrainForTurn is the leftover-queue rule's drain: every line that is not a
// scheduled event, as before — or, when there are none, exactly one
// scheduled event, armed so that the RunFull it starts runs under that
// schedule's allowance. A person's words and a scheduled event never share
// a turn, so exactly one set of rules governs each (schedules spec §2.1).
func (a *Agent) DrainForTurn() []InboxItem {
	if out := a.DrainItems(); len(out) > 0 {
		return out
	}
	a.inbox.mu.Lock()
	if len(a.inbox.items) == 0 {
		a.inbox.mu.Unlock()
		return nil
	}
	it := a.inbox.items[0]
	a.inbox.items = a.inbox.items[1:]
	a.inbox.mu.Unlock()
	a.fireMu.Lock()
	a.armed = &firing{id: it.Scheduled, text: it.Text}
	a.fireMu.Unlock()
	return []InboxItem{it}
}

// EnqueueScheduled queues a due event (harness-written: never a person's).
// Exported for the UIs' tests; production code reaches it through fireNow.
func (a *Agent) EnqueueScheduled(id, name, text string) {
	a.inbox.mu.Lock()
	a.inbox.items = append(a.inbox.items, InboxItem{Text: text, Harness: true, Scheduled: id, ScheduleName: name})
	a.inbox.mu.Unlock()
}

// scheduledQueued is the set of schedule IDs with an event still waiting.
func (a *Agent) scheduledQueued() map[string]bool {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	m := map[string]bool{}
	for _, it := range a.inbox.items {
		if it.Scheduled != "" {
			m[it.Scheduled] = true
		}
	}
	return m
}

type firing struct{ id, text string }

// takeFiring spends the arm DrainForTurn set, returning it only when this
// request is the text it armed. A stale arm never leaks into a later request.
func (a *Agent) takeFiring(input string) *firing {
	a.fireMu.Lock()
	defer a.fireMu.Unlock()
	f := a.armed
	a.armed = nil
	if f == nil || strings.TrimSpace(f.text) != strings.TrimSpace(input) {
		return nil
	}
	return f
}
```

- [ ] **Step 4: Implement the scheduler**

In `internal/agent/loop.go`, add to the `Agent` struct:

```go
	// sched runs scheduled events (schedules.go); nil when disabled or headless.
	sched  *scheduler
	fireMu sync.Mutex
	armed  *firing
```

and to `Events`:

```go
	// OnScheduleFire fires after a due scheduled event was queued, so an
	// idle UI can start its turn (the same wake a sub-agent's hand-back uses).
	OnScheduleFire func(name string)
```

Rename the existing `func (a *Agent) RunFull(` to `func (a *Agent) runFull(` (body unchanged) and add above it:

```go
func (a *Agent) RunFull(ctx context.Context, userInput string) (string, *ReviewedReport, error) {
	if f := a.takeFiring(userInput); f != nil && a.sched != nil {
		return a.runFired(ctx, f, userInput)
	}
	return a.runFull(ctx, userInput)
}
```

Check every other caller of `RunFull` inside `internal/agent` (`grep -n "RunFull(" internal/agent/*.go`): calls from inside the package that are repair or plan paths stay on `RunFull` (they are requests), none should call `runFull` directly except `runFired`.

`internal/agent/schedules.go`:

```go
package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
)

// scheduler owns a session's scheduled events (schedules spec §1.3): the
// project's recurring schedules (schedules.md, which always wins and is
// reread on every wake) and the session's one-off timers. One goroutine,
// one timer armed for the nearest due time; a due event is queued, never run
// here. It is started by a UI (StartSchedules[Async]) and never by
// buildAgent, so nothing fires before approvals and events are wired.
type scheduler struct {
	a             *Agent
	clock         schedule.Clock
	projectPath   string
	approvalsPath string

	mu        sync.Mutex
	doc       schedule.Doc
	timers    []schedule.Schedule
	approvals schedule.Approvals
	warned    map[string]bool

	startOnce sync.Once
	stopOnce  sync.Once
	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	started   bool
}

// EnableSchedules creates the scheduler. cmd calls it for interactive
// sessions when schedules.enabled; it does not start it.
func (a *Agent) EnableSchedules(clock schedule.Clock) {
	base, err := config.Dir()
	if err != nil {
		a.notice("schedules disabled: %v", err)
		return
	}
	s := &scheduler{a: a, clock: clock,
		projectPath:   filepath.Join(a.Tools.Root, ".be-code", "schedules.md"),
		approvalsPath: filepath.Join(base, "engine", engine.Key(a.Tools.Root), "schedules.json"),
		warned:        map[string]bool{},
		kick:          make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s.approvals = schedule.LoadApprovals(s.approvalsPath)
	if a.Session != nil {
		s.timers = append([]schedule.Schedule(nil), a.Session.Timers...)
	}
	a.sched = s
}

// loadTimers replaces the one-off timers after a resume.
func (s *scheduler) loadTimers(ts []schedule.Schedule) {
	s.mu.Lock()
	s.timers = append([]schedule.Schedule(nil), ts...)
	s.mu.Unlock()
	s.kickLoop()
}

// reloadLocked rereads schedules.md: the file wins over what we hold.
func (s *scheduler) reloadLocked() {
	d, err := schedule.LoadFile(s.projectPath)
	if errors.Is(err, schedule.ErrBroken) {
		aside, rerr := schedule.RenameBroken(s.projectPath, s.clock.Now())
		if rerr == nil {
			s.a.notice("schedules.md could not be read; moved aside to %s", aside)
		}
		d = schedule.Doc{}
	} else if err != nil {
		s.a.notice("schedules.md: %v", err)
		return
	}
	for _, sec := range d.Sections {
		if sec.Err != nil && !s.warned[sec.Raw] {
			s.warned[sec.Raw] = true
			s.a.notice("schedules.md: skipped %s: %v", firstLine(sec.Raw, 60), sec.Err)
		}
	}
	s.doc = d
}

func (s *scheduler) allLocked() []schedule.Schedule {
	return append(s.doc.Schedules(), s.timers...)
}

// findLocked finds by ID or name.
func (s *scheduler) findLocked(key string) (schedule.Schedule, bool, bool) {
	for _, sc := range s.doc.Schedules() {
		if sc.ID == key || sc.Name == key {
			return sc, true, true
		}
	}
	for _, sc := range s.timers {
		if sc.ID == key || sc.Name == key {
			return sc, true, false
		}
	}
	return schedule.Schedule{}, false, false
}

// lookup is findLocked after a reload, for tests and callers outside mu.
func (s *scheduler) lookup(key string) (schedule.Schedule, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.findLocked(key)
}

// putLocked stores sc in its store and persists that store.
func (s *scheduler) putLocked(sc schedule.Schedule, project bool) error {
	if project {
		s.doc.Put(sc)
		if err := schedule.SaveFile(s.projectPath, s.doc); err != nil {
			s.a.notice("could not save schedules.md: %v", err)
			return err
		}
		return nil
	}
	replaced := false
	for i := range s.timers {
		if s.timers[i].ID == sc.ID {
			s.timers[i], replaced = sc, true
		}
	}
	if !replaced {
		s.timers = append(s.timers, sc)
	}
	s.saveTimersLocked()
	return nil
}

func (s *scheduler) removeLocked(id string) bool {
	if s.doc.Remove(id) {
		if err := schedule.SaveFile(s.projectPath, s.doc); err != nil {
			s.a.notice("could not save schedules.md: %v", err)
		}
		return true
	}
	for i := range s.timers {
		if s.timers[i].ID == id {
			s.timers = append(s.timers[:i], s.timers[i+1:]...)
			s.saveTimersLocked()
			return true
		}
	}
	return false
}

func (s *scheduler) saveTimersLocked() {
	ts := append([]schedule.Schedule(nil), s.timers...)
	s.a.UpdateSession(func(ss *store.Session) { ss.Timers = ts })
}

func (s *scheduler) approveLocked(sc schedule.Schedule) {
	s.approvals[sc.ID] = sc.Hash()
	if err := s.approvals.Save(s.approvalsPath); err != nil {
		s.a.notice("could not save schedule approvals: %v", err)
	}
}

func (s *scheduler) approvedLocked(sc schedule.Schedule) bool {
	return s.approvals[sc.ID] == sc.Hash()
}

func (s *scheduler) kickLoop() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *scheduler) startLoop() {
	s.startOnce.Do(func() {
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
		go s.loop()
	})
}

// StopSchedules stops the loop; a no-op when never enabled or started.
func (a *Agent) StopSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *scheduler) loop() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.a.notice("schedules stopped: %v", r)
		}
	}()
	for {
		next, ok := s.queueDue()
		var timer schedule.Timer
		var c <-chan time.Time
		if ok {
			timer = s.clock.NewTimer(next.Sub(s.clock.Now()))
			c = timer.C()
		}
		select {
		case <-s.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.kick:
		case <-c:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// queueDue queues every due, approved schedule not already waiting, pauses
// any whose content no longer matches its approval, and returns the nearest
// future due time. Re-arming from Now() on every wake is what makes a clock
// jump (suspend, NTP) run anything it skipped over at most once.
func (s *scheduler) queueDue() (time.Time, bool) {
	now := s.clock.Now()
	waiting := s.a.scheduledQueued()
	var fire []schedule.Schedule
	var next time.Time
	have := false
	s.mu.Lock()
	s.reloadLocked()
	all := s.allLocked()
	sort.SliceStable(all, func(i, j int) bool {
		di, _ := all[i].NextDue()
		dj, _ := all[j].NextDue()
		return di.Before(dj)
	})
	for _, sc := range all {
		due, ok := sc.NextDue()
		if !ok || waiting[sc.ID] {
			continue
		}
		if due.After(now) {
			if !have || due.Before(next) {
				next, have = due, true
			}
			continue
		}
		if !s.approvedLocked(sc) {
			_, _, project := s.findLocked(sc.ID)
			sc.State = schedule.Paused
			s.putLocked(sc, project)
			s.a.notice("schedule %q changed since approved and was paused; /schedule resume %s to approve it", sc.Name, sc.Name)
			continue
		}
		fire = append(fire, sc)
	}
	s.mu.Unlock()
	for _, sc := range fire {
		s.a.fireNow(sc)
	}
	return next, have
}

// fireNow queues sc's event and wakes an idle UI.
func (a *Agent) fireNow(sc schedule.Schedule) {
	a.EnqueueScheduled(sc.ID, sc.Name, fireText(sc))
	if a.Events.OnScheduleFire != nil {
		a.Events.OnScheduleFire(sc.Name)
	}
}

// fireText is the request a fired event becomes (schedules spec §2.2).
func fireText(sc schedule.Schedule) string {
	by := "you"
	if sc.CreatedBy == "agent" {
		by = "the model"
	}
	return fmt.Sprintf("[Scheduled event %q — %s, set by %s %s]\n%s",
		sc.Name, sc.When, by, sc.Created.Format("2006-01-02"), sc.Instruction)
}

// begin marks a run's start: last run now, persisted.
func (s *scheduler) begin(id string) (schedule.Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	sc, ok, project := s.findLocked(id)
	if !ok {
		return sc, false
	}
	sc.LastRun = s.clock.Now()
	s.putLocked(sc, project)
	return sc, true
}

// finish records a run's outcome (spec §2.6).
func (s *scheduler) finish(id, outcome string) {
	s.mu.Lock()
	s.reloadLocked()
	sc, ok, project := s.findLocked(id)
	if !ok {
		s.mu.Unlock()
		return
	}
	sc.LastOutcome = outcome
	paused := false
	if outcome == "ok" {
		sc.Failures = 0
	} else {
		sc.Failures++
	}
	sp, _ := sc.Spec()
	switch {
	case !sp.Recurring():
		sc.State = schedule.Done
	case s.a.Cfg.Schedules.PauseAfterFailures > 0 && sc.Failures >= s.a.Cfg.Schedules.PauseAfterFailures:
		sc.State, paused = schedule.Paused, true
	}
	s.putLocked(sc, project)
	s.mu.Unlock()
	s.a.notice("⏰ %s: %s", sc.Name, outcome)
	if paused {
		s.a.notice("schedule %q paused after %d failed runs in a row; /schedule resume %s when it is fixed", sc.Name, sc.Failures, sc.Name)
	}
	s.kickLoop()
}

// runFired runs a scheduled event's turn under its allowance (spec §2.2–2.6).
func (a *Agent) runFired(ctx context.Context, f *firing, input string) (answer string, rep *ReviewedReport, err error) {
	sc, ok := a.sched.begin(f.id)
	if !ok {
		a.notice("scheduled event %s no longer exists; running it as an ordinary request with no allowance", f.id)
		return a.runFull(ctx, input)
	}
	_, askT, maxRT := a.Cfg.Schedules.Durations()
	if sc.AskTimeout > 0 {
		askT = sc.AskTimeout
	}
	if sc.MaxRuntime > 0 {
		maxRT = sc.MaxRuntime
	}
	if sc.Task != "" && a.engine() != nil {
		var terr error
		a.engineDo("schedule task", func(st *engine.Store) { terr = st.SetStatus(sc.Task, engine.StatusDoing, "") })
		if terr != nil {
			a.notice("schedule %q: task %s: %v; running without it", sc.Name, sc.Task, terr)
		}
	}
	a.Tools.SetAllowance(sc.Allow, askT)
	rctx, cancel := context.WithTimeout(ctx, maxRT)
	defer func() {
		refused, askTimedOut := a.Tools.ClearAllowance()
		timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if r := recover(); r != nil {
			a.sched.finish(sc.ID, fmt.Sprintf("error: %v", r))
			panic(r)
		}
		if timedOut {
			err = fmt.Errorf("scheduled event %q stopped after its max runtime of %s", sc.Name, maxRT)
		}
		if askTimedOut {
			a.notice("scheduled event %q: nobody answered %s within %s; refused", sc.Name, refused, askT)
		}
		a.sched.finish(sc.ID, firedOutcome(ctx, timedOut, err, rep, refused, askTimedOut))
	}()
	return a.runFull(rctx, input)
}

func firedOutcome(parent context.Context, timedOut bool, err error, rep *ReviewedReport, refused string, askTimedOut bool) string {
	switch {
	case parent.Err() != nil:
		return "cancelled"
	case timedOut:
		return "timed out"
	case err != nil:
		return "error: " + firstLine(err.Error(), 120)
	case refused != "" && askTimedOut:
		return "refused: " + refused + " (nobody answered)"
	case refused != "":
		return "refused: " + refused
	case rep != nil && rep.Verify != nil && !rep.Verify.Passed():
		return "checks failed"
	}
	return "ok"
}
```

(`engineDo`'s real signature: check with `grep -n "func (a \*Agent) engineDo" internal/agent/*.go` and match it; `config.Dir` is what `engine.Open` uses.)

In `internal/agent/handoff.go` `Resume`, at its end:

```go
	if a.sched != nil {
		a.sched.loadTimers(s.Timers)
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/agent/ -count=1 -race -run 'Fires|Scheduled|DrainForTurn|FiredTurn|Ordinary|Missed|Dropped|HandEdited|Pause|OkResets|MaxRuntime|TaskLink'`
Expected: PASS, no race reports.

Run: `go test ./internal/agent/ -count=1`
Expected: PASS (existing inbox and sub-agent tests still pass: `DrainItems` returns what it always did for queues holding no scheduled items).

- [ ] **Step 6: Commit**

```bash
git add internal/agent/
git commit -m "agent: scheduler queues due events; fired turns run under their allowance

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 6: Managing schedules, and consent

**Files:**
- Create: `internal/agent/schedules_api.go`
- Test: `internal/agent/schedules_api_test.go`

**Interfaces:**
- Consumes: Task 5's scheduler internals.
- Produces:
  - `func (a *Agent) AddSchedule(req schedule.Request, by string) (string, error)` — `by` is `"person"` or `"agent"`
  - `func (a *Agent) ScheduleAction(action, name, by string) (string, error)` — `pause | resume | cancel | run`
  - `func (a *Agent) ScheduleLines() []string`, `func (a *Agent) ScheduleShow(name string) ([]string, error)`
  - `func (a *Agent) NextSchedule() (name string, at time.Time, ok bool)`
  - `func (a *Agent) StartSchedules()`, `func (a *Agent) StartSchedulesAsync()`
  - `func (a *Agent) SchedulesEnabled() bool`

- [ ] **Step 1: Write the failing tests**

`internal/agent/schedules_api_test.go`:

```go
package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
)

func approver(answer bool, log *[]string) func(string, string) bool {
	return func(action, detail string) bool {
		*log = append(*log, action+"|"+detail)
		return answer
	}
}

func TestAddScheduleAsksOnceWithTheWholeSummary(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	msg, err := ag.AddSchedule(schedule.Request{Name: "nightly", When: "weekdays 09:00",
		Instruction: "run the tests", Task: "3", Allow: []string{"shell: go test ./...", "write: ."}}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !strings.HasPrefix(log[0], "schedule|") {
		t.Fatalf("one schedule prompt: %v", log)
	}
	for _, want := range []string{"The model wants to schedule", "nightly", "weekdays 09:00", "next:",
		"run the tests", "task 3", "shell: go test ./...", "anywhere in the workspace"} {
		if !strings.Contains(log[0], want) {
			t.Errorf("summary lacks %q:\n%s", want, log[0])
		}
	}
	if !strings.Contains(msg, "nightly") {
		t.Fatalf("msg %q", msg)
	}
	if sc, ok, project := ag.sched.lookup("nightly"); !ok || !project || sc.CreatedBy != "agent" {
		t.Fatalf("recurring goes to schedules.md: %+v %v %v", sc, ok, project)
	}
}

func TestAddScheduleRefusals(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	cases := []struct {
		req  schedule.Request
		want string
	}{
		{schedule.Request{Name: "Bad Name", When: "in 5m", Instruction: "x"}, "name"},
		{schedule.Request{Name: "a", When: "every 1m", Instruction: "x"}, "more often"},
		{schedule.Request{Name: "a", When: "at 2020-01-01 09:00", Instruction: "x"}, "passed"},
		{schedule.Request{Name: "a", When: "in 5m", Instruction: " "}, "instruction"},
		{schedule.Request{Name: "a", When: "in 5m", Instruction: "x", Allow: []string{"shell: *"}}, "every command"},
	}
	for _, c := range cases {
		if _, err := ag.AddSchedule(c.req, "person"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %v, want %q", c.req, err, c.want)
		}
	}
	if len(log) != 0 {
		t.Fatal("an invalid schedule never reaches a prompt")
	}
	ag.AddSchedule(schedule.Request{Name: "dup", When: "in 5m", Instruction: "x"}, "person")
	if _, err := ag.AddSchedule(schedule.Request{Name: "dup", When: "in 9m", Instruction: "y"}, "person"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestAddScheduleRefusedAndDeclined(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(false, &log)
	if _, err := ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "person"); err == nil {
		t.Fatal("a declined prompt adds nothing")
	}
	if _, ok, _ := ag.sched.lookup("a"); ok {
		t.Fatal("nothing stored")
	}
	ag.Tools.Approve = approver(true, &log)
	ag.Tools.MarkUntrustedWeb()
	if _, err := ag.AddSchedule(schedule.Request{Name: "b", When: "in 5m", Instruction: "x"}, "agent"); err == nil ||
		!strings.Contains(err.Error(), "/schedule add") {
		t.Fatalf("model-created schedules are refused after a web page: %v", err)
	}
	if _, err := ag.AddSchedule(schedule.Request{Name: "c", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatalf("a person can still add one: %v", err)
	}
}

func TestMaxActive(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.Cfg.Schedules.MaxActive = 1
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "person")
	if _, err := ag.AddSchedule(schedule.Request{Name: "b", When: "in 5m", Instruction: "x"}, "person"); err == nil ||
		!strings.Contains(err.Error(), "max_active") {
		t.Fatalf("err %v", err)
	}
}

func TestModelCannotCancelPersonsScheduleWithoutAsking(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "mine", When: "daily 09:00", Instruction: "x"}, "person")
	ag.AddSchedule(schedule.Request{Name: "its", When: "daily 10:00", Instruction: "x"}, "agent")
	log = nil
	ag.Tools.Approve = approver(false, &log)
	if _, err := ag.ScheduleAction("cancel", "mine", "agent"); err == nil {
		t.Fatal("declined: the person's schedule stays")
	}
	if len(log) != 1 {
		t.Fatalf("asked the person: %v", log)
	}
	if _, err := ag.ScheduleAction("cancel", "its", "agent"); err != nil {
		t.Fatalf("its own: no prompt needed: %v", err)
	}
	if len(log) != 1 {
		t.Fatal("narrowing its own schedule never asks")
	}
	if _, ok, _ := ag.sched.lookup("its"); ok {
		t.Fatal("cancelled")
	}
}

func TestResumeAsksAndRecordsApproval(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	ag.ScheduleAction("pause", "n", "person")
	log = nil
	if _, err := ag.ScheduleAction("resume", "n", "person"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !strings.HasPrefix(log[0], "schedule|") {
		t.Fatalf("resume asks: %v", log)
	}
	if sc, _, _ := ag.sched.lookup("n"); sc.State != schedule.Active {
		t.Fatal("active again")
	}
}

func TestRunQueuesNow(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	if _, err := ag.ScheduleAction("run", "n", "person"); err != nil {
		t.Fatal(err)
	}
	if items := ag.PeekItems(); len(items) != 1 || items[0].ScheduleName != "n" {
		t.Fatalf("queued: %+v", items)
	}
}

func TestStartupGate(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("a", "every 30m"), true)
	b := mk("b", "every 30m")
	seed(t, ag, b, true)
	// b is edited by hand: its approval no longer matches.
	ag.sched.mu.Lock()
	b.Instruction = "something else"
	ag.sched.putLocked(b, true)
	ag.sched.mu.Unlock()
	var log []string
	ag.Tools.Approve = approver(false, &log)
	ag.StartSchedules()
	if len(log) != 1 || !strings.Contains(log[0], "changed since approved") {
		t.Fatalf("one startup prompt marking b: %v", log)
	}
	for _, n := range []string{"a", "b"} {
		if sc, _, _ := ag.sched.lookup(n); sc.State != schedule.Paused {
			t.Fatalf("%s paused after no", n)
		}
	}
	clock.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("nothing fires after no")
	}
}

func TestStartupGateYesApprovesAndFires(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("a", "every 30m")
	ag.sched.mu.Lock()
	ag.sched.reloadLocked()
	ag.sched.putLocked(sc, true) // never approved (hand-written)
	ag.sched.mu.Unlock()
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.StartSchedules()
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestNothingFiresBeforeStart(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("a", "in 1m"), false)
	clock.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("the loop has not started")
	}
}

func TestScheduleLinesAndNext(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "soon", When: "in 5m", Instruction: "x"}, "agent")
	ag.AddSchedule(schedule.Request{Name: "later", When: "in 50m", Instruction: "x"}, "person")
	lines := strings.Join(ag.ScheduleLines(), "\n")
	for _, want := range []string{"soon", "later", "in 5m", "active", "model", "you"} {
		if !strings.Contains(lines, want) {
			t.Errorf("list lacks %q:\n%s", want, lines)
		}
	}
	if name, at, ok := ag.NextSchedule(); !ok || name != "soon" || !at.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("next %q %v %v", name, at, ok)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'AddSchedule|MaxActive|ModelCannot|ResumeAsks|RunQueues|StartupGate|NothingFires|ScheduleLines'`
Expected: FAIL — `ag.AddSchedule undefined`.

- [ ] **Step 3: Implement**

`internal/agent/schedules_api.go`:

```go
package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
)

var errSchedulesOff = errors.New("scheduled events are disabled (schedules.enabled is false, or this is a one-shot run)")

func (a *Agent) SchedulesEnabled() bool { return a.sched != nil }

// askSchedule raises the one "schedule" prompt (no "always"; spec §3.1).
func (a *Agent) askSchedule(detail string) bool {
	return a.Tools.Approve != nil && a.Tools.Approve("schedule", detail)
}

func byLabel(by string) string {
	if by == "agent" {
		return "the model"
	}
	return "you"
}

// describe is the whole schedule for one prompt: nothing is asked later.
func describe(sc schedule.Schedule, sp schedule.Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  when: %s\n", sc.Name, sc.When)
	var nexts []string
	t := sc.Created
	for i := 0; i < 3; i++ {
		n, ok := sp.Next(t)
		if !ok {
			break
		}
		nexts = append(nexts, n.Format("Mon 2006-01-02 15:04"))
		t = n
	}
	if len(nexts) > 0 {
		fmt.Fprintf(&b, "  next: %s\n", strings.Join(nexts, " · "))
	}
	fmt.Fprintf(&b, "  instruction: %s\n", strings.ReplaceAll(sc.Instruction, "\n", "\n    "))
	if sc.Task != "" {
		fmt.Fprintf(&b, "  linked to task %s\n", sc.Task)
	}
	if len(sc.Allow) == 0 {
		b.WriteString("  may do without asking: nothing (it can read; anything else asks)\n")
	} else {
		b.WriteString("  may do without asking:\n")
		for _, g := range sc.Allow {
			line := g.String()
			if g.Kind == "write" && g.Value == "." {
				line += "   ← anywhere in the workspace"
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// AddSchedule validates, asks once, and stores (spec §3.1–3.4).
func (a *Agent) AddSchedule(req schedule.Request, by string) (string, error) {
	s := a.sched
	if s == nil {
		return "", errSchedulesOff
	}
	if by == "agent" && a.Tools.UntrustedWeb() {
		return "", errors.New("a web page was read during this request, so the model cannot create schedules until a person's next request; a person can add it with /schedule add")
	}
	minGap, _, _ := a.Cfg.Schedules.Durations()
	now := s.clock.Now()
	name := strings.TrimSpace(req.Name)
	if !schedule.ValidName(name) {
		return "", fmt.Errorf("name %q must be lowercase letters, digits and dashes (at most 40)", name)
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return "", errors.New("an instruction is required: what should the model do when it fires?")
	}
	sp, err := schedule.Parse(req.When, now)
	if err != nil {
		return "", err
	}
	if g := sp.ShortestGap(now); sp.Recurring() && g < minGap {
		return "", fmt.Errorf("%q runs more often than every %s (schedules.min_interval)", sp.String(), minGap)
	}
	if due, ok := sp.Due(); ok && !due.After(now) {
		return "", fmt.Errorf("that time has already passed (%s)", due.Format("2006-01-02 15:04"))
	}
	allow, err := schedule.ParseAllowance(req.Allow)
	if err != nil {
		return "", err
	}
	sc := schedule.Schedule{ID: schedule.NewID(), Name: name, When: sp.String(),
		Instruction: strings.TrimSpace(req.Instruction), Task: strings.TrimSpace(req.Task), Allow: allow,
		State: schedule.Active, CreatedBy: by, Created: now}

	s.mu.Lock()
	s.reloadLocked()
	active := 0
	for _, o := range s.allLocked() {
		if o.Name == name && o.State != schedule.Done {
			s.mu.Unlock()
			return "", fmt.Errorf("a schedule named %q already exists; cancel it first", name)
		}
		if o.State == schedule.Active {
			active++
		}
	}
	s.mu.Unlock()
	if max := a.Cfg.Schedules.MaxActive; max > 0 && active >= max {
		return "", fmt.Errorf("%d schedules are already active (schedules.max_active); cancel one first", active)
	}

	head := "Add this schedule?"
	if by == "agent" {
		head = "The model wants to schedule:"
	}
	if !a.askSchedule(head + "\n\n" + describe(sc, sp)) {
		return "", errors.New("the schedule was not approved")
	}
	s.mu.Lock()
	s.reloadLocked()
	err = s.putLocked(sc, sp.Recurring())
	if err == nil {
		s.approveLocked(sc)
	}
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	s.kickLoop()
	n, _ := sp.Next(now)
	return fmt.Sprintf("scheduled %q (%s); next %s", name, sp.String(), n.Format("Mon 15:04")), nil
}

// ScheduleAction: narrowing never asks, except the model narrowing a
// person's schedule; resuming always asks (spec §3.3).
func (a *Agent) ScheduleAction(action, name, by string) (string, error) {
	s := a.sched
	if s == nil {
		return "", errSchedulesOff
	}
	sc, ok, project := s.lookup(name)
	if !ok {
		return "", fmt.Errorf("no schedule named %q", name)
	}
	sp, err := sc.Spec()
	if err != nil {
		return "", err
	}
	switch action {
	case "pause", "cancel":
		if by == "agent" && sc.CreatedBy == "person" &&
			!a.askSchedule(fmt.Sprintf("The model wants to %s your schedule:\n\n%s", action, describe(sc, sp))) {
			return "", fmt.Errorf("%s of %q was not approved", action, name)
		}
		s.mu.Lock()
		s.reloadLocked()
		if action == "cancel" {
			s.removeLocked(sc.ID)
		} else {
			sc.State = schedule.Paused
			s.putLocked(sc, project)
		}
		s.mu.Unlock()
		s.kickLoop()
		return fmt.Sprintf("%s: %s", name, map[string]string{"pause": "paused", "cancel": "cancelled"}[action]), nil
	case "resume":
		if by == "agent" && a.Tools.UntrustedWeb() {
			return "", errors.New("a web page was read during this request; a person can resume it with /schedule resume")
		}
		if sc.State == schedule.Done {
			return "", fmt.Errorf("%q already ran; add it again", name)
		}
		if !a.askSchedule("Resume this schedule?\n\n" + describe(sc, sp)) {
			return "", fmt.Errorf("resuming %q was not approved", name)
		}
		s.mu.Lock()
		s.reloadLocked()
		sc.State, sc.Failures = schedule.Active, 0
		s.putLocked(sc, project)
		s.approveLocked(sc)
		s.mu.Unlock()
		s.kickLoop()
		return name + ": active", nil
	case "run":
		if by != "person" {
			return "", errors.New("only a person can run a schedule now")
		}
		s.mu.Lock()
		approved := s.approvedLocked(sc)
		s.mu.Unlock()
		if !approved {
			return "", fmt.Errorf("%q changed since it was approved; /schedule resume %s first", name, name)
		}
		if s.a.scheduledQueued()[sc.ID] {
			return name + " is already queued", nil
		}
		a.fireNow(sc)
		return name + ": queued", nil
	}
	return "", fmt.Errorf("unknown action %q (pause, resume, cancel, run)", action)
}

// ScheduleLines is /schedule's table.
func (a *Agent) ScheduleLines() []string {
	s := a.sched
	if s == nil {
		return []string{errSchedulesOff.Error()}
	}
	s.mu.Lock()
	s.reloadLocked()
	all := s.allLocked()
	s.mu.Unlock()
	if len(all) == 0 {
		return []string{"no schedules; /schedule add <name> <when> -- <instruction>"}
	}
	out := []string{fmt.Sprintf("%-18s %-18s %-12s %-7s %-10s %s", "NAME", "WHEN", "NEXT", "STATE", "BY", "LAST")}
	for _, sc := range all {
		next := "-"
		if n, ok := sc.NextDue(); ok {
			next = n.Format("Mon 15:04")
		}
		last := "-"
		if !sc.LastRun.IsZero() {
			last = sc.LastRun.Format("01-02 15:04") + " " + sc.LastOutcome
		}
		out = append(out, fmt.Sprintf("%-18s %-18s %-12s %-7s %-10s %s", sc.Name, sc.When, next, sc.State, byLabel(sc.CreatedBy), last))
	}
	return out
}

// ScheduleShow is one schedule in full.
func (a *Agent) ScheduleShow(name string) ([]string, error) {
	if a.sched == nil {
		return nil, errSchedulesOff
	}
	sc, ok, project := a.sched.lookup(name)
	if !ok {
		return nil, fmt.Errorf("no schedule named %q", name)
	}
	sp, err := sc.Spec()
	if err != nil {
		return nil, err
	}
	where := "this session (one-off)"
	if project {
		where = ".be-code/schedules.md"
	}
	lines := strings.Split(describe(sc, sp), "\n")
	lines = append(lines, "  state: "+string(sc.State), "  set by: "+byLabel(sc.CreatedBy), "  stored in: "+where)
	if !sc.LastRun.IsZero() {
		lines = append(lines, fmt.Sprintf("  last run: %s — %s", sc.LastRun.Format("2006-01-02 15:04"), sc.LastOutcome))
	}
	return lines, nil
}

// NextSchedule is the soonest active schedule not already queued.
func (a *Agent) NextSchedule() (string, time.Time, bool) {
	s := a.sched
	if s == nil {
		return "", time.Time{}, false
	}
	waiting := a.scheduledQueued()
	s.mu.Lock()
	all := s.allLocked() // no reload: the status line is redrawn often
	s.mu.Unlock()
	var name string
	var at time.Time
	for _, sc := range all {
		if n, ok := sc.NextDue(); ok && !waiting[sc.ID] && (name == "" || n.Before(at)) {
			name, at = sc.Name, n
		}
	}
	return name, at, name != ""
}

// StartSchedules raises the startup prompt, when anything is active, and
// starts the loop (spec §3.5). Plain mode calls it on its own goroutine's
// way into the REPL; the TUI uses StartSchedulesAsync.
func (a *Agent) StartSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reloadLocked()
	var active []schedule.Schedule
	for _, sc := range s.allLocked() {
		if sc.State == schedule.Active {
			active = append(active, sc)
		}
	}
	var b strings.Builder
	b.WriteString("These scheduled events will run while this session is open:\n")
	for _, sc := range active {
		mark := ""
		if !s.approvedLocked(sc) {
			mark = "   (changed since approved)"
		}
		if sp, err := sc.Spec(); err == nil {
			fmt.Fprintf(&b, "\n%s%s", describe(sc, sp), mark)
		}
	}
	s.mu.Unlock()
	if len(active) > 0 {
		if a.askSchedule(b.String()) {
			s.mu.Lock()
			for _, sc := range active {
				s.approveLocked(sc)
			}
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			s.reloadLocked()
			for _, sc := range active {
				cur, ok, project := s.findLocked(sc.ID)
				if ok {
					cur.State = schedule.Paused
					s.putLocked(cur, project)
				}
			}
			s.mu.Unlock()
			a.notice("%d scheduled events paused; /schedule resume <name> brings one back", len(active))
		}
	}
	s.startLoop()
}

// StartSchedulesAsync is StartSchedules on a goroutine of its own, fenced
// like StartSubAgentsAsync: the prompt can wait as long as a person takes,
// and a terminal attaching later is shown it.
func (a *Agent) StartSchedulesAsync() {
	if a.sched == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.notice("schedules failed to start (%v); none will run this session", r)
			}
		}()
		a.StartSchedules()
	}()
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ -count=1 -race -run 'AddSchedule|MaxActive|ModelCannot|ResumeAsks|RunQueues|StartupGate|NothingFires|ScheduleLines'`
Expected: PASS.

Run: `go test ./internal/agent/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/
git commit -m "agent: add, pause, resume, cancel and run schedules; startup prompt

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 7: The `schedule` tool and its registration

**Files:**
- Create: `internal/tools/schedule.go`
- Modify: `cmd/root.go:292` (after `attachEngine`), `cmd/commands.go:215-245` (`headlessApprover`)
- Test: `internal/tools/schedule_test.go`, `cmd/root_test.go` (append)

**Interfaces:**
- Consumes: `agent.AddSchedule`, `ScheduleAction`, `ScheduleLines`, `EnableSchedules` (Tasks 5–6).
- Produces: `type tools.Scheduler interface { AddSchedule(schedule.Request, string) (string, error); ScheduleAction(action, name, by string) (string, error); ScheduleLines() []string }`, `func tools.NewScheduleTool(s Scheduler) Tool`.

- [ ] **Step 1: Write the failing tests**

`internal/tools/schedule_test.go`:

```go
package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

type fakeScheduler struct {
	added   []schedule.Request
	by      []string
	actions []string
}

func (f *fakeScheduler) AddSchedule(r schedule.Request, by string) (string, error) {
	f.added, f.by = append(f.added, r), append(f.by, by)
	return "scheduled " + r.Name, nil
}
func (f *fakeScheduler) ScheduleAction(action, name, by string) (string, error) {
	f.actions = append(f.actions, action+" "+name+" "+by)
	if name == "missing" {
		return "", errors.New("no schedule named missing")
	}
	return name + ": done", nil
}
func (f *fakeScheduler) ScheduleLines() []string { return []string{"NAME", "nightly"} }

func schedReg(t *testing.T) (*Registry, *fakeScheduler) {
	r, err := NewRegistry(t.TempDir(), func(a, d string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeScheduler{}
	r.AddTool(NewScheduleTool(f))
	return r, f
}

func TestScheduleToolAdd(t *testing.T) {
	r, f := schedReg(t)
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"check","when":"in 20m","instruction":"look at the build","task":"2","allow":["shell: go test ./..."]}`})
	if res.IsError || !strings.Contains(res.Content, "scheduled check") {
		t.Fatalf("%+v", res)
	}
	if len(f.added) != 1 || f.by[0] != "agent" || f.added[0].Allow[0] != "shell: go test ./..." || f.added[0].Task != "2" {
		t.Fatalf("%+v %v", f.added, f.by)
	}
}

func TestScheduleToolAcceptsAllowAsString(t *testing.T) {
	r, f := schedReg(t)
	r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"c","when":"in 5m","instruction":"x","allow":"shell: go test ./...; write: docs"}`})
	if len(f.added) != 1 || len(f.added[0].Allow) != 2 || f.added[0].Allow[1] != "write: docs" {
		t.Fatalf("%+v", f.added)
	}
}

func TestScheduleToolRefusedAfterUntrustedPage(t *testing.T) {
	r, f := schedReg(t)
	r.MarkUntrustedWeb()
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"c","when":"in 5m","instruction":"x"}`})
	if !res.IsError || len(f.added) != 0 || !strings.Contains(res.Content, "/schedule add") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "2", Name: "schedule", Arguments: `{"action":"resume","name":"c"}`})
	if !res.IsError || len(f.actions) != 0 {
		t.Fatalf("resume widens too: %+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "3", Name: "schedule", Arguments: `{"action":"pause","name":"c"}`})
	if res.IsError {
		t.Fatalf("narrowing is still allowed: %+v", res)
	}
}

func TestScheduleToolListAndErrors(t *testing.T) {
	r, _ := schedReg(t)
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule", Arguments: `{"action":"list"}`})
	if res.IsError || !strings.Contains(res.Content, "nightly") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "2", Name: "schedule", Arguments: `{"action":"cancel","name":"missing"}`})
	if !res.IsError || !strings.Contains(res.Content, "no schedule") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "3", Name: "schedule", Arguments: `{"action":"run","name":"x"}`})
	if !res.IsError {
		t.Fatal("run is a person's command, not the model's")
	}
}

func TestScheduleToolNotInSubsets(t *testing.T) {
	r, _ := schedReg(t)
	for _, sub := range []*Registry{r.Subset("read_file", "list_dir", "search"), r.Scoped(nil, nil, "s")} {
		for _, n := range sub.Names() {
			if n == "schedule" {
				t.Fatal("plan mode and sub-agents never get the schedule tool")
			}
		}
	}
}
```

Append to `cmd/root_test.go` (it reuses the file's `buildAgentSubAgentConfig`, which needs no network, and pins the package flags the way `callBuildAgent` does):

```go
func buildWith(t *testing.T, cfg *config.Config, headless bool) *agent.Agent {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prevDir, prevProvider, prevModel, prevYes, prevResume := flagDir, flagProvider, flagModel, flagYes, flagResume
	flagDir, flagProvider, flagModel, flagYes, flagResume = t.TempDir(), "", "", false, ""
	t.Cleanup(func() {
		flagDir, flagProvider, flagModel, flagYes, flagResume = prevDir, prevProvider, prevModel, prevYes, prevResume
	})
	_, ag, err := buildAgent(cfg, headless)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ag.StopSchedules()
		ag.StopAllSubAgents("test over")
		ag.Tools.Close()
		ag.Checkpoints.Cleanup()
	})
	return ag
}

func hasTool(ag *agent.Agent, name string) bool {
	for _, n := range ag.Tools.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func TestScheduleToolOnlyInInteractiveSessions(t *testing.T) {
	if ag := buildWith(t, buildAgentSubAgentConfig(false), true); hasTool(ag, "schedule") || ag.SchedulesEnabled() {
		t.Fatal("headless run has no schedule tool and no scheduler")
	}
	if ag := buildWith(t, buildAgentSubAgentConfig(false), false); !hasTool(ag, "schedule") || !ag.SchedulesEnabled() {
		t.Fatal("interactive sessions get both")
	}
	off := buildAgentSubAgentConfig(false)
	off.Schedules.Enabled = false
	if ag := buildWith(t, off, false); hasTool(ag, "schedule") || ag.SchedulesEnabled() {
		t.Fatal("schedules.enabled false turns both off")
	}
}
```

If `buildAgent(cfg, false)` needs more than the headless build does in a test (for example a terminal), read the start of `buildAgent` for what `headless=false` adds and set only what it needs; do not skip the interactive assertion.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tools/ -run ScheduleTool && go test ./cmd/ -run ScheduleTool`
Expected: FAIL — `undefined: NewScheduleTool`.

- [ ] **Step 3: Implement**

`internal/tools/schedule.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/brown-enterprises/be-code/internal/schedule"
)

// Scheduler is what the schedule tool needs from the agent (which tools
// may not import): implemented by *agent.Agent.
type Scheduler interface {
	AddSchedule(req schedule.Request, by string) (string, error)
	ScheduleAction(action, name, by string) (string, error)
	ScheduleLines() []string
}

type scheduleTool struct {
	s Scheduler
	r *Registry
}

// NewScheduleTool is the model's handle on scheduled events (schedules
// spec §4.1). Registered only in interactive sessions; never in Subset or
// Scoped registries, which list their tools by name.
func NewScheduleTool(s Scheduler) Tool { return &scheduleTool{s: s} }

func (t *scheduleTool) attach(r *Registry) { t.r = r }

func (t *scheduleTool) Name() string { return "schedule" }

func (t *scheduleTool) Description() string {
	return "Schedule a request for yourself later in this session, or keep a recurring one for the project. " +
		"Use it for follow-ups (\"check the build again in 20m\") and recurring upkeep a person asked for — " +
		"never to put off work the current request asks for now. A person approves every schedule and its " +
		"allowance before it exists; ask for the narrowest allowance that does the job. " +
		"when: in 20m · at 09:00 · at 2026-09-27 09:00 · every 30m · daily 09:00 · weekdays 09:00 · mon,thu 14:30 · cron. " +
		"allow: grants like \"shell: go test ./...\", \"write: docs\", \"browser: example.com\"; anything else asks a person when it fires."
}

func (t *scheduleTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["add","list","pause","resume","cancel"]},
		"name":{"type":"string","description":"lowercase-with-dashes"},
		"when":{"type":"string"},
		"instruction":{"type":"string","description":"what to do when it fires, written as a request to yourself"},
		"task":{"type":"string","description":"optional task id to work under, e.g. 3.2"},
		"allow":{"type":"array","items":{"type":"string"}}},
		"required":["action"]}`)
}

func (t *scheduleTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	name := strings.TrimSpace(argString(args, "name"))
	untrusted := t.r != nil && t.r.UntrustedWeb()
	var msg string
	var err error
	switch action {
	case "add":
		if untrusted {
			return Result{IsError: true, Content: "a web page was read during this request, so schedules cannot be created until a person's next request; tell the user what you wanted to schedule — they can add it with /schedule add"}
		}
		allow := argStrings(args, "allow")
		if allow == nil {
			for _, g := range strings.FieldsFunc(argString(args, "allow"), func(r rune) bool { return r == ';' || r == '\n' }) {
				if g = strings.TrimSpace(g); g != "" {
					allow = append(allow, g)
				}
			}
		}
		msg, err = t.s.AddSchedule(schedule.Request{Name: name, When: argString(args, "when"),
			Instruction: argString(args, "instruction"), Task: argString(args, "task"), Allow: allow}, "agent")
	case "list":
		return Result{Content: strings.Join(t.s.ScheduleLines(), "\n")}
	case "resume":
		if untrusted {
			return Result{IsError: true, Content: "a web page was read during this request; a person can resume it with /schedule resume"}
		}
		msg, err = t.s.ScheduleAction(action, name, "agent")
	case "pause", "cancel":
		msg, err = t.s.ScheduleAction(action, name, "agent")
	default:
		return Result{IsError: true, Content: "action must be one of add, list, pause, resume, cancel"}
	}
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: msg}
}
```

(`schema(...)` is the helper the other tools use; confirm the name with `grep -n "func schema" internal/tools/*.go`.)

In `cmd/root.go`, directly after `attachEngine(cfg, reg, ag, flagResume != "")` (line ~292):

```go
	if !headless && cfg.Schedules.Enabled {
		// Scheduled events: interactive sessions only — a one-shot run has
		// no "later" (schedules spec §1.3). The scheduler is created here but
		// started by the UI once its approvals and events are wired.
		ag.EnableSchedules(schedule.RealClock{})
		reg.AddTool(tools.NewScheduleTool(ag))
	}
```

Confirm an `ag.RefreshSystem()` call still runs after this point in `buildAgent` (`grep -n "RefreshSystem" cmd/root.go`); if the last one is before this block, add `ag.RefreshSystem()` right after it. Import `internal/schedule`.

In `cmd/commands.go` `headlessApprover`, extend the unattended refusal and the non-TTY refusal to cover `schedule`:

```go
		if (action == "browser_watch" && cfg.AutoApproveBrowser) || (action == "shell_after_web" && cfg.AutoApproveShell) ||
			(action == "schedule" && (cfg.AutoApproveShell || cfg.AutoApproveBrowser)) {
```

and in the `!stdinIsTTY()` branch: `if action == "browser_watch" || action == "shell_after_web" || action == "schedule" {`. Add a comment line: `// -y never approves a schedule (schedules spec §3.1).`

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tools/ ./cmd/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/ cmd/
git commit -m "tools: the schedule tool; registered in interactive sessions only

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 8: UIs — queue, transcript, prompt, status, startup and shutdown

**Files:**
- Modify: `internal/tui/entry.go` (`entrySchedule`), `internal/tui/theme.go` (`Schedule` colour)
- Modify: `internal/tui/session.go:1051-1071` (`startQueuedLocked`), `:286-310` (Events wiring)
- Modify: `internal/tui/view.go:768-772` (non-diff modal), `:855-890` (`a` key), `:1340-1360` (title/hint), `:1219-1260` (`bottomLine`)
- Modify: `internal/ui/repl.go:125-160` (`wireQueueWake`), `:181-216` (prompt), `:506-541` (`turn`, `startQueuedTurn`)
- Modify: `cmd/live.go:144`, `cmd/root.go:542` and `:736`/`:772`
- Test: `internal/tui/schedule_test.go`, `internal/ui/repl_test.go` (append)

**Interfaces:**
- Consumes: `DrainForTurn`, `InboxItem.Scheduled/ScheduleName`, `Events.OnScheduleFire`, `NextSchedule`, `StartSchedules[Async]`, `StopSchedules`.

- [ ] **Step 1: Write the failing tests**

`internal/tui/schedule_test.go` (helpers `newTestSession`, `twoViews`, `waitFor`, `flush` are in `helpers_test.go` / `ask_test.go`):

```go
package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

func TestScheduledTurnShownAsSchedule(t *testing.T) {
	s := newTestSession(t)
	var ran string
	s.startTurnHook = func(text string) { ran = text }
	s.ag.EnqueueScheduled("id1", "nightly", "[Scheduled event \"nightly\" — daily 09:00, set by you 2026-09-26]\nrun tests")
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if !strings.Contains(ran, "run tests") {
		t.Fatalf("the scheduled event started a turn: %q", ran)
	}
	es := s.Entries()
	last := es[len(es)-1]
	if last.Kind != entrySchedule || last.Label != "nightly" {
		t.Fatalf("shown as the schedule, not the user: %+v", last)
	}
}

func TestPersonsLineRunsBeforeScheduledEvent(t *testing.T) {
	s := newTestSession(t)
	var ran []string
	s.startTurnHook = func(text string) { ran = append(ran, text) }
	s.ag.EnqueueScheduled("id1", "nightly", "scheduled")
	s.ag.EnqueueFrom("typed", 1)
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if len(ran) != 1 || ran[0] != "typed" || s.ag.Pending() != 1 {
		t.Fatalf("the typed line first, alone: %v pending=%d", ran, s.ag.Pending())
	}
}

func TestScheduleAskHasNoAlways(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeCfg, beforeReg := s.cfg.ApproveFileWrites, s.ag.Tools.ApproveWrites
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("schedule", "Add this schedule?\n\n- not a diff line") }()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk || s.cfg.ApproveFileWrites != beforeCfg || s.ag.Tools.ApproveWrites != beforeReg {
		t.Fatal(`"a" must do nothing on a schedule prompt`)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y approves")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("y never answered")
	}
}

func TestBottomLineShowsNextSchedule(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, a, _ := twoViews(t)
	s.ag.EnableSchedules(schedule.NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)))
	s.ag.Tools.Approve = func(string, string) bool { return true }
	if _, err := s.ag.AddSchedule(schedule.Request{Name: "soon", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatal(err)
	}
	flush(a)
	a.mu.Lock()
	line := a.bottomLine()
	a.mu.Unlock()
	if !strings.Contains(line, "next: soon 10:05") {
		t.Fatalf("status: %q", line)
	}
	_ = agent.TypedByPerson
}
```

If `Session` has no exported `Entries()` accessor, use the unexported one at `session.go:~505` (`grep -n "return append(\[\]entry(nil), s.entries...)" -B3 internal/tui/session.go` gives its name). If `twoViews`' `View` does not expose its lock as `a.mu`, call `bottomLine()` the way other view tests read rendered output (`grep -n "bottomLine()" internal/tui/*_test.go`). Drop the `agent` import if unused.

Append to `internal/ui/repl_test.go` (follow the existing test for `startQueuedTurn` / `wakeQueue`, `grep -n "startQueuedTurn\|wakeQueue" internal/ui/*_test.go`):

```go
func TestREPLScheduledTurnEchoesSchedule(t *testing.T) {
	r := newTestREPL(t)
	r.Agent.EnqueueScheduled("id1", "nightly", "[Scheduled event \"nightly\" — daily 09:00, set by you 2026-09-26]\nrun tests")
	out := capture(t, func() {
		if !r.startQueuedTurn(context.Background()) {
			t.Error("the scheduled event did not start a turn")
		}
	})
	if !strings.Contains(out, "⏰ nightly") || strings.Contains(out, "you>") {
		t.Fatalf("echo:\n%s", out)
	}
}

func TestREPLScheduleApprovalHasNoAlways(t *testing.T) {
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	r.lines <- lineEvent{line: "a"}
	beforeShell, beforeWrites := r.Cfg.AutoApproveShell, r.Cfg.ApproveFileWrites
	var ok bool
	out := capture(t, func() { ok = r.approveCtx(context.Background(), "schedule", "Add this schedule?") })
	if ok {
		t.Fatal(`"a" is not an answer to a schedule prompt`)
	}
	if r.Cfg.AutoApproveShell != beforeShell || r.Cfg.ApproveFileWrites != beforeWrites {
		t.Fatal(`"a" changed a standing approval`)
	}
	if !strings.Contains(out, "schedule:") {
		t.Fatalf("header:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ ./internal/ui/ -run 'Schedule'`
Expected: FAIL — `entrySchedule undefined`.

- [ ] **Step 3: Implement the TUI**

`internal/tui/entry.go`: add `entrySchedule // Label = schedule name, Text = the fired request` after `entryCowork`, and render it in the same switch as `entryCowork` with prefix `"⏰ " + e.Label` in `st.Schedule`, followed by the instruction text dimmed (strip the first `[Scheduled event …]` line: `text := e.Text; if i := strings.IndexByte(text, '\n'); i >= 0 && strings.HasPrefix(text, "[Scheduled event") { text = text[i+1:] }`).

`internal/tui/theme.go`: add `Schedule string` to `Palette` beside `Cowork`, `Schedule lipgloss.Style` to the styles struct, and where `st.Cowork` is built:

```go
	st.Schedule = lipgloss.NewStyle().Bold(true)  // in the no-colour branch, beside st.Cowork
	...
	sched := p.Schedule
	if sched == "" {
		sched = p.Cowork // palettes without their own schedule colour borrow the co-worker's
	}
	st.Schedule = fg(sched).Bold(true)
```

`internal/tui/session.go` `startQueuedLocked`: replace `left := s.ag.DrainItems()` with `left := s.ag.DrainForTurn()`, and in the echo loop:

```go
	for _, it := range left {
		if it.Scheduled != "" {
			s.appendEntryLocked(entry{Kind: entrySchedule, Label: it.ScheduleName, Text: it.Text})
		} else {
			s.appendEntryLocked(entry{Kind: entryUser, Label: s.userPrefix(it.From), Text: it.Text})
		}
		texts = append(texts, it.Text)
	}
```

In the `Events` wiring (beside `OnSubAgentEnd: s.onSubAgentEnd,`) add `OnScheduleFire: s.onScheduleFire,` and:

```go
// onScheduleFire is Events.OnScheduleFire: a due event was queued. An idle
// session starts its turn now; a busy one picks it up when its run ends
// (finishTurnLocked's leftover-queue drain), never mid-run.
func (s *Session) onScheduleFire(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startQueuedLocked()
}
```

`internal/tui/view.go`:
- line ~769: add `a.Action == "schedule" ||` to the non-diff list.
- `a`-key branch: extend `} else if a.Action == "browser_watch" || a.Action == "shell_after_web" {` to also match `a.Action == "schedule"`, and update its comment: `// No "always" to grant (browser spec §3.2, §3.6; schedules spec §3.1).`
- title/hint switch, add:

```go
	case "schedule":
		title = "Scheduled event"
		hint = "y approve · n refuse · ↑↓ scroll"
		compactHint = "y/n · ↑↓"
```

- `bottomLine`, after the `runningSubs` block:

```go
	if name, at, ok := m.ag.NextSchedule(); ok {
		line += m.st.Dim.Render(" · next: " + name + " " + at.Format("15:04"))
	}
```

- [ ] **Step 4: Implement the REPL**

`internal/ui/repl.go`:
- `wireQueueWake`: add

```go
	fire := ag.Events.OnScheduleFire
	ag.Events.OnScheduleFire = func(name string) {
		if fire != nil {
			fire(name)
		}
		r.wakeQueue()
	}
```

- `approveCtx`: add `case "schedule": fmt.Printf("%s\n%s\n", yell("schedule:"), detail)` to the header switch, and add `|| action == "schedule"` to the `[y/N]` prompt condition.
- `turn`: replace `items := r.Agent.DrainItems()` with `items := r.Agent.DrainForTurn()` and the echo with `r.echoQueued(items, input)`.
- `startQueuedTurn`: replace `DrainItems` with `DrainForTurn` and the `fmt.Printf("%s %s\n", cyan("you>"), queued)` with `r.echoQueued(items, queued)`.
- add:

```go
// echoQueued shows what the next turn is: a scheduled event by name, or the
// queued lines under "you>".
func (r *REPL) echoQueued(items []agent.InboxItem, joined string) {
	if len(items) == 1 && items[0].Scheduled != "" {
		fmt.Printf("%s %s\n", cyan("⏰ "+items[0].ScheduleName), dim(firstLineOf(items[0].Text)))
		return
	}
	fmt.Printf("%s %s\n", cyan("you>"), joined)
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

(`dim` and `cyan` are the REPL's existing colour helpers.)

- [ ] **Step 5: Wire start and stop in cmd**

- `cmd/live.go`, right after `ag.StartSubAgentsAsync()` (line ~144): `ag.StartSchedulesAsync()`.
- `cmd/root.go`, right after the local TUI's `ag.StartSubAgentsAsync()` (line ~772): `ag.StartSchedulesAsync()`.
- `cmd/root.go`, plain REPL path, right after `ag.StartSubAgents()` (line ~736): `go ag.StartSchedules()` — on its own goroutine, because its prompt is answered through the REPL's own input loop, which starts just after.
- `cmd/root.go` `finishSession`, first line after `ag.StopAllSubAgents("session ended")`: `ag.StopSchedules()`.

Read the comments around each `StartSubAgents*` call first: the ordering constraints they describe (approvals and events wired before any dispatch) apply to schedules for the same reason.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/tui/ ./internal/ui/ ./cmd/ -count=1`
Expected: PASS.

Run: `make -f build.mk verify`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/ internal/ui/ internal/agent/ cmd/
git commit -m "ui: scheduled turns in both UIs; schedule prompt; next-due status; start and stop

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 9: `/schedule`

**Files:**
- Create: `internal/ui/schedule.go`
- Modify: `internal/ui/common.go:61-90` (`SlashCommandTable`), `:116-130` (`busySafe`)
- Modify: `internal/ui/repl.go:818-830` (dispatch), `internal/tui/view.go:1880-1900` (dispatch) and a `scheduleCmd` beside `agentsCmd` (`view.go:324`)
- Test: `internal/ui/schedule_test.go`

**Interfaces:**
- Consumes: `AddSchedule`, `ScheduleAction`, `ScheduleLines`, `ScheduleShow` (Task 6).
- Produces: `func ui.ParseScheduleAdd(rest string) (schedule.Request, error)`, `func ui.ScheduleCommandLines(ag *agent.Agent, args string) []string`, `func ui.IsScheduleBlocking(args []string) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/ui/schedule_test.go`:

```go
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
```

(Check the `SlashCommandInfo` field name for the command — `grep -n "type SlashCommandInfo" -A4 internal/ui/common.go` — and adapt `c.Name`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/ui/ -run 'Schedule'`
Expected: FAIL — `undefined: ParseScheduleAdd`.

- [ ] **Step 3: Implement**

`internal/ui/schedule.go`:

```go
package ui

import (
	"errors"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

const scheduleUsage = "usage: /schedule add <name> <when> -- <instruction> [allow <grant>; <grant>…]"

// ParseScheduleAdd reads "<name> <when> -- <instruction> [allow g; g]". The
// allow clause is the last " allow " followed by a grant kind, so an
// instruction may itself use the word "allow".
func ParseScheduleAdd(rest string) (schedule.Request, error) {
	head, instr, ok := strings.Cut(rest, " -- ")
	f := strings.Fields(head)
	if !ok || len(f) < 2 || strings.TrimSpace(instr) == "" {
		return schedule.Request{}, errors.New(scheduleUsage)
	}
	r := schedule.Request{Name: f[0], When: strings.Join(f[1:], " ")}
	for i := strings.LastIndex(instr, " allow "); i >= 0; i = strings.LastIndex(instr[:i], " allow ") {
		tail := strings.TrimSpace(instr[i+len(" allow "):])
		if strings.HasPrefix(tail, "shell:") || strings.HasPrefix(tail, "write:") || strings.HasPrefix(tail, "browser:") {
			for _, g := range strings.Split(tail, ";") {
				if g = strings.TrimSpace(g); g != "" {
					r.Allow = append(r.Allow, g)
				}
			}
			instr = instr[:i]
			break
		}
	}
	r.Instruction = strings.TrimSpace(instr)
	return r, nil
}

// IsScheduleBlocking reports subcommands that can raise a prompt, which a
// TUI must run off its Update goroutine.
func IsScheduleBlocking(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "add", "resume", "pause", "cancel", "run":
		return true
	}
	return false
}

// ScheduleCommandLines runs /schedule and returns what to print. args is
// everything after "/schedule", unsplit (the instruction keeps its spacing).
func ScheduleCommandLines(ag *agent.Agent, args string) []string {
	args = strings.TrimSpace(args)
	sub, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "":
		return ag.ScheduleLines()
	case "add":
		req, err := ParseScheduleAdd(rest)
		if err != nil {
			return []string{err.Error()}
		}
		msg, err := ag.AddSchedule(req, "person")
		if err != nil {
			return []string{err.Error()}
		}
		return []string{msg}
	case "show":
		lines, err := ag.ScheduleShow(rest)
		if err != nil {
			return []string{err.Error()}
		}
		return lines
	case "pause", "resume", "cancel", "run":
		msg, err := ag.ScheduleAction(sub, rest, "person")
		if err != nil {
			return []string{err.Error()}
		}
		return []string{msg}
	}
	return []string{scheduleUsage, "       /schedule [show <name>|pause|resume|cancel|run <name>]"}
}
```

`internal/ui/common.go`: add to `SlashCommandTable` after `/task`:

```go
	{"/schedule", "scheduled events: /schedule [add <name> <when> -- <instruction> [allow …]|show|pause|resume|cancel|run <name>]", true},
```

and to `busySafe`:

```go
	// Scheduled events live in their own store; a fired one queues, never
	// interrupts, so managing them mid-run is safe.
	"/schedule": true,
```

`internal/ui/repl.go` dispatch, beside `/browser`:

```go
	case "/schedule":
		for _, l := range ScheduleCommandLines(r.Agent, strings.TrimPrefix(strings.TrimSpace(input), fields[0])) {
			fmt.Println(l)
		}
```

`internal/tui/view.go`: beside `agentsCmd`:

```go
// scheduleCmd runs a /schedule subcommand that can raise a prompt off the
// Update goroutine: Session.Ask may not be waited on under the session lock.
func (m *View) scheduleCmd(args string) tea.Cmd {
	ag := m.ag
	return func() tea.Msg { return agentsMsg{lines: ui.ScheduleCommandLines(ag, args)} }
}
```

and in the dispatch beside `/browser`:

```go
	case "/schedule":
		rest := strings.TrimPrefix(strings.TrimSpace(input), fields[0])
		if ui.IsScheduleBlocking(fields[1:]) {
			return m, m.scheduleCmd(rest)
		}
		m.renderLocalLines(ui.ScheduleCommandLines(m.ag, rest))
		return m, nil
```

(Use the variable name the surrounding dispatch uses for the raw input line; `agentsMsg` already renders a `lines` slice locally — confirm with `grep -n "case agentsMsg" internal/tui/*.go`.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/ui/ ./internal/tui/ -count=1 && make -f build.mk verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/ internal/tui/
git commit -m "ui: /schedule in both UIs

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 10: Docs, version, live checklist

**Files:**
- Modify: `README.md` (a "Scheduled events" section beside the browser section), `CHANGELOG.md` (new `1.2.0` section at the top), `build.mk` (VERSION 1.2.0), `docs/live-checklist.md` (new items after the browser's 27–32)
- Modify (outside git, never `git add`): `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md` — a "### Scheduled events (1.2.0)" section after "### Browser (1.1.5)"
- Test: none new; `make -f build.mk verify`

- [ ] **Step 1: README section**

Add after the browser section (`grep -n "^## .*[Bb]rowser" README.md`):

```markdown
## Scheduled events

A running session can wake the model later to do a pre-decided piece of work — a follow-up
("check the build in 20 minutes") or recurring upkeep ("every weekday at 09:00, pull, run the
tests and summarise what broke"). Schedules run only while a session for the workspace is open.

- **You** add one with `/schedule add nightly weekdays 09:00 -- pull, run the tests and summarise allow shell: git pull; shell: go test ./...`
- **The model** can ask for one with its `schedule` tool; you approve it in one prompt that shows the time, the instruction and everything it may do without asking.
- **Times:** `in 20m`, `at 09:00`, `at 2026-09-27 09:00`, `every 30m`, `daily 09:00`, `weekdays 09:00`, `mon,thu 14:30`, or five-field cron. Local time.
- **Allowance:** `shell: <glob>`, `write: <path>`, `browser: <host>`. Inside it the event runs unasked; anything else asks, and a question nobody answers within `ask_timeout` is refused. The deny list, the browser's watch tier, and the shell-after-a-web-page rule still apply. `-y` never approves a schedule.
- **Where they live:** recurring schedules in `.be-code/schedules.md` (readable, editable — an edited schedule is asked about again); one-off timers in the session.
- **When a session starts** with saved schedules, one prompt lists them; `no` pauses them all.
- A fired event is queued like a message and runs as its own turn (`⏰ name`), never interrupting.
- `/schedule` lists them; `/schedule show|pause|resume|cancel|run <name>` manages them.
```

- [ ] **Step 2: CHANGELOG, version**

At the top of `CHANGELOG.md`, above the 1.1.5 section, in the file's existing heading style:

```markdown
## v1.2.0 (in development)

- **Scheduled events.** A running session can wake the model at a chosen time — one-off follow-ups the model sets for itself, recurring upkeep a person sets for the project. Each schedule carries an allowance approved up front; fired events queue as their own turns. `/schedule`, the `schedule` tool, `.be-code/schedules.md`, and a `schedules` config block.
```

Set `VERSION := 1.2.0` in `build.mk` (match the file's existing assignment form). Update the README's version line (`grep -n "in development" README.md`) to name 1.2.0 and scheduled events, keeping 1.1.5's browser entry.

- [ ] **Step 3: Live checklist**

Append to `docs/live-checklist.md`, numbering on from its last item:

```markdown
N. **Overnight schedule, nobody attached.** In a hosted session: `/schedule add check every 1h -- run the tests and write a one-line result to docs/check.md allow shell: go test ./...; write: docs`, approve, then detach. Next morning, attach: the transcript shows the `⏰ check` turns, `docs/check.md` exists, `/schedule` shows `last … ok`.
N+1. **Prompt with nobody there.** A schedule with no `write:` grant whose instruction writes a file: after `ask_timeout` the prompt is withdrawn, the outcome reads `refused: file_write (nobody answered)`.
N+2. **Startup prompt.** Quit, `be-code --resume <code>`: one prompt lists the schedules; answer `n`; `/schedule` shows them paused; `/schedule resume check` asks again.
N+3. **Hand edit.** Add an `allow:` line to `.be-code/schedules.md` by hand; at the next due time the schedule pauses with "changed since approved".
N+4. **The model schedules a follow-up.** Ask "check again in 5 minutes whether the tests pass"; approve the prompt; five minutes later a `⏰` turn runs.
```

(Replace `N` with the real next number.)

- [ ] **Step 4: Root CLAUDE.md section (outside git)**

Add after the "### Browser (1.1.5)" section of `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md`, one paragraph in the file's style, covering: `internal/schedule` is a stdlib-only leaf (times, allowances, `schedules.md`, approvals); the agent's `scheduler` (one goroutine, fake-clock tested, started only by a UI through `StartSchedules[Async]` after a startup prompt, stopped in `finishSession`); scheduled inbox items are never delivered mid-run and `DrainForTurn` hands out one per turn, arming `RunFull` to run it under `Registry.SetAllowance`; the allowance is checked inside shell/process/fs/browser — **never by wrapping `Approve`**, because `Scoped` captures the parent's `Approve`; the `schedule` approval has no "always" and the TUI's `a` must list it; `-y` never approves it; the model cannot create or resume a schedule while the untrusted-web flag is set; the spec path. Do not `git add` this file.

- [ ] **Step 5: Verify and commit**

Run: `make -f build.mk verify && go test ./... -count=1`
Expected: PASS.

```bash
git add README.md CHANGELOG.md build.mk docs/live-checklist.md
git commit -m "docs: scheduled events in README, CHANGELOG 1.2.0, live checklist

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```
