// Package mcp implements a minimal Model Context Protocol client over the
// stdio transport (newline-delimited JSON-RPC 2.0). BE-Code can attach any
// MCP tool server — including Continuum components like BE-MCPql — and
// expose its tools to the agent as first-class tools.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ToolDef is a tool advertised by a server.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client is one connected stdio MCP server.
type Client struct {
	ServerName string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	nextID int64
	// pending maps request id -> reply channel.
	pending map[int64]chan rpcResponse
	pmu     sync.Mutex
	tools   []ToolDef
	closed  bool
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Dial spawns the server process and completes the MCP handshake.
// ClientVersion is reported to servers in the initialize handshake; cmd
// sets it from the build version.
var ClientVersion = "dev"

func Dial(ctx context.Context, name, command string, args []string, env map[string]string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}

	c := &Client{ServerName: name, cmd: cmd, stdin: stdin, pending: map[int64]chan rpcResponse{}}
	go c.readLoop(stdout)

	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "be-code", "version": ClientVersion},
	}
	if _, err := c.call(ctx, "initialize", initParams, 15*time.Second); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	res, err := c.call(ctx, "tools/list", map[string]any{}, 15*time.Second)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: tools/list: %w", name, err)
	}
	var listed struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(res, &listed); err != nil {
		c.Close()
		return nil, err
	}
	c.tools = listed.Tools
	return c, nil
}

// Tools returns the server's advertised tools.
func (c *Client) Tools() []ToolDef { return c.tools }

// CallTool invokes a tool and flattens text content into one string.
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, bool, error) {
	params := map[string]any{"name": tool, "arguments": json.RawMessage(args)}
	res, err := c.call(ctx, "tools/call", params, 120*time.Second)
	if err != nil {
		return "", true, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return string(res), false, nil // non-standard but non-fatal
	}
	var parts []string
	for _, cpart := range out.Content {
		if cpart.Text != "" {
			parts = append(parts, cpart.Text)
		}
	}
	return strings.Join(parts, "\n"), out.IsError, nil
}

// Close terminates the server process.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	}
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var resp rpcResponse
		if json.Unmarshal([]byte(line), &resp) != nil || resp.ID == nil {
			continue // notification or garbage; ignore
		}
		c.pmu.Lock()
		ch, ok := c.pending[*resp.ID]
		if ok {
			delete(c.pending, *resp.ID)
		}
		c.pmu.Unlock()
		if ok {
			ch <- resp
		}
	}
	// EOF: fail all pending calls.
	c.pmu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.pmu.Unlock()
}

func (c *Client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("mcp %s: connection closed", c.ServerName)
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	ch := make(chan rpcResponse, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()

	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp %s: server exited", c.ServerName)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s (%d)", c.ServerName, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		c.forget(id)
		return nil, fmt.Errorf("mcp %s: %s timed out after %s", c.ServerName, method, timeout)
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

// forget drops a pending entry whose caller gave up, so late replies are
// discarded instead of leaking a channel per timed-out call.
func (c *Client) forget(id int64) {
	c.pmu.Lock()
	delete(c.pending, id)
	c.pmu.Unlock()
}
