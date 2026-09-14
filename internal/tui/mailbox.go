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
//
// One queue, one consumer. The queue has to be drained from two places —
// the delivery goroutine, which is how a broadcast from another goroutine
// wakes a program at all, and View.Update's own drainInto, which is how a
// keystroke's entries reach the screen in the frame that keystroke draws.
// If both *took messages off* it, the one the goroutine was holding while
// parked inside p.Send would be applied after everything the drain took:
// an entryMsg overtaken that way is dropped outright (its index is already
// behind View.renderedN) and two deltas would render out of order. So the
// goroutine never takes a message. It waits on wake and delivers a bare
// drainMsg, and drainInto is the only thing that ever reads ch — see run.
type mailbox struct {
	ch chan tea.Msg
	// wake holds at most one pending poke: one is enough, because a poke
	// drains everything queued rather than one message.
	wake chan struct{}
	done chan struct{}
}

// drainMsg is the poke the delivery goroutine sends: "there is something in
// your mailbox". It carries nothing — View.Update drains the queue itself
// after every message, this one included.
type drainMsg struct{}

// mailboxDepth is how far a view may fall behind before its messages are
// dropped. Generous: a burst of tool output is hundreds of entries, and a
// terminal that has not been read from for that long is not coming back.
const mailboxDepth = 256

func newMailbox() *mailbox {
	return &mailbox{ch: make(chan tea.Msg, mailboxDepth), wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// send queues msg for the view, or drops it if the queue is full, and pokes
// the delivery goroutine. It never blocks — see the type comment for why
// that is the whole point.
func (mb *mailbox) send(msg tea.Msg) {
	select {
	case mb.ch <- msg:
	default: // the view has fallen too far behind; drop it
	}
	select {
	case mb.wake <- struct{}{}:
	default: // a poke is already pending, and one poke drains everything
	}
}

// run pokes the view's program whenever something is queued, until the
// mailbox is closed. deliver is the view's tea.Program.Send: it may block
// (Bubble Tea's message channel is unbuffered and the Update goroutine may
// be busy), which is exactly why it runs here on a goroutine of its own
// instead of under the caller's lock — and exactly why it must not be
// holding a message while it does (see the type comment).
func (mb *mailbox) run(deliver func(tea.Msg)) {
	defer close(mb.done)
	for range mb.wake {
		deliver(drainMsg{})
	}
}

// close retires the mailbox. The caller must have detached the view from the
// session first, so that no broadcast can still be holding a reference and
// send on the closed channel.
func (mb *mailbox) close() { close(mb.wake) }

// drainInto delivers everything queued right now straight into v, without
// going through Bubble Tea. View.Update calls it at the end of its own body
// (still under the session lock, hence v.update and not v.Update) so that a
// view renders the entries its own keystroke just appended in the same frame;
// tests call it through flush, where nothing runs the mailbox goroutine.
//
// It is the mailbox's only consumer, which is what keeps queued messages in
// the order they were broadcast (see the type comment).
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
