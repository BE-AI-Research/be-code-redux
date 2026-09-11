package ide

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

// fakeIDEServerReviewing is fakeIDEServer (see ide_test.go) extended to
// answer tools/call for review_diff with the given reply text, so
// Session.ReviewWrite can be exercised end to end.
func fakeIDEServerReviewing(t *testing.T, token, reply string) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req struct {
				ID     *int64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
				continue
			}
			var res any
			switch req.Method {
			case "initialize":
				var p struct {
					Auth struct {
						Token string `json:"token"`
					} `json:"auth"`
				}
				json.Unmarshal(req.Params, &p)
				if p.Auth.Token != token {
					conn.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(*req.ID, 10) + `,"error":{"code":-32001,"message":"bad token"}}` + "\n"))
					return
				}
				res = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake-ide"}}
			case "tools/list":
				res = map[string]any{"tools": []map[string]any{{"name": "review_diff", "description": "review", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				res = map[string]any{"content": []map[string]any{{"type": "text", "text": reply}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			conn.Write(append(b, '\n'))
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestSessionReviewWriteMapsDecision(t *testing.T) {
	port := fakeIDEServerReviewing(t, "tok", `{"decision":"accept_all"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d := s.ReviewWrite("a.go", "old", "new"); d != tools.ReviewAcceptAll {
		t.Fatalf("decision = %v", d)
	}
}

// A nil *Session, or a Session with no Client yet (e.g. before Connect
// succeeds), must yield ReviewUnavailable rather than panicking — mirrors
// the same guard on Session.ContextNote.
func TestSessionReviewWriteNilSafe(t *testing.T) {
	var s *Session
	if d := s.ReviewWrite("a.go", "old", "new"); d != tools.ReviewUnavailable {
		t.Fatalf("nil session: decision = %v", d)
	}
	s = &Session{}
	if d := s.ReviewWrite("a.go", "old", "new"); d != tools.ReviewUnavailable {
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
		port := fakeIDEServerReviewing(t, "tok", c.reply)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if d := s.ReviewWrite("a.go", "old", "new"); d != c.want {
			t.Errorf("reply %q: decision = %v, want %v", c.reply, d, c.want)
		}
		s.Close()
		cancel()
	}
}
