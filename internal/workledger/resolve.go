package workledger

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Decision is what to do about a conflict.
type Decision int

const (
	// KeepMine writes this session's version. The other version is
	// recoverable: unattended, that is guaranteed before the write.
	KeepMine Decision = iota
	// KeepTheirs abandons this write.
	KeepTheirs
)

// Asker is tools.ApproveFunc's shape, taken as a plain function so this
// package does not import internal/tools — which would be the wrong
// direction, since tools reaches this through an injected hook.
type Asker func(action, detail string) bool

// Conflict is one file that changed since this session read it.
type Conflict struct {
	Path      string
	Workspace string
	Theirs    []byte
	// Others is the live work records for this workspace, so the detail can
	// say who changed it when that is knowable.
	Others []Record
}

// Detail is what a person reads to choose. It names the file, who changed it
// where a record says so, and what each answer does — a question whose
// options are not spelled out is a question people answer by habit.
func (c Conflict) Detail() string {
	var b strings.Builder
	rel := c.Path
	if r, err := filepath.Rel(c.Workspace, c.Path); err == nil && !strings.HasPrefix(r, "..") {
		rel = filepath.ToSlash(r)
	}
	fmt.Fprintf(&b, "%s changed since this session read it\n", rel)
	switch {
	case len(c.Others) == 1:
		o := c.Others[0]
		fmt.Fprintf(&b, "  another session (pid %d", o.PID)
		if o.Branch != "" {
			fmt.Fprintf(&b, ", %s", o.Branch)
		}
		if o.Task != "" {
			fmt.Fprintf(&b, ", task %s", o.Task)
		}
		b.WriteString(") is working in this workspace\n")
	case len(c.Others) > 1:
		fmt.Fprintf(&b, "  %d other sessions are working in this workspace\n", len(c.Others))
	default:
		b.WriteString("  no other session is recorded, so it was changed outside BE-Code\n")
	}
	b.WriteString("\n  y  keep mine   — write this session's version; theirs is kept and the\n")
	b.WriteString("                   notice says where\n")
	b.WriteString("  n  keep theirs — abandon this write\n")
	b.WriteString("  d  show me     — print the difference and ask again\n")
	return b.String()
}

// Resolve decides what happens to one conflict.
//
// With a person there, it asks. With nobody there — a fired scheduled turn,
// or no approver at all — it writes, but only after the old version is
// recoverable, and it says where that is. Refusing was the alternative and
// the owner ruled against it: an unattended turn should finish its work, and
// nothing should be unrecoverable.
//
// The one case it refuses is the one where that promise cannot be kept: if
// nothing can be saved, the write does not proceed.
func Resolve(ctx context.Context, c Conflict, ask Asker, fired bool, notice func(string)) (Decision, error) {
	if ask != nil && !fired {
		if ask("work_conflict", c.Detail()) {
			// A person chose to overwrite, having seen what they were
			// overwriting. Keep the old version anyway when it is free to do
			// so, but a failure here is not worth refusing their choice.
			if r, err := KeepOld(ctx, c.Workspace, c.Path); err == nil && r.Kind != RestoreNone && notice != nil {
				notice(keptNotice(c.Path, r))
			}
			return KeepMine, nil
		}
		return KeepTheirs, nil
	}

	r, err := KeepOld(ctx, c.Workspace, c.Path)
	if err != nil {
		return KeepTheirs, fmt.Errorf(
			"%s changed since this session read it, and the old version could not be kept, "+
				"so it was not overwritten: %w", filepath.Base(c.Path), err)
	}
	if notice != nil && r.Kind != RestoreNone {
		notice(keptNotice(c.Path, r))
	}
	return KeepMine, nil
}

func keptNotice(path string, r Restore) string {
	where := r.Branch
	if r.Kind == RestoreSidecar {
		where = r.Sidecar
	}
	return fmt.Sprintf("wrote %s over a change this session did not make; the old version is at %s\n  %s",
		filepath.Base(path), where, r.HowTo)
}
