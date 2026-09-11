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
	"net"
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

// Client is one connected MCP server (stdio subprocess or TCP).
type Client struct {
	ServerName string

	cmd    *exec.Cmd // stdio transport only
	w      io.Writer // stdin pipe or net.Conn
	closer func()    // transport shutdown
	mu     sync.Mutex
	nextID int64
	// pending maps request id -> reply channel.
	pending map[int64]chan rpcResponse
	pmu     sync.Mutex
	tools   []ToolDef
	closed  bool
	// dead is set when readLoop ends (EOF or a read error): the transport
	// is gone, so further sends and calls fail at once instead of waiting
	// out the per-call timeout on a connection nobody is answering.
	dead bool
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

	c := &Client{ServerName: name, cmd: cmd, w: stdin, pending: map[int64]chan rpcResponse{}}
	c.closer = func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}
	go c.readLoop(stdout)

	if err := c.handshake(ctx, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// DialTCP connects to an MCP server listening on a loopback TCP address
// (the editor extension) and completes the handshake, presenting token.
func DialTCP(ctx context.Context, name, addr, token string) (*Client, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := &Client{ServerName: name, w: conn, pending: map[int64]chan rpcResponse{}}
	c.closer = func() { _ = conn.Close() }
	go c.readLoop(conn)
	if err := c.handshake(ctx, map[string]any{"token": token}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// handshake runs initialize → initialized → tools/list. auth, when
// non-nil, is sent as params.auth (the extension checks the token).
func (c *Client) handshake(ctx context.Context, auth map[string]any) error {
	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "be-code", "version": ClientVersion},
	}
	if auth != nil {
		initParams["auth"] = auth
	}
	if _, err := c.call(ctx, "initialize", initParams, 15*time.Second); err != nil {
		return fmt.Errorf("mcp %s: initialize: %w", c.ServerName, err)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	res, err := c.call(ctx, "tools/list", map[string]any{}, 15*time.Second)
	if err != nil {
		return fmt.Errorf("mcp %s: tools/list: %w", c.ServerName, err)
	}
	var listed struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(res, &listed); err != nil {
		return err
	}
	c.tools = listed.Tools
	return nil
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

// Close shuts the transport down (idempotent).
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	if c.closer != nil {
		c.closer()
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
	// EOF: the transport is gone. Mark the client dead, then fail all
	// pending calls.
	c.mu.Lock()
	c.dead = true
	c.mu.Unlock()
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
	if c.dead {
		return fmt.Errorf("mcp %s: server exited", c.ServerName)
	}
	_, err = c.w.Write(append(data, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	// Nothing will ever answer a call on a closed or dead transport: fail
	// now rather than register a pending entry and wait out the timeout.
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: connection closed", c.ServerName)
	}
	if c.dead {
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: server exited", c.ServerName)
	}
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	ch := make(chan rpcResponse, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()

	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.forget(id)
		return nil, err
	}
	// When ctx already carries its own deadline, that deadline is the only
	// limit — a caller (e.g. an editor-side review that may take minutes)
	// can ask for longer than the fixed per-call timeout. Only fall back
	// to the fixed timer when ctx has no deadline of its own.
	var timedOut <-chan time.Time
	if _, ok := ctx.Deadline(); !ok {
		timedOut = time.After(timeout)
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
	case <-timedOut:
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
