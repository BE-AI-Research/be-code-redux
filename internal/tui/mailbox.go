package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// A mailbox is one View's inbox for messages the Session broadcasts.
//
// It exists because Session.broadcast is called from two places that must
// never block: the agent goroutine (a stalled terminal cannot be allowed to
// wedge a run) and, more sharply, from inside View.Update itself — every
// appendEntry a keystroke causes broadcasts an entryMsg, and tea.Program.Send
// writes to the very channel the Update goroutine is receiving from, so a
// direct Send there would deadlock the program for good.
//
// So a broadcast is a non-blocking hand-off into a buffered queue. A view
// that cannot keep up drops messages rather than stalling the core; that is
// the deliberate trade, and the entries themselves are never lost (they live
// on Session.entries, which rebuild() re-renders from).
type mailbox struct {
	ch   chan tea.Msg
	done chan struct{}
}

// mailboxDepth is how far a view may fall behind before its messages are
// dropped. Generous: a burst of tool output is hundreds of entries, and a
// terminal that has not been read from for that long is not coming back.
const mailboxDepth = 256

func newMailbox() *mailbox {
	return &mailbox{ch: make(chan tea.Msg, mailboxDepth), done: make(chan struct{})}
}

// send queues msg for the view, or drops it if the queue is full. It never
// blocks — see the type comment for why that is the whole point.
func (mb *mailbox) send(msg tea.Msg) {
	select {
	case mb.ch <- msg:
	default:
	}
}

// run delivers queued messages until the mailbox is closed. deliver is the
// view's tea.Program.Send: it may block (Bubble Tea's message channel is
// unbuffered and the Update goroutine may be busy), which is exactly why it
// runs here on a goroutine of its own instead of under the caller's lock.
func (mb *mailbox) run(deliver func(tea.Msg)) {
	defer close(mb.done)
	for msg := range mb.ch {
		deliver(msg)
	}
}

// close retires the mailbox. The caller must have detached the view from the
// session first, so that no broadcast can still be holding a reference and
// send on the closed channel.
func (mb *mailbox) close() { close(mb.ch) }

// drainInto delivers everything queued right now straight into v, without
// going through Bubble Tea. View.Update calls it at the end of its own body
// (still under the session lock, hence v.update and not v.Update) so that a
// view renders the entries its own keystroke just appended in the same frame;
// tests call it through flush, where nothing runs the mailbox goroutine.
//
// The commands the delivered messages produce are batched into the return so
// a drained tea.Quit is not swallowed.
func (mb *mailbox) drainInto(v *View) tea.Cmd {
	var cmds []tea.Cmd
	for {
		select {
		case msg := <-mb.ch:
			if _, cmd := v.update(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		default:
			if len(cmds) == 0 {
				return nil
			}
			return tea.Batch(cmds...)
		}
	}
}
