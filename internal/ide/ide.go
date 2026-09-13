// Package ide connects BE-Code to an editor bridge (the VS Code extension):
// a local MCP server advertised by a lock file, offering ide_* tools,
// editor context, and in-editor review of file changes.
package ide

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/mcp"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// contextNoteTimeout bounds how long a per-turn editor context lookup may
// block a new request, independent of the MCP client's own call timeout.
const contextNoteTimeout = 3 * time.Second

// cancelReviewTimeout bounds withdrawing a diff from the editor: it happens
// after the change was already decided elsewhere, so it must never hold the
// tool call up.
const cancelReviewTimeout = 3 * time.Second

type Lock struct {
	PID              int      `json:"pid"`
	Port             int      `json:"port"`
	Token            string   `json:"token"`
	WorkspaceFolders []string `json:"workspaceFolders"`
	IDEName          string   `json:"ideName"`
	Version          string   `json:"version"`
	path             string
	mtime            int64
}

// LockDir is ~/.be-code/ide, created on demand.
func LockDir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "ide")
	return d, os.MkdirAll(d, 0o700)
}

// Discover picks the live lock whose workspace folder contains workspace,
// else the newest live lock. Locks of dead processes are deleted.
// Returns nil, nil when no editor is listening.
func Discover(dir, workspace string) (*Lock, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ws := filepath.Clean(workspace)
	var live []*Lock
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var l Lock
		if json.Unmarshal(data, &l) != nil {
			// Malformed and can never become valid; remove it.
			_ = os.Remove(p)
			continue
		}
		if l.Port == 0 {
			continue
		}
		if l.PID <= 0 || !processAlive(l.PID) {
			// A missing/zero/negative PID is never a live process; treat it
			// the same as a dead-process lock rather than "alive forever"
			// (processAlive(0) targets the caller's own process group).
			_ = os.Remove(p)
			continue
		}
		if info, err := e.Info(); err == nil {
			l.mtime = info.ModTime().UnixNano()
		}
		l.path = p
		live = append(live, &l)
	}
	if len(live) == 0 {
		return nil, nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].mtime > live[j].mtime })
	for _, l := range live {
		for _, f := range l.WorkspaceFolders {
			f = filepath.Clean(f)
			if ws == f || strings.HasPrefix(ws, f+string(filepath.Separator)) {
				return l, nil
			}
		}
	}
	return live[0], nil
}

// Session is a live connection to the editor bridge.
type Session struct {
	Client *mcp.Client
	Lock   *Lock
}

// Connect dials the lock's server with its token.
func Connect(ctx context.Context, l *Lock) (*Session, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port))
	name := l.IDEName
	if name == "" {
		name = "ide"
	}
	c, err := mcp.DialTCP(ctx, name, addr, l.Token)
	if err != nil {
		return nil, fmt.Errorf("ide: %w", err)
	}
	return &Session{Client: c, Lock: l}, nil
}

func (s *Session) Close() {
	if s != nil && s.Client != nil {
		s.Client.Close()
	}
}

// Context is what the editor reports about the user's focus.
type Context struct {
	File      string   `json:"file"`
	Line      int      `json:"line"`
	SelStart  int      `json:"selStart"`
	SelEnd    int      `json:"selEnd"`
	Selection string   `json:"selection"`
	Open      []string `json:"open"`
}

const maxSelectionNote = 2048

// ContextNote renders the one-line note prepended to a prompt, plus the
// selected text when present and small. Empty when nothing is active.
func ContextNote(c Context) string {
	if c.File == "" {
		return ""
	}
	n := fmt.Sprintf("[editor: %s, cursor line %d", c.File, c.Line)
	if c.SelStart > 0 && c.SelEnd >= c.SelStart {
		n += fmt.Sprintf(", selection lines %d–%d", c.SelStart, c.SelEnd)
	}
	n += "]"
	if sel := strings.TrimRight(c.Selection, "\n"); sel != "" && len(sel) <= maxSelectionNote {
		n += "\n" + sel
	}
	return n
}

// ContextNote asks the editor for the current focus; any failure yields "".
func (s *Session) ContextNote(ctx context.Context) string {
	if s == nil || s.Client == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, contextNoteTimeout)
	defer cancel()
	out, isErr, err := s.Client.CallTool(ctx, "context", json.RawMessage(`{}`))
	if err != nil || isErr {
		return ""
	}
	var c Context
	if json.Unmarshal([]byte(out), &c) != nil {
		return ""
	}
	return ContextNote(c)
}

// ReviewWrite shows the change in the editor and returns the decision;
// any transport or protocol problem yields ReviewUnavailable so the TUI
// prompt takes over. The 10-minute review window hangs off the caller's
// ctx, so cancelling the run (Esc) abandons a diff left sitting in the
// editor instead of blocking the tool call for the full window.
//
// shared says the same change is also open as a terminal prompt, so this
// diff may be withdrawn by CancelReview; the extension then resolves it
// as "cancelled" (ReviewCancelled), which the coordinator ignores.
func (s *Session) ReviewWrite(ctx context.Context, rel, oldContent, newContent string, shared bool) tools.ReviewDecision {
	if s == nil || s.Client == nil {
		return tools.ReviewUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return tools.ReviewUnavailable
	}
	args, _ := json.Marshal(map[string]any{"path": rel, "original": oldContent, "proposed": newContent,
		"summary": fmt.Sprintf("BE-Code wants to change %s", rel), "shared": shared})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, isErr, err := s.Client.CallTool(ctx, "review_diff", args)
	if err != nil || isErr {
		return tools.ReviewUnavailable
	}
	var r struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return tools.ReviewUnavailable
	}
	switch r.Decision {
	case "accept":
		return tools.ReviewAccept
	case "accept_all":
		return tools.ReviewAcceptAll
	case "reject":
		return tools.ReviewReject
	case "cancelled":
		return tools.ReviewCancelled
	}
	return tools.ReviewUnavailable
}

// CancelReview withdraws a pending diff for rel because the change was
// answered elsewhere (a terminal prompt). It is best effort on purpose: it
// runs on its own short deadline — the caller's context is usually already
// cancelled by then — and ignores every error, since an extension without
// review_cancel simply leaves the diff open and its late decision is
// discarded by the coordinator.
func (s *Session) CancelReview(rel string) {
	if s == nil || s.Client == nil {
		return
	}
	args, _ := json.Marshal(map[string]string{"path": rel})
	ctx, cancel := context.WithTimeout(context.Background(), cancelReviewTimeout)
	defer cancel()
	_, _, _ = s.Client.CallTool(ctx, "review_cancel", args)
}

// ReviewEditor is the session as the review coordinator's editor-side
// reviewer (review.Editor, satisfied structurally so this package does not
// import the coordinator).
type ReviewEditor struct{ s *Session }

// ReviewEditor adapts the session for the review coordinator.
func (s *Session) ReviewEditor() ReviewEditor { return ReviewEditor{s} }

func (e ReviewEditor) Review(ctx context.Context, rel, oldContent, newContent string, shared bool) tools.ReviewDecision {
	return e.s.ReviewWrite(ctx, rel, oldContent, newContent, shared)
}

func (e ReviewEditor) Cancel(rel string) { e.s.CancelReview(rel) }
