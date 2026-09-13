package ide

import (
	"bufio"
	"encoding/json"
	"net"
	"strconv"
	"testing"
)

// fakeOpts configures fakeIDEServer.
type fakeOpts struct {
	// token is the auth token the server accepts; anything else is
	// rejected with -32001 and the connection is dropped.
	token string
	// tools is the tool list reported by tools/list.
	tools []string
	// callReply is the text content returned for tools/call.
	callReply string
	// hangOnCall accepts tools/call but never answers it, so the caller's
	// own deadline has to bound the wait.
	hangOnCall bool
	// onCall, when set, records every tools/call the client makes. It runs
	// on the server goroutine, so it must be safe for concurrent use.
	onCall func(name string, args json.RawMessage)
}

// fakeIDEServer stands in for the editor extension's embedded MCP server:
// newline-delimited JSON-RPC answering initialize / tools/list / tools/call
// on loopback. It returns the port and a pointer to the token last
// presented by a client.
func fakeIDEServer(t *testing.T, o fakeOpts) (port int, gotToken *string) {
	t.Helper()
	if len(o.tools) == 0 {
		o.tools = []string{"ide_diagnostics"}
	}
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
				if p.Auth.Token != o.token {
					conn.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(*req.ID, 10) + `,"error":{"code":-32001,"message":"bad token"}}` + "\n"))
					return
				}
				res = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake-ide"}}
			case "tools/list":
				var list []map[string]any
				for _, name := range o.tools {
					list = append(list, map[string]any{"name": name, "description": name, "inputSchema": map[string]any{"type": "object"}})
				}
				res = map[string]any{"tools": list}
			case "tools/call":
				if o.onCall != nil {
					var p struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"arguments"`
					}
					json.Unmarshal(req.Params, &p)
					o.onCall(p.Name, p.Args)
				}
				if o.hangOnCall {
					continue // never reply; the caller's context must bound the wait
				}
				res = map[string]any{"content": []map[string]any{{"type": "text", "text": o.callReply}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			conn.Write(append(b, '\n'))
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}
