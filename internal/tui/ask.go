package tui

import (
	"context"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/review"
)

// An ask is one question the session puts to every attached terminal at
// once — a tool approval, a plan waiting for a verdict, or a shared picker.
// The first terminal to answer decides it for the whole session; the rest
// have their modal closed and are told who answered.
//
// This is the shared half of what used to be a per-terminal modal. The
// question and its generation live on the Session, so a second terminal can
// never open a copy nobody is waiting for, and a withdrawal can never race
// ahead of the prompt it withdraws: both cross to a view through the same
// ordered mailbox.
type askKind int

const (
	askApproval askKind = iota
	askPlan
	askPicker
)

type ask struct {
	Gen    int
	Kind   askKind
	Action string     // askApproval: "shell" | "file_write"
	Title  string     // askPicker title / plan title
	Detail string     // approval diff preview / plan text
	Items  []pickItem // askPicker
	// onPick runs on the answering view, under s.mu, with the picked id.
	// Its tea.Cmd (the session picker's terminal switch) is returned to
	// that view's Update.
	onPick func(v *View, id string, from int) tea.Cmd
	reply  chan askAnswer // askApproval, askPlan, askPicker
	req    string         // askPlan: the request text, for ExecutePlan
	// raised, when set, is called under s.mu the moment Gen is assigned, so
	// an asker that has to withdraw its own prompt later knows which
	// generation it raised (see reviewTerminal). It must not take a lock.
	raised func(gen int)
}

type askAnswer struct {
	OK   bool
	Note string // approval note, or the picked id for askPicker
	From int
	// Refused says the question was never put to anyone: another prompt was
	// already on screen. It is not a verdict, and a caller that reads OK as
	// a decision must not treat it as one silently.
	Refused bool
}

// askMsg is broadcast when an ask opens, askResolvedMsg when it is answered
// or withdrawn. by names the terminal that answered, or carries a
// withdrawal note ("answered in VS Code"); "" shows nothing.
//
// from is the client id that answered, so a terminal can tell "somebody else
// decided this" from "I decided this" by identity rather than by comparing
// labels — two terminals may well share one. A withdrawal has no answering
// client and carries noClient.
type askMsg struct{ a *ask }

// noClient is askResolvedMsg.from for a withdrawal: no client answered, so
// the note is for everyone. It cannot collide with a real client id (0 is
// the in-process terminal; the host numbers from 1).
const noClient = -1

type askResolvedMsg struct {
	gen  int
	by   string
	from int
}

// Ask raises a shared question and blocks until a terminal answers it, it is
// withdrawn, or ctx ends. It must never be called while holding s.mu: its
// callers are the agent goroutine (approveFromAgent, reviewTerminal.Ask),
// the plan goroutine, and the picker-loading tea.Cmd goroutines.
func (s *Session) Ask(ctx context.Context, a *ask) askAnswer {
	a.reply = make(chan askAnswer, 1)
	s.mu.Lock()
	// Two questions cannot share one screen, and displacing the open one is
	// how a file write used to be denied without a line anywhere saying so.
	//
	// A picker is the one ask worth displacing: nothing waits on its verdict
	// (askList throws the answer away), so an approval raised while a model
	// or session list is up takes the screen and the list is withdrawn —
	// every terminal closes it on the askResolvedMsg. Anything else refuses
	// the newcomer instead, and says so where its asker will be seen.
	if old := s.ask; old != nil {
		if old.Kind == askPicker && a.Kind != askPicker {
			s.ask = nil
			select {
			case old.reply <- askAnswer{}:
			default:
			}
			s.broadcast(askResolvedMsg{gen: old.Gen, by: "withdrawn", from: noClient})
		} else {
			if a.Kind != askPicker {
				// An approval or a plan: its caller reads the refusal as a
				// denial, so the transcript has to carry what happened.
				s.appendEntryLocked(entry{Kind: entryWarn,
					Text: "another prompt is already open; this request was not shown and counts as denied"})
			}
			s.mu.Unlock()
			return askAnswer{Refused: true}
		}
	}
	s.askGen++
	a.Gen = s.askGen
	if a.raised != nil {
		a.raised(a.Gen)
	}
	s.ask = a
	if s.quitCh == nil {
		s.quitCh = make(chan struct{})
	}
	quit := s.quitCh
	s.broadcast(askMsg{a})
	s.mu.Unlock()
	select {
	case ans := <-a.reply:
		return ans
	case <-quit:
		// The session ended with this question still open — the last
		// terminal went away, or /quit was typed on another one. Release the
		// asker rather than leave it parked for the life of the process.
		s.CancelAsk(a.Gen, "")
		select {
		case ans := <-a.reply:
			return ans
		default:
			return askAnswer{}
		}
	case <-ctx.Done():
		s.CancelAsk(a.Gen, "")
		// An answer given at the very instant the context ended is already
		// in the (buffered) channel and is the real verdict: a terminal
		// approved this write, so reporting a denial would be a lie. If
		// CancelAsk got there first its own zero answer is what is read.
		select {
		case ans := <-a.reply:
			return ans
		default:
			return askAnswer{}
		}
	}
}

