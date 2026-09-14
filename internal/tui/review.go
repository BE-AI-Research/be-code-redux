package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/brown-enterprises/be-code/internal/review"
)

// reviewTerminal is the session as the coordinator's terminal-side
// reviewer; SetReview and ReviewTerminal live on Session (see session.go).
type reviewTerminal struct{ s *Session }

// Ask raises the approval modal and waits. It runs on the agent goroutine
// (never inside Update), so the bridge is the same one tool approvals use:
// a message with a reply channel.
//
// A cancelled ctx returns false without touching the modal. The coordinator
// closes it — with the note that says why — in every path where it stops
// waiting, and it does so before Decide returns: a cancellation sent from
// here could otherwise arrive late and close the *next* write's prompt,
// which nobody would then be able to answer.
func (t reviewTerminal) Ask(ctx context.Context, preview string) bool {
	resp := make(chan bool, 1)
	// Number this prompt so its own withdrawal can be told apart from the
	// previous write's: both messages cross to the Update goroutine through
	// p.Send, and an editor that answers the instant the diff opens can get
	// its cancel there first (see approvalMsg.gen).
	gen := int(t.s.askGen.Add(1))
	t.s.send(approvalMsg{action: "file_write", detail: preview, resp: resp, gen: gen})
	select {
	case ok := <-resp:
		return ok
	case <-ctx.Done():
		return false
	}
}

// Withdraw closes a still-open prompt, noting where the answer came from. It
// carries the generation of the newest Ask — the one the coordinator is
// withdrawing, since Decide handles one write at a time.
func (t reviewTerminal) Withdraw(note string) {
	t.s.send(approvalCancelMsg{note: note, gen: int(t.s.askGen.Load())})
}

// reviewCommand is /review: with no argument it reports the mode and what
// auto currently resolves to; with one it sets the mode for the session.
func (m *View) reviewCommand(fields []string) {
	if m.review == nil {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "review: not available in this session"})
		return
	}
	if len(fields) > 1 {
		if err := m.review.SetMode(review.Mode(strings.ToLower(fields[1]))); err != nil {
			m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
			return
		}
	}
	// Resolve reads the host's client roster from inside Update, which the
	// "never call into the host from Update" rule allows: that rule is about
	// the host callbacks that p.Send into the very channel this goroutine
	// receives from (Detach, SetOverlay — see /detach). Clients() only takes
	// h.mu long enough to copy the roster and notifies nobody, and no holder
	// of h.mu ever blocks on the program, so it cannot deadlock.
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("review: %s (resolves to %s)", m.review.Mode(), m.review.Resolve())})
}
