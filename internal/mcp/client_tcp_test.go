package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeTCPServer answers initialize/tools/list/tools/call over newline
// JSON-RPC. When hangOnCall is true, tools/call is accepted (the handshake
// still completes) but never answered, so a caller relying on it must be
// bounded by its own context deadline.
func fakeTCPServer(t *testing.T, token string, hangOnCall bool) (addr string, gotToken *string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := new(string)
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
				*got = p.Auth.Token
				if p.Auth.Token != token {
					conn.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(*req.ID, 10) + `,"error":{"code":-32001,"message":"bad token"}}` + "\n"))
					return
				}
				res = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}}
			case "tools/list":
				res = map[string]any{"tools": []map[string]any{{"name": "diagnostics", "description": "errors", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				if hangOnCall {
					continue // never reply; the caller's context must bound the wait
				}
				res = map[string]any{"content": []map[string]any{{"type": "text", "text": "no errors"}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			conn.Write(append(b, '\n'))
		}
	}()
	return ln.Addr().String(), got
}

func TestDialTCPHandshakeAndCall(t *testing.T) {
	addr, got := fakeTCPServer(t, "secret", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialTCP(ctx, "vscode", addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if *got != "secret" {
		t.Fatalf("token sent = %q", *got)
	}
	if len(c.Tools()) != 1 || c.Tools()[0].Name != "diagnostics" {
		t.Fatalf("tools = %+v", c.Tools())
	}
	out, isErr, err := c.CallTool(ctx, "diagnostics", json.RawMessage(`{}`))
	if err != nil || isErr || out != "no errors" {
		t.Fatalf("call: out=%q isErr=%v err=%v", out, isErr, err)
	}
}

func TestDialTCPRejectsBadToken(t *testing.T) {
	addr, _ := fakeTCPServer(t, "secret", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialTCP(ctx, "vscode", addr, "wrong"); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}

// A caller may pass a context with its own (possibly long) deadline for a
// single call — e.g. an editor-side review that can take minutes — and
// that deadline, not the fixed per-call timeout, must bound the wait.
// Here the server never answers tools/call at all, so this only returns
// quickly if the context deadline is actually honoured.
func TestCallHonoursContextDeadline(t *testing.T) {
	addr, _ := fakeTCPServer(t, "secret", true)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	c, err := DialTCP(dialCtx, "vscode", addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = c.CallTool(ctx, "diagnostics", json.RawMessage(`{}`))
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected context deadline error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("call took %s; context deadline (300ms) was not honoured", elapsed)
	}
}

// The bug this guards against only shows up when the caller's context
// deadline is LONGER than CallTool's fixed 120s timeout (e.g.
// Session.ReviewWrite's 10-minute context) — the fixed timer must not cut
// the wait short. Waiting >120s in a unit test is impractical, so this
// calls the unexported call() directly with a deliberately short "fixed"
// timeout and a longer context deadline, standing in for that relationship
// at a testable timescale: the short fixed timeout must not fire early.
func TestCallContextDeadlineNotCappedByFixedTimeout(t *testing.T) {
	addr, _ := fakeTCPServer(t, "secret", true)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	c, err := DialTCP(dialCtx, "vscode", addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.call(ctx, "tools/call", map[string]any{"name": "diagnostics", "arguments": json.RawMessage(`{}`)}, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected context deadline error (not a fixed-timeout error), got %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("returned after %s; the 100ms fixed timeout fired instead of the 500ms context deadline", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("call took %s; too slow", elapsed)
	}
}
