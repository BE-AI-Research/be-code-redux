// Package review decides where a proposed file change is reviewed — the
// editor diff, the terminal prompt, or both at once with the first answer
// winning — and withdraws the loser.
package review

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/brown-enterprises/be-code/internal/diff"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Mode is where a change is reviewed. Auto resolves per write (see Resolve).
type Mode string

// The mode constants are ModeX rather than bare names because Editor is
// also the reviewer interface below.
const (
	ModeAuto   Mode = "auto"
	ModeEditor Mode = "editor"
	ModeTUI    Mode = "tui"
	ModeBoth   Mode = "both"
)

// Editor is the in-editor diff reviewer (an *ide.Session adapter).
type Editor interface {
	// Review shows the change and blocks until the user answers, the
	// bridge fails (ReviewUnavailable) or ctx is cancelled. shared says
	// the same change is also being offered in a terminal, so a cancel
	// may follow.
	Review(ctx context.Context, rel, old, new string, shared bool) tools.ReviewDecision
	// Cancel withdraws a pending diff for rel. Best effort.
	Cancel(rel string)
}

// Terminal is the approval-prompt reviewer (the TUI modal or the plain
// y/N question), shared by every attached client.
type Terminal interface {
	// Ask raises the approval prompt with the preview; it returns when the
	// user answers or ctx is cancelled (then false).
	Ask(ctx context.Context, preview string) bool
	// Withdraw closes a still-open prompt with a note.
	Withdraw(note string)
}

// Coordinator owns both reviewers and the mode that picks between them.
// Decide is called on the agent goroutine, one write at a time.
type Coordinator struct {
	mu      sync.Mutex
	mode    Mode
	editor  Editor
	term    Terminal
	clients func() []string
}

// New builds a coordinator. clients reports the labels of the terminals
// attached to a served session; it is nil for an in-process one. An
// unknown mode is ignored, leaving ModeAuto.
func New(mode Mode, editor Editor, term Terminal, clients func() []string) *Coordinator {
	c := &Coordinator{mode: ModeAuto, editor: editor, term: term, clients: clients}
	_ = c.SetMode(mode)
	return c
}

// Normalize maps a configured or typed value to a mode. Case and
// surrounding space are forgiven (it comes from a config file or a typed
// command); anything else is an error, with ModeAuto as the safe result so
// callers that only want to warn can use it.
func Normalize(m Mode) (Mode, error) {
	norm := Mode(strings.ToLower(strings.TrimSpace(string(m))))
	switch norm {
	case ModeAuto, ModeEditor, ModeTUI, ModeBoth:
		return norm, nil
	}
	return ModeAuto, fmt.Errorf("review mode %q: use auto, editor, tui or both", m)
}

// SetMode validates and applies a mode for the rest of the session.
// An invalid mode is an error and changes nothing.
func (c *Coordinator) SetMode(m Mode) error {
	norm, err := Normalize(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.mode = norm
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// Resolve turns auto into editor or both from the attached clients: both
// as soon as any terminal other than the VS Code one is attached.
func (c *Coordinator) Resolve() Mode {
	m := c.Mode()
	if m != ModeAuto {
		return m
	}
	if c.clients == nil {
		return ModeEditor
	}
	for _, label := range c.clients() {
		if !strings.HasPrefix(label, "vscode") {
			return ModeBoth
		}
	}
	return ModeEditor
}

type answer struct {
	from string
	d    tools.ReviewDecision
}

// editorOnly asks the editor alone, mapping a withdrawn diff (nobody
// withdrew it here, so it is simply unanswered) and a missing editor to
// ReviewUnavailable, which sends fs.go to its own approval prompt.
func (c *Coordinator) editorOnly(ctx context.Context, rel, old, new string) tools.ReviewDecision {
	if c.editor == nil {
		return tools.ReviewUnavailable
	}
	if d := c.editor.Review(ctx, rel, old, new, false); d != tools.ReviewCancelled {
		return d
	}
	return tools.ReviewUnavailable
}

// Decide implements tools.Registry.ReviewWrite. It never returns
// ReviewCancelled: a withdrawn reviewer means the other one decides.
func (c *Coordinator) Decide(ctx context.Context, rel, old, new string) tools.ReviewDecision {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return tools.ReviewReject
	}
	switch c.Resolve() {
	case ModeEditor:
		return c.editorOnly(ctx, rel, old, new)
	case ModeTUI:
		return tools.ReviewUnavailable // fs.go asks the terminal itself
	}
	if c.editor == nil || c.term == nil {
		// Nothing to race. With no editor this must NOT raise the terminal
		// prompt: fs.go's Approve is the only path that honours the
		// auto-approve switches (-y, approve_file_writes: false), which the
		// prompt itself cannot see. A shared review exists only when a live
		// editor is on the other side of it.
		return c.editorOnly(ctx, rel, old, new)
	}
	// both: race the two, first real answer wins, the other is withdrawn.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan answer, 2)
	go func() {
		results <- answer{"editor", c.editor.Review(rctx, rel, old, new, true)}
	}()
	go func() {
		preview := diff.Preview(rel, old, new, false)
		if c.term.Ask(rctx, preview) {
			results <- answer{"terminal", tools.ReviewAccept}
			return
		}
		if rctx.Err() != nil {
			results <- answer{"terminal", tools.ReviewCancelled}
			return
		}
		results <- answer{"terminal", tools.ReviewReject}
	}()
	// editorOpen tracks whether a diff may still be sitting in the editor:
	// one that already answered (or could not) has nothing to withdraw.
	editorOpen := true
	pending := 2
	for pending > 0 {
		select {
		case <-ctx.Done():
			// The run was cancelled (Esc): withdraw both places. Withdraw
			// first, then cancel, so the note reaches the prompt before the
			// prompt's own ctx.Done path returns.
			c.term.Withdraw("cancelled")
			cancel()
			if editorOpen {
				// On its own goroutine: withdrawing a diff costs a round
				// trip to the editor (up to its 3s deadline) and nothing
				// here reads the result, so the tool call must not wait.
				go c.editor.Cancel(rel)
			}
			return tools.ReviewReject
		case a := <-results:
			pending--
			if a.from == "editor" {
				editorOpen = false
			}
			if a.d == tools.ReviewUnavailable || a.d == tools.ReviewCancelled {
				continue // the other place decides
			}
			if a.from == "editor" {
				// Order matters: the terminal must see the note before its
				// own cancellation wakes it (and before this write's
				// decision lets the next one raise a fresh prompt).
				c.term.Withdraw("answered in VS Code")
				cancel()
			} else {
				cancel()
				if editorOpen {
					go c.editor.Cancel(rel) // see the ctx.Done case
				}
			}
			return a.d
		}
	}
	// Neither place could answer (no editor and no terminal answer):
	// fs.go falls back to the plain approval prompt.
	return tools.ReviewUnavailable
}
