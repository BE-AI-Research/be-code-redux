package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer is a minimal MCP stdio server used to test the client.
const fakeServer = `
import sys, json
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    m = msg.get("method")
    if m == "initialize":
        send({"jsonrpc":"2.0","id":msg["id"],"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake","version":"1.0"}}})
    elif m == "notifications/initialized":
        pass
    elif m == "tools/list":
        send({"jsonrpc":"2.0","id":msg["id"],"result":{"tools":[{"name":"echo","description":"echoes text back","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}})
    elif m == "tools/call":
        args = msg["params"]["arguments"]
        if args.get("text") == "explode":
            send({"jsonrpc":"2.0","id":msg["id"],"result":{"content":[{"type":"text","text":"boom"}],"isError":True}})
        else:
            send({"jsonrpc":"2.0","id":msg["id"],"result":{"content":[{"type":"text","text":"echo: "+args.get("text","")}]}})
    else:
        send({"jsonrpc":"2.0","id":msg.get("id"),"error":{"code":-32601,"message":"unknown"}})
`

func dialFake(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	script := filepath.Join(t.TempDir(), "server.py")
	if err := os.WriteFile(script, []byte(fakeServer), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(context.Background(), "fake", "python3", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestHandshakeAndToolList(t *testing.T) {
	c := dialFake(t)
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("bad schema: %v", err)
	}
}

func TestCallTool(t *testing.T) {
	c := dialFake(t)
	out, isErr, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hello mcp"}`))
	if err != nil || isErr {
		t.Fatalf("call failed: %v %v", err, isErr)
	}
	if out != "echo: hello mcp" {
		t.Fatalf("out = %q", out)
	}

	out, isErr, err = c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"explode"}`))
	if err != nil || !isErr {
		t.Fatalf("expected isError result, got %q %v %v", out, isErr, err)
	}
}

func TestServerDeathFailsPending(t *testing.T) {
	c := dialFake(t)
	c.Close()
	time.Sleep(100 * time.Millisecond)
	_, _, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected connection error, got %v", err)
	}
}
