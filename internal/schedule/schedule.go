package schedule

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// Hash covers exactly what a person approved: time (the spec and Created,
// which places an every-N grid), instruction, task, allowance and the
// limits (max runtime, ask timeout) — never the run bookkeeping (state,
// last run, outcome, failures). Created counts to the second, the precision
// schedules.md stores it at, and in no particular zone.
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
	fmt.Fprintf(&b, "\x00created %d\x00max %s\x00ask %s", s.Created.Unix(), s.MaxRuntime, s.AskTimeout)
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

var taskRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// ValidTask reports whether t is a task id in the engine's format (digits
// and dots, e.g. 3.2), or empty (no task).
func ValidTask(t string) bool { return t == "" || taskRe.MatchString(t) }
