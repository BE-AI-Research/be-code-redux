package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"

	"github.com/brown-enterprises/be-code/internal/mcp"
)

// fakeTCPServer answers initialize/tools/list/tools/call over newline JSON-RPC.
// Adapted from internal/mcp/client_tcp_test.go; kept minimal for this
// package's needs (single "diagnostics" tool, token check on initialize).
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

func TestAttachMCPPrefixedNamesTools(t *testing.T) {
	addr, _ := fakeTCPServer(t, "tok") // same helper as internal/mcp/client_tcp_test.go, adapted to package tools
	c, err := mcp.DialTCP(context.Background(), "vscode", addr, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reg, _ := NewRegistry(t.TempDir(), nil)
	names := reg.AttachMCPPrefixed(c, "ide_")
	if len(names) != 1 || names[0] != "ide_diagnostics" {
		t.Fatalf("names = %v", names)
	}
	if _, ok := reg.byName["ide_diagnostics"]; !ok {
		t.Fatal("tool not registered under prefixed name")
	}
}
