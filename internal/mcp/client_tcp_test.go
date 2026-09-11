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

// fakeTCPServer answers initialize/tools/list/tools/call over newline JSON-RPC.
func fakeTCPServer(t *testing.T, token string) (addr string, gotToken *string) {
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
				res = map[string]any{"content": []map[string]any{{"type": "text", "text": "no errors"}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			conn.Write(append(b, '\n'))
		}
	}()
	return ln.Addr().String(), got
}

func TestDialTCPHandshakeAndCall(t *testing.T) {
	addr, got := fakeTCPServer(t, "secret")
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
	addr, _ := fakeTCPServer(t, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialTCP(ctx, "vscode", addr, "wrong"); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}