// current is the open ask, if any. The caller holds s.mu.
func (s *Session) current() *ask { return s.ask }

// Answer resolves the open ask if gen is still current, reporting false for
// a stale generation (another terminal got there first). The caller holds
// s.mu — it is a view's Update — and nothing here blocks: the reply channel
// is buffered and broadcast only queues.
//
// v is the view that rendered the ask and is answering it, and from the
// client id that pressed the key. They are two different things while one
// program serves a whole roster (RunServed's single view is client 0's while
// the keys come in tagged 1, 2, …), which is why the view is passed in
// rather than looked up from the id: onPick's view-local effects have to
// land on the terminal that is actually rendering.
//
// The verdict lines are session entries, not local notes, so every terminal
// sees what was decided.
func (s *Session) Answer(gen int, ans askAnswer, from int, v *View) (tea.Cmd, bool) {
	a := s.ask
	if a == nil || a.Gen != gen {
		return nil, false
	}
	ans.From = from
	var cmd tea.Cmd
	switch a.Kind {
	case askApproval:
		verdict := "denied"
		if ans.OK {
			verdict = "approved"
		}
		s.appendEntryLocked(entry{Kind: entryVerdict, Label: a.Action, Text: verdict})
		if ans.Note != "" {
			s.appendEntryLocked(entry{Kind: entryDim, Text: ans.Note})
		}
	case askPlan:
		if ans.OK {
			s.appendEntryLocked(entry{Kind: entryOK, Text: "plan approved — executing"})
		} else {
			s.appendEntryLocked(entry{Kind: entryWarn, Text: "plan discarded"})
		}
	case askPicker:
		if ans.OK && a.onPick != nil {
			cmd = a.onPick(v, ans.Note, from)
		}
	}
	a.reply <- ans
	s.ask = nil
	s.broadcast(askResolvedMsg{gen: gen, by: s.clientLabel(from), from: from})
	return cmd, true
}

// CancelAsk withdraws the open ask — the editor answered it, or the asker's
// context ended. by is the note every view shows; "" shows nothing. A late
// or duplicate withdrawal is a no-op.
func (s *Session) CancelAsk(gen int, by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.ask
	if a == nil || a.Gen != gen {
		return
	}
	s.ask = nil
	select {
	case a.reply <- askAnswer{}:
	default:
	}
	s.broadcast(askResolvedMsg{gen: gen, by: by, from: noClient})
}

// SetReview hands the session the review coordinator built in cmd, so
// /review can report and change where file changes are reviewed.
func (s *Session) SetReview(c *review.Coordinator) { s.review = c }

// ReviewTerminal is this session as the coordinator's terminal-side
// reviewer: the shared ask, which any attached terminal can answer.
func (s *Session) ReviewTerminal() review.Terminal { return &reviewTerminal{s: s} }

type reviewTerminal struct {
	s *Session
	// gen is the generation of the prompt this adapter last raised, so
	// Withdraw closes exactly that one. It is written from inside Ask (under
	// s.mu) and read by Withdraw on the coordinator's goroutine, hence the
	// atomic rather than a mutex: a lock taken here would be taken under
	// s.mu one way and before it the other.
	gen atomic.Int64
}

// Ask raises the shared approval and waits. It runs on the agent goroutine,
// never inside Update. A cancelled ctx returns false; the coordinator closes
// the prompt itself, with the note that says why, in every path where it
// stops waiting.
func (t *reviewTerminal) Ask(ctx context.Context, preview string) bool {
	return t.s.Ask(ctx, &ask{Kind: askApproval, Action: "file_write", Detail: preview,
		raised: func(gen int) { t.gen.Store(int64(gen)) }}).OK
}

// Withdraw closes a still-open prompt, noting where the answer came from. It
// names the prompt this adapter itself raised, not simply the newest one: a
// picker displaced in between would otherwise be the ask a stale generation
// pointed at. CancelAsk ignores it if that prompt has already been answered,
// and a Withdraw with nothing ever raised (generation 0) matches nothing.
func (t *reviewTerminal) Withdraw(note string) { t.s.CancelAsk(int(t.gen.Load()), note) }

// approveFromAgent bridges the agent goroutine into the shared ask: the
// approval modal every attached terminal sees, and any of them may answer.
// It waits on the session's own lifetime, which is right for a tool call:
// the thing that raised it lives as long as the session does.
func (s *Session) approveFromAgent(action, detail string) bool {
	ctx := s.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.approveFromAgentCtx(ctx, action, detail)
}

// approveFromAgentCtx is the same modal for an asker that can give up on
// its own question — a model-parameter resolution under a deadline. Ask
// already withdraws on ctx (CancelAsk, broadcast to every view), so the
// prompt closes everywhere rather than outliving the goroutine waiting for
// it. See tools.ApproveCtxFunc.
func (s *Session) approveFromAgentCtx(ctx context.Context, action, detail string) bool {
	if action == "shell" && s.cfg.AutoApproveShell {
		return true
	}
	if action == "file_write" && !s.cfg.ApproveFileWrites {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.Ask(ctx, &ask{Kind: askApproval, Action: action, Detail: detail}).OK
}
