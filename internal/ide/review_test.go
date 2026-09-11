package ide

import (
	"context"
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
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new"); d != tools.ReviewAcceptAll {
		t.Fatalf("decision = %v", d)
	}
}

// A nil *Session, or a Session with no Client yet (e.g. before Connect
// succeeds), must yield ReviewUnavailable rather than panicking — mirrors
// the same guard on Session.ContextNote.
func TestSessionReviewWriteNilSafe(t *testing.T) {
	var s *Session
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new"); d != tools.ReviewUnavailable {
		t.Fatalf("nil session: decision = %v", d)
	}
	s = &Session{}
	if d := s.ReviewWrite(context.Background(), "a.go", "old", "new"); d != tools.ReviewUnavailable {
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
		if d := s.ReviewWrite(context.Background(), "a.go", "old", "new"); d != c.want {
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
	d := s.ReviewWrite(ctx, "a.go", "old", "new")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ReviewWrite took %s; the caller's cancellation was not honoured", elapsed)
	}
	if d != tools.ReviewUnavailable {
		t.Fatalf("decision = %v, want ReviewUnavailable", d)
	}
}
