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
