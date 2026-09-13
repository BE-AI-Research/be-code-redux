package ide

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

func TestSessionReviewWriteMapsDecision(t *testing.T) {
	port, _ := fakeIDEServer(t, fakeOpts{token: "tok", tools: []string{"review_diff"}, callReply: `{"decision":"accept_all"}`})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", false); d != tools.ReviewAcceptAll {
		t.Fatalf("decision = %v", d)
	}
}

// A nil *Session, or a Session with no Client yet (e.g. before Connect
// succeeds), must yield ReviewUnavailable rather than panicking — mirrors
// the same guard on Session.ContextNote.
func TestSessionReviewWriteNilSafe(t *testing.T) {
	var s *Session
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", false); d != tools.ReviewUnavailable {
		t.Fatalf("nil session: decision = %v", d)
	}
	s = &Session{}
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", false); d != tools.ReviewUnavailable {
		t.Fatalf("session with nil client: decision = %v", d)
	}
}

func TestSessionReviewWriteMapsAllDecisions(t *testing.T) {
	cases := []struct {
		reply string
		want  tools.ReviewDecision
	}{
		{`{"decision":"accept"}`, tools.ReviewAccept},
		{`{"decision":"reject"}`, tools.ReviewReject},
		{`{"decision":"accept_all"}`, tools.ReviewAcceptAll},
		{`{"decision":"bogus"}`, tools.ReviewUnavailable},
		{`not json`, tools.ReviewUnavailable},
	}
	for _, c := range cases {
		port, _ := fakeIDEServer(t, fakeOpts{token: "tok", tools: []string{"review_diff"}, callReply: c.reply})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", false); d != c.want {
			t.Errorf("reply %q: decision = %v, want %v", c.reply, d, c.want)
		}
		s.Close()
		cancel()
	}
}

// Esc cancels the run while a diff sits unanswered in the editor: the
// 10-minute review window hangs off the caller's context, so ReviewWrite
// returns at once (unavailable) instead of pinning the tool call.
func TestSessionReviewWriteAbandonedOnCancel(t *testing.T) {
	port, _ := fakeIDEServer(t, fakeOpts{token: "tok", tools: []string{"review_diff"}, hangOnCall: true})
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	s, err := Connect(dialCtx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	d := s.ReviewWrite(ctx, "a.go", "old", "new", false)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ReviewWrite took %s; the caller's cancellation was not honoured", elapsed)
	}
	if d != tools.ReviewUnavailable {
		t.Fatalf("decision = %v, want ReviewUnavailable", d)
	}
}

// callLog records the tools/call traffic a test's fake server sees.
type callLog struct {
	mu    sync.Mutex
	names []string
	args  []string
}

func (l *callLog) record(name string, args json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
	l.args = append(l.args, string(args))
}

func (l *callLog) last() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.names) == 0 {
		return "", ""
	}
	return l.names[len(l.names)-1], l.args[len(l.args)-1]
}

func connectFake(t *testing.T, o fakeOpts) *Session {
	t.Helper()
	o.token = "tok"
	port, _ := fakeIDEServer(t, o)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	s, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// A shared review tells the extension so, and the "cancelled" decision the
// extension reports when the other place answered first maps to
// ReviewCancelled (which the coordinator ignores).
func TestSessionReviewWriteSharedAndCancelled(t *testing.T) {
	var log callLog
	s := connectFake(t, fakeOpts{tools: []string{"review_diff"}, callReply: `{"decision":"cancelled"}`, onCall: log.record})
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", true); d != tools.ReviewCancelled {
		t.Fatalf("decision = %v, want ReviewCancelled", d)
	}
	name, args := log.last()
	if name != "review_diff" {
		t.Fatalf("tool called = %q", name)
	}
	if !strings.Contains(args, `"shared":true`) {
		t.Fatalf("shared flag missing from args: %s", args)
	}
	if !strings.Contains(args, `"path":"a.go"`) || !strings.Contains(args, `"proposed":"new"`) {
		t.Fatalf("args lost their content: %s", args)
	}
}

// An unshared review is still announced as unshared, so the extension can
// tell "only place to answer" from "one of two".
func TestSessionReviewWriteUnshared(t *testing.T) {
	var log callLog
	s := connectFake(t, fakeOpts{tools: []string{"review_diff"}, callReply: `{"decision":"accept"}`, onCall: log.record})
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new", false); d != tools.ReviewAccept {
		t.Fatalf("decision = %v", d)
	}
	if _, args := log.last(); !strings.Contains(args, `"shared":false`) {
		t.Fatalf("shared flag missing from args: %s", args)
	}
}

// CancelReview withdraws the editor diff by path. It runs on its own short
// deadline (the caller's context is typically already cancelled) and
// swallows every error: an extension without review_cancel just leaves the
// diff open.
func TestSessionCancelReview(t *testing.T) {
	var log callLog
	s := connectFake(t, fakeOpts{tools: []string{"review_diff", "review_cancel"}, callReply: `{"ok":true}`, onCall: log.record})
	s.CancelReview("a.go")
	name, args := log.last()
	if name != "review_cancel" {
		t.Fatalf("tool called = %q", name)
	}
	if !strings.Contains(args, `"path":"a.go"`) {
		t.Fatalf("path missing from args: %s", args)
	}
}

// A nil session (no editor attached) must not panic.
func TestSessionCancelReviewNilSafe(t *testing.T) {
	var s *Session
	s.CancelReview("a.go")
	(&Session{}).CancelReview("a.go")
}

// The review.Editor adapter forwards to the session's own methods.
func TestSessionReviewEditorAdapter(t *testing.T) {
	var log callLog
	s := connectFake(t, fakeOpts{tools: []string{"review_diff", "review_cancel"}, callReply: `{"decision":"reject"}`, onCall: log.record})
	ed := s.ReviewEditor()
	if d := ed.Review(context.Background(), "a.go", "old", "new", true); d != tools.ReviewReject {
		t.Fatalf("decision = %v", d)
	}
	ed.Cancel("a.go")
	if name, _ := log.last(); name != "review_cancel" {
		t.Fatalf("Cancel called %q", name)
	}
}
