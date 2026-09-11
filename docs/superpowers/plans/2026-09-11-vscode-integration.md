# VS Code Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give BE-Code, running as the TUI inside VS Code's terminal, the editor's diagnostics, symbol information, debugger, editor-side diff review of file changes, and automatic "what the user is looking at" context.

**Architecture:** A VS Code extension hosts a small MCP server over loopback TCP (newline-delimited JSON-RPC) and advertises `ide_*` tools; it writes a lock file BE-Code discovers at startup. BE-Code connects with its existing MCP client through a new TCP transport, registers the tools with an `ide_` prefix, calls `ide_context` before each prompt, and routes file-write approvals to `ide_review_diff` with a TUI fallback.

**Tech Stack:** Go 1.25 (module `github.com/brown-enterprises/be-code`), existing `internal/mcp` client; TypeScript extension in `be-code/vscode/` built with esbuild, tested with vitest, packaged with `@vscode/vsce`; Node 22.

**Spec:** `be-code/docs/superpowers/specs/2026-09-11-vscode-integration-design.md`

## Global Constraints

- All paths in tool arguments and results are workspace-relative; the extension resolves against the first workspace folder that contains the file.
- IDE tools are named `ide_<name>` on the BE-Code side (never `mcp_vscode_<name>`).
- Lock file path: `~/.be-code/ide/<pid>.json` with fields `pid, port, token, workspaceFolders, ideName, version`.
- Server binds `127.0.0.1` only; `initialize` params must carry `auth.token`; a wrong token gets JSON-RPC error code `-32001` and the connection is closed.
- Debug: one session at a time; `ide_debug_start`, `ide_debug_continue`, `ide_debug_step` wait up to 60 s for the next `stopped`/`terminated`/`exited` event.
- Tool results are plain text capped by `Registry.MaxOutput`; `ide_context` and `ide_review_diff` return compact JSON text.
- Shell approvals never go to the editor; headless `run` never uses the editor.
- The repository is **not** a git checkout: skip the commit steps, but run `make -f build.mk verify` (Go) and `npm test` (extension) at the end of each task.
- Extension version tracks `tui.PublicVersion` (`v1.0` → `1.0.0`).

---

### Task 1: TCP transport for the MCP client

**Files:**
- Modify: `internal/mcp/client.go` (Client struct, `Dial`, `Close`, `send`)
- Test: `internal/mcp/client_tcp_test.go`

**Interfaces:**
- Consumes: existing `Client.call`, `Client.notify`, `readLoop`.
- Produces: `func DialTCP(ctx context.Context, name, addr, token string) (*Client, error)` — same handshake as `Dial`, sends `params.auth.token`; `Client.Close()` works for both transports.

- [ ] **Step 1: Write the failing test** — a fake MCP server over TCP that checks the token and lists one tool.

```go
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
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
					Auth struct{ Token string `json:"token"` } `json:"auth"`
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
```

Imports for the test file: `bufio`, `context`, `encoding/json`, `net`, `strconv`, `strings`, `testing`, `time`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/mcp -run TestDialTCP -v`
Expected: build failure `undefined: DialTCP`.

- [ ] **Step 3: Implement** — generalize the client over an `io.Writer` plus a close function.

In `internal/mcp/client.go`:

```go
// Client is one connected MCP server (stdio subprocess or TCP).
type Client struct {
	ServerName string

	cmd    *exec.Cmd // stdio transport only
	w      io.Writer // stdin pipe or net.Conn
	closer func()    // transport shutdown
	mu     sync.Mutex
	nextID int64
	pending map[int64]chan rpcResponse
	pmu     sync.Mutex
	tools   []ToolDef
	closed  bool
}
```

Change `Dial` to build `c := &Client{ServerName: name, cmd: cmd, w: stdin, pending: ...}` with `closer` that closes stdin and waits/kills the process (move the body of today's `Close` into it), then call a shared `c.handshake(ctx, nil)`. Add:

```go
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
```

`send` writes to `c.w` instead of `c.stdin`. Keep the "server exited: fail all pending" behaviour in `readLoop` (it already closes pending channels on EOF, which also covers a dropped TCP connection). Add `"net"` to imports.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/mcp -v`
Expected: PASS, including the existing stdio subprocess test.

- [ ] **Step 5: Verify** `make -f build.mk verify`.

---

### Task 2: Prefixed tool attachment

**Files:**
- Modify: `internal/tools/mcp_tool.go`
- Test: `internal/tools/mcp_prefix_test.go`

**Interfaces:**
- Produces: `func (r *Registry) AttachMCPPrefixed(client *mcp.Client, prefix string) []string`; `AttachMCP(client)` = `AttachMCPPrefixed(client, "")`; `MCPTool.Name()` returns `prefix+Def.Name` when prefix is set.

- [ ] **Step 1: Write the failing test** — `mcp.Client` cannot be built without a transport, so test through the fake TCP server from Task 1 (copy `fakeTCPServer` into this test file under the `tools` package, importing `internal/mcp`):

```go
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
```

- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/tools -run TestAttachMCPPrefixed`; expected `undefined: AttachMCPPrefixed`.

- [ ] **Step 3: Implement**

```go
// MCPTool adapts one MCP server tool into the agent's tool interface.
type MCPTool struct {
	Client *mcp.Client
	Def    mcp.ToolDef
	prefix string // "" → mcp_<server>_<tool>; "ide_" → ide_<tool>
	r      *Registry
}

// AttachMCP registers all of a server's tools under mcp_<server>_<tool>.
func (r *Registry) AttachMCP(client *mcp.Client) []string { return r.AttachMCPPrefixed(client, "") }

// AttachMCPPrefixed registers a server's tools under prefix+<tool>, for
// servers whose tool names should be stable regardless of server name
// (the editor bridge uses "ide_").
func (r *Registry) AttachMCPPrefixed(client *mcp.Client, prefix string) []string {
	r.mcpClients = append(r.mcpClients, client)
	var names []string
	for _, def := range client.Tools() {
		t := &MCPTool{Client: client, Def: def, prefix: prefix, r: r}
		r.AddTool(t)
		names = append(names, t.Name())
	}
	return names
}

func (t *MCPTool) Name() string {
	if t.prefix != "" {
		return t.prefix + t.Def.Name
	}
	return fmt.Sprintf("mcp_%s_%s", t.Client.ServerName, t.Def.Name)
}
```

- [ ] **Step 4: Run tests** `go test ./internal/tools`; expected PASS.
- [ ] **Step 5: Verify** `make -f build.mk verify`.

---

### Task 3: IDE lock discovery and connection

**Files:**
- Create: `internal/ide/ide.go`, `internal/ide/pid_unix.go`, `internal/ide/pid_windows.go`
- Test: `internal/ide/ide_test.go`

**Interfaces:**
- Produces:

```go
package ide
type Lock struct {
	PID              int      `json:"pid"`
	Port             int      `json:"port"`
	Token            string   `json:"token"`
	WorkspaceFolders []string `json:"workspaceFolders"`
	IDEName          string   `json:"ideName"`
	Version          string   `json:"version"`
	path             string
}
func LockDir() (string, error)                              // ~/.be-code/ide (created)
func Discover(dir, workspace string) (*Lock, error)         // nil,nil when none
func Connect(ctx context.Context, l *Lock) (*Session, error)
type Session struct { Client *mcp.Client; Lock *Lock }
func (s *Session) Close()
```

- [ ] **Step 1: Write the failing tests**

```go
package ide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeLock stores a lock as <pid>-<port>.json (Discover reads every
// *.json, so two test locks may share a pid).
func writeLock(t *testing.T, dir string, l Lock, mtime time.Time) string {
	t.Helper()
	b, _ := json.Marshal(l)
	p := filepath.Join(dir, strconv.Itoa(l.PID)+"-"+strconv.Itoa(l.Port)+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	return p
}

// Picks the lock whose workspace folder contains the workspace; ignores
// and deletes locks whose process is gone.
func TestDiscoverPrefersMatchingWorkspaceAndCleansStale(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	now := time.Now()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", WorkspaceFolders: []string{"/tmp/other"}}, now)
	want := writeLock(t, dir, Lock{PID: me, Port: 2, Token: "b", WorkspaceFolders: []string{"/tmp/proj"}}, now.Add(-time.Hour))
	stale := writeLock(t, dir, Lock{PID: 999999999, Port: 3, Token: "c", WorkspaceFolders: []string{"/tmp/proj"}}, now)
	l, err := Discover(dir, "/tmp/proj/sub")
	if err != nil || l == nil || l.Port != 2 {
		t.Fatalf("got %+v err=%v", l, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale lock not removed")
	}
	_ = want
}

func TestDiscoverFallsBackToNewest(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", WorkspaceFolders: []string{"/a"}}, time.Now().Add(-time.Hour))
	writeLock(t, dir, Lock{PID: me, Port: 2, Token: "b", WorkspaceFolders: []string{"/b"}}, time.Now())
	l, err := Discover(dir, "/elsewhere")
	if err != nil || l == nil || l.Port != 2 {
		t.Fatalf("got %+v err=%v", l, err)
	}
}

func TestDiscoverNoneIsNilNil(t *testing.T) {
	l, err := Discover(t.TempDir(), "/x")
	if err != nil || l != nil {
		t.Fatalf("got %+v err=%v", l, err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/ide`; expected `undefined: Discover`.

- [ ] **Step 3: Implement**

`internal/ide/pid_unix.go`:

```go
//go:build !windows

package ide

import "syscall"

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil || syscall.Kill(pid, 0) == syscall.EPERM }
```

`internal/ide/pid_windows.go`:

```go
//go:build windows

package ide

import "os"

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil
}
```

`internal/ide/ide.go`:

```go
// Package ide connects BE-Code to an editor bridge (the VS Code extension):
// a local MCP server advertised by a lock file, offering ide_* tools,
// editor context, and in-editor review of file changes.
package ide

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/mcp"
)

type Lock struct {
	PID              int      `json:"pid"`
	Port             int      `json:"port"`
	Token            string   `json:"token"`
	WorkspaceFolders []string `json:"workspaceFolders"`
	IDEName          string   `json:"ideName"`
	Version          string   `json:"version"`
	path             string
	mtime            int64
}

// LockDir is ~/.be-code/ide, created on demand.
func LockDir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "ide")
	return d, os.MkdirAll(d, 0o700)
}

// Discover picks the live lock whose workspace folder contains workspace,
// else the newest live lock. Locks of dead processes are deleted.
// Returns nil, nil when no editor is listening.
func Discover(dir, workspace string) (*Lock, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ws := filepath.Clean(workspace)
	var live []*Lock
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var l Lock
		if json.Unmarshal(data, &l) != nil || l.Port == 0 {
			continue
		}
		if !processAlive(l.PID) {
			_ = os.Remove(p)
			continue
		}
		if info, err := e.Info(); err == nil {
			l.mtime = info.ModTime().UnixNano()
		}
		l.path = p
		live = append(live, &l)
	}
	if len(live) == 0 {
		return nil, nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].mtime > live[j].mtime })
	for _, l := range live {
		for _, f := range l.WorkspaceFolders {
			f = filepath.Clean(f)
			if ws == f || strings.HasPrefix(ws, f+string(filepath.Separator)) {
				return l, nil
			}
		}
	}
	return live[0], nil
}

// Session is a live connection to the editor bridge.
type Session struct {
	Client *mcp.Client
	Lock   *Lock
}

// Connect dials the lock's server with its token.
func Connect(ctx context.Context, l *Lock) (*Session, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port))
	name := l.IDEName
	if name == "" {
		name = "ide"
	}
	c, err := mcp.DialTCP(ctx, name, addr, l.Token)
	if err != nil {
		return nil, fmt.Errorf("ide: %w", err)
	}
	return &Session{Client: c, Lock: l}, nil
}

func (s *Session) Close() {
	if s != nil && s.Client != nil {
		s.Client.Close()
	}
}
```

- [ ] **Step 4: Run tests** `go test ./internal/ide -v`; expected PASS.
- [ ] **Step 5: Verify** `make -f build.mk verify`.

---

### Task 4: Editor context note and IDE guidance in the agent

**Files:**
- Modify: `internal/ide/ide.go` (add `Context`, `ContextNote`, `Session.ContextNote`)
- Modify: `internal/agent/loop.go` (fields `ContextProvider`, `Guidance`; `run()`; `composeSystem`)
- Create: `internal/agent/ide_guidance.go`
- Test: `internal/ide/context_test.go`, `internal/agent/context_note_test.go`

**Interfaces:**
- Produces:

```go
// ide
type Context struct {
	File      string   `json:"file"`       // workspace-relative, "" when none
	Line      int      `json:"line"`       // 1-based cursor line
	SelStart  int      `json:"selStart"`   // 1-based, 0 when no selection
	SelEnd    int      `json:"selEnd"`
	Selection string   `json:"selection"`  // text, already capped by the extension
	Open      []string `json:"open"`
}
func ContextNote(c Context) string                       // "" when File == ""
func (s *Session) ContextNote(ctx context.Context) string // calls ide_context; "" on any error
// agent
type Agent struct { ...; ContextProvider func(ctx context.Context) string; Guidance string }
const IDEGuidance = "..."
```

- [ ] **Step 1: Write the failing tests**

`internal/ide/context_test.go`:

```go
package ide

import (
	"strings"
	"testing"
)

func TestContextNoteFormats(t *testing.T) {
	n := ContextNote(Context{File: "internal/agent/loop.go", Line: 214, SelStart: 210, SelEnd: 231, Selection: "func x() {}"})
	if !strings.HasPrefix(n, "[editor: internal/agent/loop.go, cursor line 214, selection lines 210–231]") {
		t.Fatalf("note = %q", n)
	}
	if !strings.Contains(n, "func x() {}") {
		t.Fatal("selection text missing")
	}
	if ContextNote(Context{File: "a.go", Line: 3}) != "[editor: a.go, cursor line 3]" {
		t.Fatalf("no-selection note = %q", ContextNote(Context{File: "a.go", Line: 3}))
	}
	if ContextNote(Context{}) != "" {
		t.Fatal("no active file must produce no note")
	}
	long := strings.Repeat("x", 3000)
	if n := ContextNote(Context{File: "a.go", Line: 1, SelStart: 1, SelEnd: 9, Selection: long}); strings.Contains(n, long) {
		t.Fatal("selection over 2KB must be omitted from the note")
	}
}
```

`internal/agent/context_note_test.go`:

```go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// The context provider's note is prepended to a new user request, shown as
// a notice, and NOT added to repair prompts (run with newTurn=false).
func TestContextProviderPrependsNoteOnNewTurnsOnly(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.ContextProvider = func(context.Context) string { return "[editor: a.go, cursor line 3]" }
	notes := collectNotices(ag)
	ag.Run(context.Background(), "explain this")
	first := p.reqs[0].Messages[len(p.reqs[0].Messages)-1]
	if !strings.HasPrefix(first.Content, "[editor: a.go, cursor line 3]\n\nexplain this") {
		t.Fatalf("note not prepended: %q", first.Content)
	}
	if !hasNotice(*notes, "editor:") {
		t.Fatalf("no notice: %v", *notes)
	}
	ag.run(context.Background(), "fix the failing check", false)
	last := p.reqs[1].Messages[len(p.reqs[1].Messages)-1]
	if strings.Contains(last.Content, "[editor:") {
		t.Fatal("note must not be added to repair prompts")
	}
}

func TestGuidanceAppendedToSystemPrompt(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Guidance = IDEGuidance
	if !strings.Contains(ag.composeSystem(""), "ide_diagnostics") {
		t.Fatal("guidance missing from system prompt")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/ide ./internal/agent -run 'TestContext|TestGuidance'`; expected undefined symbols.

- [ ] **Step 3: Implement**

Append to `internal/ide/ide.go`:

```go
// Context is what the editor reports about the user's focus.
type Context struct {
	File      string   `json:"file"`
	Line      int      `json:"line"`
	SelStart  int      `json:"selStart"`
	SelEnd    int      `json:"selEnd"`
	Selection string   `json:"selection"`
	Open      []string `json:"open"`
}

const maxSelectionNote = 2048

// ContextNote renders the one-line note prepended to a prompt, plus the
// selected text when present and small. Empty when nothing is active.
func ContextNote(c Context) string {
	if c.File == "" {
		return ""
	}
	n := fmt.Sprintf("[editor: %s, cursor line %d", c.File, c.Line)
	if c.SelStart > 0 && c.SelEnd >= c.SelStart {
		n += fmt.Sprintf(", selection lines %d–%d", c.SelStart, c.SelEnd)
	}
	n += "]"
	if sel := strings.TrimRight(c.Selection, "\n"); sel != "" && len(sel) <= maxSelectionNote {
		n += "\n" + sel
	}
	return n
}

// ContextNote asks the editor for the current focus; any failure yields "".
func (s *Session) ContextNote(ctx context.Context) string {
	out, isErr, err := s.Client.CallTool(ctx, "context", json.RawMessage(`{}`))
	if err != nil || isErr {
		return ""
	}
	var c Context
	if json.Unmarshal([]byte(out), &c) != nil {
		return ""
	}
	return ContextNote(c)
}
```

(The extension's tool is named `context`; BE-Code registers it as `ide_context`, but `CallTool` uses the server's own name.)

`internal/agent/ide_guidance.go`:

```go
package agent

// IDEGuidance is appended to the system prompt when editor tools are
// attached, so a small model reaches for the language server and debugger
// instead of grepping and guessing.
const IDEGuidance = `Editor tools are available (ide_*). Prefer ide_diagnostics over running a build to find errors. Before guessing at an API, use ide_definition, ide_references or ide_hover on the symbol. To check runtime behaviour, use the debugger (ide_debug_start with a launch config or program, ide_debug_breakpoint, ide_debug_step, ide_debug_variables, ide_debug_evaluate) instead of adding print statements. A message starting with [editor: ...] tells you which file and lines the user is looking at.`
```

In `internal/agent/loop.go`, add to `Agent`:

```go
	// ContextProvider, when set, returns a short note about what the user
	// is looking at in their editor; it is prepended to each new request.
	ContextProvider func(ctx context.Context) string
	// Guidance is extra system-prompt text (editor tools, etc.).
	Guidance string
```

In `run()`, after `expanded := ExpandMentions(a.Tools.Root, userInput)`:

```go
	if newTurn && a.ContextProvider != nil {
		if note := a.ContextProvider(ctx); note != "" {
			a.notice("%s", strings.SplitN(note, "\n", 2)[0])
			expanded = note + "\n\n" + expanded
		}
	}
```

In `composeSystem`, before the git info block:

```go
	if a.Guidance != "" {
		sys += "\n\n" + a.Guidance
	}
```

- [ ] **Step 4: Run tests** `go test ./internal/ide ./internal/agent`; expected PASS.
- [ ] **Step 5: Verify** `make -f build.mk verify`.

---

### Task 5: Editor-side review of file changes

**Files:**
- Modify: `internal/tools/tool.go` (Registry fields `ReviewWrite`, `OnStatus`), `internal/tools/fs.go` (`approveWrite`)
- Modify: `internal/ide/ide.go` (`Decision`, `Session.ReviewWrite`)
- Modify: `internal/tui/tui.go` (`statusMsg`, `⌘ ide` marker), `internal/tui/palette.go` (menu status row)
- Test: `internal/tools/review_test.go`, `internal/ide/review_test.go`, `internal/tui/ide_test.go`

**Interfaces:**
- Produces:

```go
// tools
type ReviewDecision int
const (
	ReviewUnavailable ReviewDecision = iota // fall back to Approve
	ReviewAccept
	ReviewReject
	ReviewAcceptAll
)
type Registry struct { ...; ReviewWrite func(rel, oldContent, newContent string) ReviewDecision; OnStatus func(string) }
// ide
func (s *Session) ReviewWrite(rel, oldContent, newContent string) tools.ReviewDecision
// agent
type Agent struct { ...; IDEName string } // "" when no editor is connected
// tui
type statusMsg string // sets the bottom-line state without a transcript line
```

- [ ] **Step 1: Write the failing tests**

`internal/tools/review_test.go`:

```go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestReviewWriteDecisions(t *testing.T) {
	dir := t.TempDir()
	prompted := 0
	reg, _ := NewRegistry(dir, func(a, d string) bool { prompted++; return true })
	reg.ApproveWrites = true
	var statuses []string
	reg.OnStatus = func(s string) { statuses = append(statuses, s) }
	write := func() Result {
		return reg.Dispatch(context.Background(), provider.ToolCall{Name: "write_file", Arguments: `{"path":"a.txt","content":"v"}`})
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewReject }
	if res := write(); !res.IsError {
		t.Fatal("reject not honoured")
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewAccept }
	if res := write(); res.IsError {
		t.Fatal(res.Content)
	}
	if prompted != 0 {
		t.Fatalf("TUI approver called %d times although the editor decided", prompted)
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewUnavailable }
	write()
	if prompted != 1 {
		t.Fatal("fallback to Approve did not happen")
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewAcceptAll }
	write()
	if reg.ApproveWrites {
		t.Fatal("accept-all must stop asking for the session")
	}
	if len(statuses) == 0 || statuses[0] != "reviewing change in VS Code…" {
		t.Fatalf("status not reported: %v", statuses)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "v" {
		t.Fatal("file not written after accept")
	}
}
```

`internal/ide/review_test.go` (uses a fake TCP server as in Task 1 whose `tools/call` for `review_diff` returns `{"decision":"accept_all"}`; copy the helper, parameterize the reply):

```go
func TestSessionReviewWriteMapsDecision(t *testing.T) {
	addr := fakeTCPServerReplying(t, "tok", `{"decision":"accept_all"}`) // helper: like Task 1's server, returns this text for tools/call
	s, err := Connect(context.Background(), &Lock{Port: portOf(addr), Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d := s.ReviewWrite("a.go", "old", "new"); d != tools.ReviewAcceptAll {
		t.Fatalf("decision = %v", d)
	}
}
```

`internal/tui/ide_test.go`:

```go
func TestIDEMarkerAndStatusMsg(t *testing.T) {
	m := newTestModel(t)
	m.ag.IDEName = "vscode"
	if !strings.Contains(m.View(), "⌘ ide") {
		t.Fatal("ide marker missing from bottom line")
	}
	m.mode = modeBusy
	m.running = true
	m.Update(statusMsg("reviewing change in VS Code…"))
	if !strings.Contains(m.View(), "reviewing change in VS Code") {
		t.Fatal("status not shown")
	}
}
```

- [ ] **Step 2: Run to verify failure** — expected undefined `ReviewDecision`, `ReviewWrite`, `statusMsg`, `IDEName`.

- [ ] **Step 3: Implement**

`internal/tools/tool.go` additions:

```go
// ReviewDecision is the outcome of an editor-side review of a file change.
type ReviewDecision int

const (
	ReviewUnavailable ReviewDecision = iota // editor could not review: fall back to Approve
	ReviewAccept
	ReviewReject
	ReviewAcceptAll // accept and stop asking for the rest of the session
)
```

and in `Registry`:

```go
	// ReviewWrite, when set, is asked first for file changes (an editor
	// diff review). ReviewUnavailable falls back to Approve.
	ReviewWrite func(rel, oldContent, newContent string) ReviewDecision
	// OnStatus receives short progress notes for the UI's status line.
	OnStatus func(msg string)
```

`internal/tools/fs.go` `approveWrite`:

```go
func (r *Registry) approveWrite(absPath, newContent string) (Result, bool) {
	if !r.ApproveWrites || r.Approve == nil {
		return Result{}, true
	}
	oldContent := ""
	if data, err := os.ReadFile(absPath); err == nil {
		oldContent = string(data)
	}
	rel, _ := filepath.Rel(r.Root, absPath)
	rejected := Result{IsError: true,
		Content: "user rejected this file change; ask what they want instead or take a different approach"}
	if r.ReviewWrite != nil {
		if r.OnStatus != nil {
			r.OnStatus("reviewing change in VS Code…")
		}
		d := r.ReviewWrite(rel, oldContent, newContent)
		if r.OnStatus != nil {
			r.OnStatus("")
		}
		switch d {
		case ReviewAccept:
			return Result{}, true
		case ReviewAcceptAll:
			r.ApproveWrites = false
			return Result{}, true
		case ReviewReject:
			return rejected, false
		}
		// ReviewUnavailable: fall through to the terminal prompt.
	}
	preview := diff.Preview(rel, oldContent, newContent, false)
	if r.Approve("file_write", preview) {
		return Result{}, true
	}
	return rejected, false
}
```

`internal/ide/ide.go` addition (import `internal/tools`):

```go
// ReviewWrite shows the change in the editor and returns the decision;
// any transport or protocol problem yields ReviewUnavailable so the TUI
// prompt takes over.
func (s *Session) ReviewWrite(rel, oldContent, newContent string) tools.ReviewDecision {
	args, _ := json.Marshal(map[string]string{"path": rel, "original": oldContent, "proposed": newContent,
		"summary": fmt.Sprintf("BE-Code wants to change %s", rel)})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, isErr, err := s.Client.CallTool(ctx, "review_diff", args)
	if err != nil || isErr {
		return tools.ReviewUnavailable
	}
	var r struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return tools.ReviewUnavailable
	}
	switch r.Decision {
	case "accept":
		return tools.ReviewAccept
	case "accept_all":
		return tools.ReviewAcceptAll
	case "reject":
		return tools.ReviewReject
	}
	return tools.ReviewUnavailable
}
```

Add `"time"` to the ide imports. Check for an import cycle: `tools` must not import `ide` (it doesn't). `agent` gets `IDEName string` on `Agent`.

`internal/tui/tui.go`: add `type statusMsg string`; in `Update`: `case statusMsg: m.statusNote = string(msg); if m.statusNote == "" && m.running { m.statusNote = "thinking" }`; in `New`, after `ag.Events = ...`: `ag.Tools.OnStatus = func(s string) { m.send(statusMsg(s)) }`. In `bottomLine`, after the model segment: `if m.ag.IDEName != "" { line += stAccent.Render(" ⌘ ide") }`. In `menuStatus`, add a row `{"editor", m.ag.IDEName}` when non-empty.

- [ ] **Step 4: Run tests** `go test ./internal/tools ./internal/ide ./internal/tui`; expected PASS.
- [ ] **Step 5: Verify** `make -f build.mk verify`.

---

### Task 6: Startup wiring, config, flags, docs

**Files:**
- Modify: `internal/config/config.go` (IDEConfig), `cmd/root.go` (flags, `attachIDE`), `cmd/commands.go` (`doctor` line; headless never sets `ReviewWrite`)
- Modify: `README.md`, `CHANGELOG.md`, root `CLAUDE.md`
- Test: `internal/config/config_test.go` (defaults)

**Interfaces:**
- Produces: `config.IDEConfig{Enabled bool; AutoContext bool}` (json `ide.enabled`, `ide.auto_context`, both default true); flags `--ide` (force discovery) and `--no-ide`; `attachIDE(cfg, reg, ag) *ide.Session`.

- [ ] **Step 1: Write the failing test**

```go
package config

import "testing"

func TestIDEDefaults(t *testing.T) {
	c := Default()
	if !c.IDE.Enabled || !c.IDE.AutoContext {
		t.Fatalf("ide defaults = %+v", c.IDE)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/config`; expected undefined `IDE`.

- [ ] **Step 3: Implement**

`config.go`:

```go
	// IDE controls the editor bridge (VS Code extension): discovery of the
	// lock file when running inside the editor's terminal, and whether a
	// note about the active file/selection is added to each prompt.
	IDE IDEConfig `json:"ide"`
```

```go
type IDEConfig struct {
	Enabled     bool `json:"enabled"`
	AutoContext bool `json:"auto_context"`
}
```

Default: `IDE: IDEConfig{Enabled: true, AutoContext: true}`.

`cmd/root.go`: flags

```go
	rootCmd.PersistentFlags().BoolVar(&flagIDE, "ide", false, "connect to the editor bridge even outside an editor terminal")
	rootCmd.PersistentFlags().BoolVar(&flagNoIDE, "no-ide", false, "never connect to the editor bridge")
```

and after the MCP servers block in `buildAgent` (before `agent.New` is fine for tools, but `ag` is needed for guidance/context, so place it right after `ag := agent.New(...)`):

```go
	if sess := attachIDE(cfg, reg, ag); sess != nil {
		ideSession = sess // package var; closed in runInteractive/run defers
	}
```

```go
var ideSession *ide.Session

// attachIDE connects to an editor bridge when one is advertised and
// wanted, registers its tools as ide_*, and wires context and review.
func attachIDE(cfg *config.Config, reg *tools.Registry, ag *agent.Agent) *ide.Session {
	if flagNoIDE || !cfg.IDE.Enabled {
		return nil
	}
	if !flagIDE && os.Getenv("TERM_PROGRAM") != "vscode" {
		return nil
	}
	dir, err := ide.LockDir()
	if err != nil {
		return nil
	}
	lock, err := ide.Discover(dir, reg.Root)
	if err != nil || lock == nil {
		if flagIDE {
			fmt.Fprintln(os.Stderr, "warn: --ide given but no editor bridge is listening (is the BE-Code extension installed and active?)")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := ide.Connect(ctx, lock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: editor bridge at port %d: %v\n", lock.Port, err)
		return nil
	}
	names := reg.AttachMCPPrefixed(sess.Client, "ide_")
	ag.Guidance = agent.IDEGuidance
	ag.IDEName = lock.IDEName
	if ag.IDEName == "" {
		ag.IDEName = "ide"
	}
	if cfg.IDE.AutoContext {
		ag.ContextProvider = sess.ContextNote
	}
	ag.RefreshSystem() // the system prompt was composed before these tools and guidance existed
	fmt.Fprintf(os.Stderr, "VS Code connected: %d tools\n", len(names))
	return sess
}
```

Note on the system prompt: `agent.New` composes the system prompt before IDE tools are attached and before `Guidance` is set. Add to `agent`:

```go
// RefreshSystem recomposes the system prompt after tools or guidance
// changed (used once at startup when the editor bridge attaches).
func (a *Agent) RefreshSystem() {
	a.knownTools = map[string]bool{}
	for _, n := range a.Tools.Names() {
		a.knownTools[n] = true
	}
	a.History.System.Content = a.composeSystem("")
}
```

(`attachIDE` above already calls it.)

Review routing: in `runInteractive` (TUI/REPL), after `buildAgent`: `if ideSession != nil { ag.Tools.ReviewWrite = ideSession.ReviewWrite; defer ideSession.Close() }`. In the headless `run` command: `if ideSession != nil { defer ideSession.Close() }` and do **not** set `ReviewWrite`.

`doctor`: after the web search line: 

```go
		if dir, err := ide.LockDir(); err == nil {
			if lock, _ := ide.Discover(dir, mustAbs(flagDir)); lock != nil {
				fmt.Printf("editor bridge: %s v%s on port %d (workspace %s)\n", lock.IDEName, lock.Version, lock.Port, strings.Join(lock.WorkspaceFolders, ", "))
			} else {
				fmt.Println("editor bridge: none listening (install the BE-Code VS Code extension)")
			}
		}
```

Docs: README section "VS Code" (install the extension from `dist/be-code-<ver>.vsix` or `vscode/`, run `be-code` in the integrated terminal, what the agent gains, `ide.*` config, `--ide/--no-ide`); CHANGELOG bullet under the next version; root `CLAUDE.md` paragraph describing `internal/ide`, the `ide_` prefix, `ReviewWrite`, and `ContextProvider`.

- [ ] **Step 4: Run** `make -f build.mk verify` and `sh test/e2e/run_e2e.sh`; expected all PASS (e2e runs outside VS Code, so no discovery happens; if your shell has `TERM_PROGRAM=vscode`, run e2e with `--no-ide` added to its be-code invocation or `env -u TERM_PROGRAM`).

---

### Task 7: Extension scaffold and pure protocol library

**Files:**
- Create: `vscode/package.json`, `vscode/tsconfig.json`, `vscode/esbuild.mjs`, `vscode/.vscodeignore`, `vscode/src/lib/framing.ts`, `vscode/src/lib/lock.ts`
- Test: `vscode/test/framing.test.ts`, `vscode/test/lock.test.ts`

**Interfaces:**
- Produces:

```ts
// src/lib/framing.ts
export class LineFramer { push(chunk: Buffer): string[] }   // returns complete lines (without "\n"), keeps partial
export function encode(msg: unknown): string                  // JSON + "\n"
// src/lib/lock.ts
export interface LockInfo { pid: number; port: number; token: string; workspaceFolders: string[]; ideName: string; version: string }
export function lockDir(home: string): string                  // <home>/.be-code/ide
export async function writeLock(home: string, info: LockInfo): Promise<string>  // returns path
export async function removeLock(home: string, pid: number): Promise<void>
```

- [ ] **Step 1: Scaffold** `vscode/package.json`:

```json
{
  "name": "be-code",
  "displayName": "BE-Code",
  "description": "Editor bridge for BE-Code: diagnostics, symbols, debugger and diff review for the local coding agent.",
  "version": "1.0.0",
  "publisher": "be-ai-research",
  "engines": { "vscode": "^1.90.0" },
  "categories": ["Other"],
  "activationEvents": ["onStartupFinished"],
  "main": "./dist/extension.js",
  "contributes": {
    "commands": [
      { "command": "be-code.openTerminal", "title": "BE-Code: Open terminal" },
      { "command": "be-code.status", "title": "BE-Code: Show connection status" }
    ],
    "configuration": {
      "title": "BE-Code",
      "properties": {
        "be-code.port": { "type": "number", "default": 0, "description": "Port for the editor bridge (0 = random)." },
        "be-code.autoStart": { "type": "boolean", "default": true, "description": "Start the bridge when VS Code starts." }
      }
    }
  },
  "scripts": {
    "build": "node esbuild.mjs",
    "watch": "node esbuild.mjs --watch",
    "test": "vitest run",
    "package": "npm run build && vsce package --no-dependencies -o ../dist/be-code-1.0.0.vsix"
  },
  "devDependencies": {
    "@types/node": "^22.0.0",
    "@types/vscode": "^1.90.0",
    "@vscode/vsce": "^3.0.0",
    "esbuild": "^0.23.0",
    "typescript": "^5.5.0",
    "vitest": "^2.0.0"
  }
}
```

`tsconfig.json`: `{"compilerOptions":{"target":"ES2022","module":"Node16","moduleResolution":"Node16","strict":true,"esModuleInterop":true,"skipLibCheck":true,"outDir":"dist","rootDir":"."},"include":["src","test"]}`.

`esbuild.mjs`:

```js
import { build, context } from "esbuild";
const opts = { entryPoints: ["src/extension.ts"], bundle: true, outfile: "dist/extension.js", external: ["vscode"], format: "cjs", platform: "node", target: "node20", sourcemap: true };
if (process.argv.includes("--watch")) { (await context(opts)).watch(); } else { await build(opts); }
```

`.vscodeignore`: `src/**`, `test/**`, `node_modules/**`, `esbuild.mjs`, `tsconfig.json`, `**/*.map`.

Run `cd vscode && npm install`.

- [ ] **Step 2: Write the failing tests**

`test/framing.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { LineFramer, encode } from "../src/lib/framing";

describe("LineFramer", () => {
  it("splits complete lines and keeps the partial tail", () => {
    const f = new LineFramer();
    expect(f.push(Buffer.from('{"a":1}\n{"b":'))).toEqual(['{"a":1}']);
    expect(f.push(Buffer.from('2}\n'))).toEqual(['{"b":2}']);
  });
  it("handles multiple lines in one chunk and CRLF", () => {
    const f = new LineFramer();
    expect(f.push(Buffer.from("x\r\ny\n"))).toEqual(["x", "y"]);
  });
});

describe("encode", () => {
  it("appends a newline", () => {
    expect(encode({ jsonrpc: "2.0", id: 1, result: {} })).toBe('{"jsonrpc":"2.0","id":1,"result":{}}\n');
  });
});
```

`test/lock.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { mkdtempSync, readFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { lockDir, writeLock, removeLock } from "../src/lib/lock";

describe("lock file", () => {
  it("writes and removes ~/.be-code/ide/<pid>.json", async () => {
    const home = mkdtempSync(join(tmpdir(), "bec-"));
    const p = await writeLock(home, { pid: 4242, port: 5000, token: "t", workspaceFolders: ["/w"], ideName: "vscode", version: "1.0.0" });
    expect(p).toBe(join(lockDir(home), "4242.json"));
    expect(JSON.parse(readFileSync(p, "utf8")).port).toBe(5000);
    await removeLock(home, 4242);
    expect(existsSync(p)).toBe(false);
  });
});
```

- [ ] **Step 3: Run** `npm test`; expected failures: modules not found.

- [ ] **Step 4: Implement**

`src/lib/framing.ts`:

```ts
export class LineFramer {
  private buf = "";
  push(chunk: Buffer): string[] {
    this.buf += chunk.toString("utf8");
    const lines: string[] = [];
    let i: number;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      lines.push(this.buf.slice(0, i).replace(/\r$/, ""));
      this.buf = this.buf.slice(i + 1);
    }
    return lines;
  }
}

export function encode(msg: unknown): string {
  return JSON.stringify(msg) + "\n";
}
```

`src/lib/lock.ts`:

```ts
import { promises as fs } from "node:fs";
import { join } from "node:path";

export interface LockInfo {
  pid: number; port: number; token: string; workspaceFolders: string[]; ideName: string; version: string;
}

export function lockDir(home: string): string {
  return join(home, ".be-code", "ide");
}

export async function writeLock(home: string, info: LockInfo): Promise<string> {
  const dir = lockDir(home);
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  const p = join(dir, `${info.pid}.json`);
  await fs.writeFile(p, JSON.stringify(info, null, 1), { mode: 0o600 });
  return p;
}

export async function removeLock(home: string, pid: number): Promise<void> {
  await fs.rm(join(lockDir(home), `${pid}.json`), { force: true });
}
```

- [ ] **Step 5: Run** `npm test`; expected PASS.

---

### Task 8: MCP server, tool registry, and extension activation

**Files:**
- Create: `vscode/src/server.ts`, `vscode/src/tools/registry.ts`, `vscode/src/extension.ts`
- Test: `vscode/test/server.test.ts`

**Interfaces:**
- Produces:

```ts
// src/tools/registry.ts
export interface ToolDef { name: string; description: string; inputSchema: object; handler: (args: any) => Promise<string> }
export class ToolRegistry { add(t: ToolDef): void; list(): {name:string;description:string;inputSchema:object}[]; call(name: string, args: any): Promise<{ text: string; isError: boolean }> }
// src/server.ts
export class BridgeServer { constructor(tools: ToolRegistry, token: string); listen(port: number): Promise<number>; close(): Promise<void>; readonly connections: number; onConnectionChange?: () => void }
```

- [ ] **Step 1: Write the failing test** (`test/server.test.ts`) — a client that speaks newline JSON-RPC:

```ts
import { describe, it, expect } from "vitest";
import { createConnection } from "node:net";
import { BridgeServer } from "../src/server";
import { ToolRegistry } from "../src/tools/registry";

function rpc(port: number, msgs: object[]): Promise<any[]> {
  return new Promise((resolve, reject) => {
    const out: any[] = [];
    const c = createConnection({ host: "127.0.0.1", port }, () => {
      for (const m of msgs) c.write(JSON.stringify(m) + "\n");
    });
    let buf = "";
    c.on("data", (d) => {
      buf += d.toString();
      let i: number;
      while ((i = buf.indexOf("\n")) >= 0) { out.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1); }
      if (out.length >= msgs.filter((m: any) => m.id !== undefined).length) { c.end(); resolve(out); }
    });
    c.on("error", reject);
    c.on("close", () => resolve(out));
  });
}

describe("BridgeServer", () => {
  it("handshakes with the right token, lists and calls tools", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "echo", description: "echo", inputSchema: { type: "object" }, handler: async (a) => `hi ${a.who}` });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } },
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", id: 2, method: "tools/list" },
      { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "echo", arguments: { who: "bob" } } },
    ]);
    expect(res[0].result.serverInfo.name).toBe("be-code-vscode");
    expect(res[1].result.tools[0].name).toBe("echo");
    expect(res[2].result.content[0].text).toBe("hi bob");
    await s.close();
  });
  it("rejects a bad token with -32001 and closes", async () => {
    const s = new BridgeServer(new ToolRegistry(), "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [{ jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "nope" } } }]);
    expect(res[0].error.code).toBe(-32001);
    await s.close();
  });
  it("reports tool exceptions as isError results", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "boom", description: "", inputSchema: { type: "object" }, handler: async () => { throw new Error("bad"); } });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } },
      { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "boom", arguments: {} } },
    ]);
    expect(res[1].result.isError).toBe(true);
    expect(res[1].result.content[0].text).toContain("bad");
    await s.close();
  });
});
```

- [ ] **Step 2: Run** `npm test`; expected module-not-found failures.

- [ ] **Step 3: Implement**

`src/tools/registry.ts`:

```ts
export interface ToolDef {
  name: string;
  description: string;
  inputSchema: object;
  handler: (args: any) => Promise<string>;
}

export class ToolRegistry {
  private tools = new Map<string, ToolDef>();
  add(t: ToolDef): void { this.tools.set(t.name, t); }
  list() { return [...this.tools.values()].map(({ name, description, inputSchema }) => ({ name, description, inputSchema })); }
  async call(name: string, args: any): Promise<{ text: string; isError: boolean }> {
    const t = this.tools.get(name);
    if (!t) return { text: `unknown tool ${name}`, isError: true };
    try {
      return { text: await t.handler(args ?? {}), isError: false };
    } catch (e: any) {
      return { text: `${name}: ${e?.message ?? String(e)}`, isError: true };
    }
  }
}
```

`src/server.ts`:

```ts
import { createServer, Server, Socket } from "node:net";
import { LineFramer, encode } from "./lib/framing";
import { ToolRegistry } from "./tools/registry";

export class BridgeServer {
  private srv?: Server;
  private socks = new Set<Socket>();
  onConnectionChange?: () => void;

  constructor(private tools: ToolRegistry, private token: string) {}

  get connections(): number { return this.socks.size; }

  listen(port: number): Promise<number> {
    return new Promise((resolve, reject) => {
      this.srv = createServer((sock) => this.handle(sock));
      this.srv.on("error", reject);
      this.srv.listen(port, "127.0.0.1", () => {
        const addr = this.srv!.address();
        resolve(typeof addr === "object" && addr ? addr.port : port);
      });
    });
  }

  async close(): Promise<void> {
    for (const s of this.socks) s.destroy();
    this.socks.clear();
    await new Promise<void>((r) => (this.srv ? this.srv.close(() => r()) : r()));
  }

  private handle(sock: Socket) {
    const framer = new LineFramer();
    let authed = false;
    this.socks.add(sock);
    this.onConnectionChange?.();
    sock.on("close", () => { this.socks.delete(sock); this.onConnectionChange?.(); });
    sock.on("error", () => {});
    sock.on("data", async (chunk) => {
      for (const line of framer.push(chunk)) {
        let req: any;
        try { req = JSON.parse(line); } catch { continue; }
        if (req.id === undefined) continue; // notification
        const reply = (result: unknown) => sock.write(encode({ jsonrpc: "2.0", id: req.id, result }));
        const fail = (code: number, message: string) => sock.write(encode({ jsonrpc: "2.0", id: req.id, error: { code, message } }));
        switch (req.method) {
          case "initialize":
            if (req.params?.auth?.token !== this.token) { fail(-32001, "bad token"); sock.end(); return; }
            authed = true;
            reply({ protocolVersion: "2024-11-05", capabilities: { tools: {} }, serverInfo: { name: "be-code-vscode", version: "1.0.0" } });
            break;
          case "tools/list":
            if (!authed) { fail(-32001, "not authenticated"); break; }
            reply({ tools: this.tools.list() });
            break;
          case "tools/call": {
            if (!authed) { fail(-32001, "not authenticated"); break; }
            const { text, isError } = await this.tools.call(req.params?.name, req.params?.arguments);
            reply({ content: [{ type: "text", text }], isError });
            break;
          }
          case "ping":
            reply({});
            break;
          default:
            fail(-32601, `unknown method ${req.method}`);
        }
      }
    });
  }
}
```

`src/extension.ts`:

```ts
import * as vscode from "vscode";
import { randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { BridgeServer } from "./server";
import { ToolRegistry } from "./tools/registry";
import { writeLock, removeLock } from "./lib/lock";
import { registerEditorTools } from "./tools/editor";
import { registerDiagnosticsTool } from "./tools/diagnostics";
import { registerDebugTools } from "./tools/debug";
import { registerReviewTool } from "./tools/review";

let server: BridgeServer | undefined;
let status: vscode.StatusBarItem;

export async function activate(ctx: vscode.ExtensionContext) {
  status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  ctx.subscriptions.push(status);
  ctx.subscriptions.push(
    vscode.commands.registerCommand("be-code.openTerminal", () => {
      const cwd = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
      const t = vscode.window.createTerminal({ name: "BE-Code", cwd });
      t.sendText("be-code");
      t.show();
    }),
    vscode.commands.registerCommand("be-code.status", () => {
      vscode.window.showInformationMessage(server ? `BE-Code bridge listening; ${server.connections} connection(s)` : "BE-Code bridge is not running");
    }),
  );
  if (vscode.workspace.getConfiguration("be-code").get<boolean>("autoStart", true)) {
    await start(ctx);
  }
}

async function start(ctx: vscode.ExtensionContext) {
  const tools = new ToolRegistry();
  registerEditorTools(tools);
  registerDiagnosticsTool(tools);
  registerDebugTools(tools, ctx);
  registerReviewTool(tools, ctx);
  const token = randomBytes(24).toString("hex");
  server = new BridgeServer(tools, token);
  const port = await server.listen(vscode.workspace.getConfiguration("be-code").get<number>("port", 0));
  await writeLock(homedir(), {
    pid: process.pid, port, token,
    workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath),
    ideName: "vscode", version: ctx.extension.packageJSON.version,
  });
  const refresh = () => { status.text = server && server.connections > 0 ? "$(plug) BE-Code: connected" : "$(radio-tower) BE-Code: listening"; status.show(); };
  server.onConnectionChange = refresh;
  refresh();
}

export async function deactivate() {
  await server?.close();
  await removeLock(homedir(), process.pid);
}
```

Until Tasks 9–11 exist, create the four `register*` modules as empty functions so the extension builds; each later task replaces its stub.

- [ ] **Step 4: Run** `npm test` and `npm run build`; expected PASS and `dist/extension.js` produced.

---

### Task 9: Editor and diagnostics tools

**Files:**
- Create: `vscode/src/tools/editor.ts`, `vscode/src/tools/diagnostics.ts`, `vscode/src/lib/format.ts`, `vscode/src/lib/paths.ts`
- Test: `vscode/test/format.test.ts`

**Interfaces:**
- Produces tools `context`, `open`, `definition`, `references`, `hover`, `diagnostics` (BE-Code shows them as `ide_*`); pure helpers `formatDiagnostics(items: DiagItem[]): string` and `relPath(folders: string[], fsPath: string): string`.

- [ ] **Step 1: Write the failing test** (`test/format.test.ts`):

```ts
import { describe, it, expect } from "vitest";
import { formatDiagnostics } from "../src/lib/format";
import { relPath } from "../src/lib/paths";

describe("formatDiagnostics", () => {
  it("groups by file with a count summary first", () => {
    const out = formatDiagnostics([
      { path: "b.go", line: 3, col: 1, severity: "error", source: "go", message: "undefined: x" },
      { path: "a.py", line: 10, col: 5, severity: "warning", source: "Pylance", message: "unused" },
      { path: "b.go", line: 1, col: 1, severity: "error", source: "go", message: "missing import" },
    ]);
    expect(out.split("\n")[0]).toBe("2 errors, 1 warning in 2 files");
    expect(out).toContain("b.go:1:1 error go: missing import");
    expect(out.indexOf("a.py")).toBeLessThan(out.indexOf("b.go")); // files sorted
  });
  it("says so when clean", () => {
    expect(formatDiagnostics([])).toBe("no diagnostics");
  });
});

describe("relPath", () => {
  it("makes paths workspace-relative", () => {
    expect(relPath(["/w/proj"], "/w/proj/internal/x.go")).toBe("internal/x.go");
    expect(relPath(["/w/proj"], "/elsewhere/y.go")).toBe("/elsewhere/y.go");
  });
});
```

- [ ] **Step 2: Run** `npm test`; expected module-not-found.

- [ ] **Step 3: Implement**

`src/lib/paths.ts`:

```ts
import { relative, isAbsolute, resolve } from "node:path";

export function relPath(folders: string[], fsPath: string): string {
  for (const f of folders) {
    const r = relative(f, fsPath);
    if (r && !r.startsWith("..") && !isAbsolute(r)) return r.split("\\").join("/");
  }
  return fsPath;
}

export function absPath(folders: string[], p: string): string {
  if (isAbsolute(p)) return p;
  return resolve(folders[0] ?? process.cwd(), p);
}
```

`src/lib/format.ts`:

```ts
export interface DiagItem { path: string; line: number; col: number; severity: "error" | "warning" | "info" | "hint"; source: string; message: string }

export function formatDiagnostics(items: DiagItem[]): string {
  if (items.length === 0) return "no diagnostics";
  const byFile = new Map<string, DiagItem[]>();
  for (const d of items) byFile.set(d.path, [...(byFile.get(d.path) ?? []), d]);
  const errors = items.filter((d) => d.severity === "error").length;
  const warnings = items.filter((d) => d.severity === "warning").length;
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
  const lines = [`${plural(errors, "error")}, ${plural(warnings, "warning")} in ${byFile.size} file${byFile.size === 1 ? "" : "s"}`];
  for (const file of [...byFile.keys()].sort()) {
    for (const d of byFile.get(file)!.sort((a, b) => a.line - b.line)) {
      lines.push(`${d.path}:${d.line}:${d.col} ${d.severity} ${d.source || "-"}: ${d.message.split("\n")[0]}`);
    }
  }
  return lines.join("\n");
}
```

`src/tools/diagnostics.ts`:

```ts
import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { formatDiagnostics, DiagItem } from "../lib/format";
import { relPath } from "../lib/paths";

const sev = (s: vscode.DiagnosticSeverity): DiagItem["severity"] =>
  s === vscode.DiagnosticSeverity.Error ? "error" : s === vscode.DiagnosticSeverity.Warning ? "warning" : s === vscode.DiagnosticSeverity.Information ? "info" : "hint";

export function registerDiagnosticsTool(reg: ToolRegistry) {
  reg.add({
    name: "diagnostics",
    description: "Errors and warnings from the editor's language servers, as path:line:col severity source: message. Optional path filters to one file; severity: error, warning or all (default: errors and warnings).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, severity: { type: "string", enum: ["error", "warning", "all"] } } },
    handler: async (args) => {
      const folders = (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
      const want = args.severity === "all" ? ["error", "warning", "info", "hint"] : args.severity === "error" ? ["error"] : ["error", "warning"];
      const items: DiagItem[] = [];
      for (const [uri, diags] of vscode.languages.getDiagnostics()) {
        const p = relPath(folders, uri.fsPath);
        if (args.path && p !== args.path) continue;
        for (const d of diags) {
          const s = sev(d.severity);
          if (!want.includes(s)) continue;
          items.push({ path: p, line: d.range.start.line + 1, col: d.range.start.character + 1, severity: s, source: d.source ?? "", message: d.message });
        }
      }
      return formatDiagnostics(items);
    },
  });
}
```

`src/tools/editor.ts`:

```ts
import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { relPath, absPath } from "../lib/paths";

const folders = () => (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
const pos = (args: any) => new vscode.Position(Math.max(0, (args.line ?? 1) - 1), Math.max(0, (args.col ?? 1) - 1));
const uriOf = (p: string) => vscode.Uri.file(absPath(folders(), p));
const locLine = (l: vscode.Location | vscode.LocationLink) => {
  const uri = "targetUri" in l ? l.targetUri : l.uri;
  const range = "targetRange" in l ? l.targetRange : l.range;
  return `${relPath(folders(), uri.fsPath)}:${range.start.line + 1}:${range.start.character + 1}`;
};

export function registerEditorTools(reg: ToolRegistry) {
  reg.add({
    name: "context",
    description: "What the user is looking at: active file, cursor line, selection, open files, workspace folders (JSON).",
    inputSchema: { type: "object", properties: {} },
    handler: async () => {
      const ed = vscode.window.activeTextEditor;
      const open = vscode.workspace.textDocuments.filter((d) => d.uri.scheme === "file").map((d) => relPath(folders(), d.uri.fsPath));
      if (!ed || ed.document.uri.scheme !== "file") return JSON.stringify({ file: "", line: 0, selStart: 0, selEnd: 0, selection: "", open, workspaceFolders: folders() });
      const sel = ed.selection;
      const selection = sel.isEmpty ? "" : ed.document.getText(sel).slice(0, 2048);
      return JSON.stringify({
        file: relPath(folders(), ed.document.uri.fsPath), line: sel.active.line + 1,
        selStart: sel.isEmpty ? 0 : sel.start.line + 1, selEnd: sel.isEmpty ? 0 : sel.end.line + 1,
        selection, open, workspaceFolders: folders(),
      });
    },
  });
  reg.add({
    name: "open",
    description: "Reveal a file in the editor, optionally at a line.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" } }, required: ["path"] },
    handler: async (args) => {
      const doc = await vscode.workspace.openTextDocument(uriOf(args.path));
      const ed = await vscode.window.showTextDocument(doc, { preserveFocus: true });
      if (args.line) { const p = new vscode.Position(args.line - 1, 0); ed.revealRange(new vscode.Range(p, p), vscode.TextEditorRevealType.InCenter); ed.selection = new vscode.Selection(p, p); }
      return `opened ${args.path}${args.line ? ":" + args.line : ""}`;
    },
  });
  reg.add({
    name: "definition",
    description: "Where the symbol at path:line:col is defined (language server).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<(vscode.Location | vscode.LocationLink)[]>("vscode.executeDefinitionProvider", uriOf(args.path), pos(args))) ?? [];
      return res.length ? res.map(locLine).join("\n") : "no definition found (is the language server running and the file saved?)";
    },
  });
  reg.add({
    name: "references",
    description: "References to the symbol at path:line:col, as path:line: text.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" }, max: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<vscode.Location[]>("vscode.executeReferenceProvider", uriOf(args.path), pos(args))) ?? [];
      const max = args.max ?? 50;
      const lines: string[] = [];
      for (const l of res.slice(0, max)) {
        const doc = await vscode.workspace.openTextDocument(l.uri);
        lines.push(`${relPath(folders(), l.uri.fsPath)}:${l.range.start.line + 1}: ${doc.lineAt(l.range.start.line).text.trim()}`);
      }
      return lines.length ? `${res.length} reference(s)\n` + lines.join("\n") : "no references found";
    },
  });
  reg.add({
    name: "hover",
    description: "Type or signature information for the symbol at path:line:col (language server hover).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<vscode.Hover[]>("vscode.executeHoverProvider", uriOf(args.path), pos(args))) ?? [];
      const text = res.flatMap((h) => h.contents.map((c) => (typeof c === "string" ? c : "value" in c ? c.value : String(c)))).join("\n\n").trim();
      return text || "no hover information";
    },
  });
}
```

- [ ] **Step 4: Run** `npm test` and `npm run build`; expected PASS.

---

### Task 10: Debug tools

**Files:**
- Create: `vscode/src/lib/stopwaiter.ts`, `vscode/src/tools/debug.ts`
- Test: `vscode/test/stopwaiter.test.ts`

**Interfaces:**
- Produces tools `debug_configs`, `debug_start`, `debug_breakpoint`, `debug_continue`, `debug_step`, `debug_stack`, `debug_variables`, `debug_evaluate`, `debug_output`, `debug_stop`; pure `StopWaiter` (event → promise) and `RingLog`.

- [ ] **Step 1: Write the failing test** (`test/stopwaiter.test.ts`):

```ts
import { describe, it, expect } from "vitest";
import { StopWaiter, RingLog } from "../src/lib/stopwaiter";

describe("StopWaiter", () => {
  it("resolves the pending wait on a stopped event", async () => {
    const w = new StopWaiter();
    const p = w.wait(1000);
    w.onEvent({ event: "stopped", body: { reason: "breakpoint", threadId: 1 } });
    expect(await p).toEqual({ kind: "stopped", reason: "breakpoint", threadId: 1 });
  });
  it("resolves on terminated and times out otherwise", async () => {
    const w = new StopWaiter();
    const p = w.wait(1000);
    w.onEvent({ event: "terminated" });
    expect((await p).kind).toBe("terminated");
    expect((await w.wait(10)).kind).toBe("timeout");
  });
  it("delivers an event that arrived before wait was called", async () => {
    const w = new StopWaiter();
    w.onEvent({ event: "stopped", body: { reason: "step", threadId: 2 } });
    expect((await w.wait(10)).reason).toBe("step");
  });
});

describe("RingLog", () => {
  it("keeps the newest lines and reads since a cursor", () => {
    const r = new RingLog(3);
    for (const l of ["a", "b", "c", "d"]) r.push(l);
    const first = r.since(0);
    expect(first.lines).toEqual(["b", "c", "d"]);
    r.push("e");
    expect(r.since(first.cursor).lines).toEqual(["e"]);
  });
});
```

- [ ] **Step 2: Run** `npm test`; expected module-not-found.

- [ ] **Step 3: Implement**

`src/lib/stopwaiter.ts`:

```ts
export type StopResult = { kind: "stopped" | "terminated" | "exited" | "timeout"; reason?: string; threadId?: number; exitCode?: number };

// StopWaiter turns DAP events into "wait for the next stop" promises; an
// event that arrives before anyone waits is kept for the next wait.
export class StopWaiter {
  private pending?: (r: StopResult) => void;
  private queued?: StopResult;

  onEvent(ev: { event: string; body?: any }) {
    let r: StopResult | undefined;
    if (ev.event === "stopped") r = { kind: "stopped", reason: ev.body?.reason, threadId: ev.body?.threadId };
    else if (ev.event === "terminated") r = { kind: "terminated" };
    else if (ev.event === "exited") r = { kind: "exited", exitCode: ev.body?.exitCode };
    if (!r) return;
    if (this.pending) { const p = this.pending; this.pending = undefined; p(r); } else this.queued = r;
  }

  wait(timeoutMs: number): Promise<StopResult> {
    if (this.queued) { const q = this.queued; this.queued = undefined; return Promise.resolve(q); }
    return new Promise((resolve) => {
      const t = setTimeout(() => { this.pending = undefined; resolve({ kind: "timeout" }); }, timeoutMs);
      this.pending = (r) => { clearTimeout(t); resolve(r); };
    });
  }
}

// RingLog keeps the newest N lines with a monotonic cursor for "since".
export class RingLog {
  private lines: string[] = [];
  private base = 0; // cursor of lines[0]
  constructor(private cap: number) {}
  push(line: string) {
    this.lines.push(line);
    if (this.lines.length > this.cap) { this.lines.shift(); this.base++; }
  }
  since(cursor: number): { lines: string[]; cursor: number } {
    const start = Math.max(0, cursor - this.base);
    return { lines: this.lines.slice(start), cursor: this.base + this.lines.length };
  }
}
```

`src/tools/debug.ts`:

```ts
import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { StopWaiter, RingLog, StopResult } from "../lib/stopwaiter";
import { relPath, absPath } from "../lib/paths";

const WAIT_MS = 60_000;
const folders = () => (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);

class DebugManager {
  session?: vscode.DebugSession;
  waiter = new StopWaiter();
  log = new RingLog(2000);
  threadId?: number;

  constructor(ctx: vscode.ExtensionContext) {
    ctx.subscriptions.push(
      vscode.debug.registerDebugAdapterTrackerFactory("*", {
        createDebugAdapterTracker: (session) => ({
          onDidSendMessage: (m: any) => {
            if (session !== this.session || m.type !== "event") return;
            if (m.event === "output" && m.body?.output) for (const l of String(m.body.output).split("\n")) if (l) this.log.push(l);
            if (m.event === "stopped" && m.body?.threadId) this.threadId = m.body.threadId;
            this.waiter.onEvent(m);
          },
        }),
      }),
      vscode.debug.onDidTerminateDebugSession((s) => { if (s === this.session) this.session = undefined; }),
    );
  }

  async start(args: any): Promise<string> {
    if (this.session) await vscode.debug.stopDebugging(this.session);
    this.waiter = new StopWaiter(); this.log = new RingLog(2000); this.threadId = undefined;
    const folder = vscode.workspace.workspaceFolders?.[0];
    let config: string | vscode.DebugConfiguration;
    if (args.config) config = args.config;
    else if (args.program) {
      const type = args.type === "python" ? "python" : "go";
      config = type === "python"
        ? { type: "python", request: "launch", name: "be-code", program: absPath(folders(), args.program), args: args.args ?? [], console: "internalConsole", justMyCode: false }
        : { type: "go", request: "launch", name: "be-code", mode: "debug", program: absPath(folders(), args.program), args: args.args ?? [] };
    } else throw new Error("give config (a launch.json name) or program (+ type go|python)");
    const started = new Promise<vscode.DebugSession>((resolve) => {
      const d = vscode.debug.onDidStartDebugSession((s) => { d.dispose(); resolve(s); });
    });
    if (!(await vscode.debug.startDebugging(folder, config))) throw new Error("debug session failed to start (check the launch configuration and that the debugger extension is installed)");
    this.session = await started;
    return this.describe(await this.waiter.wait(WAIT_MS));
  }

  need(): vscode.DebugSession { if (!this.session) throw new Error("no debug session; use debug_start first"); return this.session; }

  async describe(r: StopResult): Promise<string> {
    if (r.kind === "stopped") {
      const top = await this.stack(3);
      return `stopped (${r.reason ?? "unknown"})\n${top}`;
    }
    if (r.kind === "timeout") return `still running after ${WAIT_MS / 1000}s (no breakpoint hit); use debug_output or debug_stop`;
    return r.kind === "exited" ? `program exited with code ${r.exitCode ?? "?"}` : "debug session terminated";
  }

  async stack(depth: number): Promise<string> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    const st = await s.customRequest("stackTrace", { threadId: tid, startFrame: 0, levels: depth });
    return (st.stackFrames ?? []).map((f: any, i: number) => `#${i} ${f.name} ${f.source?.path ? relPath(folders(), f.source.path) : "?"}:${f.line}  [frame ${f.id}]`).join("\n") || "no frames";
  }

  async variables(frameArg: number | undefined, scopeName: string): Promise<string> {
    const s = this.need();
    const frameId = frameArg ?? (await this.topFrameId());
    const scopes = (await s.customRequest("scopes", { frameId })).scopes ?? [];
    const out: string[] = [];
    for (const sc of scopes) {
      if (scopeName !== "all" && !sc.name.toLowerCase().includes(scopeName === "args" ? "arg" : "local")) continue;
      out.push(`${sc.name}:`);
      const vars = (await s.customRequest("variables", { variablesReference: sc.variablesReference })).variables ?? [];
      for (const v of vars.slice(0, 60)) {
        out.push(`  ${v.name} = ${v.value}${v.type ? " (" + v.type + ")" : ""}`);
        if (v.variablesReference > 0 && vars.length <= 10) {
          const kids = (await s.customRequest("variables", { variablesReference: v.variablesReference })).variables ?? [];
          for (const k of kids.slice(0, 20)) out.push(`    ${k.name} = ${k.value}`);
        }
      }
    }
    return out.join("\n") || "no variables in scope";
  }

  async topFrameId(): Promise<number> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    const st = await s.customRequest("stackTrace", { threadId: tid, startFrame: 0, levels: 1 });
    return st.stackFrames?.[0]?.id;
  }

  async resume(cmd: "continue" | "next" | "stepIn" | "stepOut"): Promise<string> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    await s.customRequest(cmd, { threadId: tid });
    return this.describe(await this.waiter.wait(WAIT_MS));
  }
}

export function registerDebugTools(reg: ToolRegistry, ctx: vscode.ExtensionContext) {
  const dm = new DebugManager(ctx);
  reg.add({ name: "debug_configs", description: "Launch configurations available (launch.json). debug_start also accepts program + type directly.", inputSchema: { type: "object", properties: {} },
    handler: async () => {
      const cfgs = vscode.workspace.getConfiguration("launch").get<any[]>("configurations") ?? [];
      return cfgs.length ? cfgs.map((c) => `${c.name} (${c.type}, ${c.request})`).join("\n") : "no launch configurations; pass program and type (go|python) to debug_start";
    } });
  reg.add({ name: "debug_start", description: "Start debugging and wait until it stops at a breakpoint or exits (60s max). Use config (launch.json name) or program + type (go|python) + args.",
    inputSchema: { type: "object", properties: { config: { type: "string" }, program: { type: "string" }, type: { type: "string", enum: ["go", "python"] }, args: { type: "array", items: { type: "string" } } } },
    handler: (a) => dm.start(a) });
  reg.add({ name: "debug_breakpoint", description: "Add or remove a breakpoint at path:line, optionally with a condition. Returns the breakpoints in that file.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, action: { type: "string", enum: ["add", "remove"] }, condition: { type: "string" } }, required: ["path", "line"] },
    handler: async (a) => {
      const uri = vscode.Uri.file(absPath(folders(), a.path));
      const existing = vscode.debug.breakpoints.filter((b) => b instanceof vscode.SourceBreakpoint && b.location.uri.fsPath === uri.fsPath) as vscode.SourceBreakpoint[];
      if (a.action === "remove") vscode.debug.removeBreakpoints(existing.filter((b) => b.location.range.start.line === a.line - 1));
      else vscode.debug.addBreakpoints([new vscode.SourceBreakpoint(new vscode.Location(uri, new vscode.Position(a.line - 1, 0)), true, a.condition)]);
      const now = vscode.debug.breakpoints.filter((b) => b instanceof vscode.SourceBreakpoint && b.location.uri.fsPath === uri.fsPath) as vscode.SourceBreakpoint[];
      return now.length ? now.map((b) => `${a.path}:${b.location.range.start.line + 1}${b.condition ? " if " + b.condition : ""}`).join("\n") : `no breakpoints in ${a.path}`;
    } });
  reg.add({ name: "debug_continue", description: "Continue and wait for the next stop.", inputSchema: { type: "object", properties: {} }, handler: () => dm.resume("continue") });
  reg.add({ name: "debug_step", description: "Step over, into or out, and wait for the stop.", inputSchema: { type: "object", properties: { step: { type: "string", enum: ["over", "into", "out"] } }, required: ["step"] },
    handler: (a) => dm.resume(a.step === "into" ? "stepIn" : a.step === "out" ? "stepOut" : "next") });
  reg.add({ name: "debug_stack", description: "Current call stack.", inputSchema: { type: "object", properties: { depth: { type: "integer" } } }, handler: (a) => dm.stack(a.depth ?? 10) });
  reg.add({ name: "debug_variables", description: "Variables in a frame (default: top). scope: locals, args or all.", inputSchema: { type: "object", properties: { frame: { type: "integer" }, scope: { type: "string" } } },
    handler: (a) => dm.variables(a.frame, a.scope ?? "locals") });
  reg.add({ name: "debug_evaluate", description: "Evaluate an expression in a frame (default: top).", inputSchema: { type: "object", properties: { expression: { type: "string" }, frame: { type: "integer" } }, required: ["expression"] },
    handler: async (a) => { const r = await dm.need().customRequest("evaluate", { expression: a.expression, frameId: a.frame ?? (await dm.topFrameId()), context: "repl" }); return `${r.result}${r.type ? " (" + r.type + ")" : ""}`; } });
  reg.add({ name: "debug_output", description: "Debug console output since the last read (pass the returned cursor next time).", inputSchema: { type: "object", properties: { since: { type: "integer" } } },
    handler: async (a) => { const r = dm.log.since(a.since ?? 0); return (r.lines.join("\n") || "(no new output)") + `\n[cursor ${r.cursor}]`; } });
  reg.add({ name: "debug_stop", description: "Stop the debug session.", inputSchema: { type: "object", properties: {} },
    handler: async () => { if (dm.session) await vscode.debug.stopDebugging(dm.session); dm.session = undefined; return "stopped"; } });
}
```

- [ ] **Step 4: Run** `npm test` and `npm run build`; expected PASS.

---

### Task 11: In-editor diff review tool

**Files:**
- Create: `vscode/src/tools/review.ts`

**Interfaces:**
- Produces tool `review_diff` (`path, original, proposed, summary`) returning `{"decision":"accept"|"reject"|"accept_all"}` as JSON text.

- [ ] **Step 1: Implement** (VS Code API only; no unit test is possible without the extension host, so this task is verified live in Task 12)

```ts
import * as vscode from "vscode";
import { ToolRegistry } from "./registry";

const SCHEME = "be-code-review";

export function registerReviewTool(reg: ToolRegistry, ctx: vscode.ExtensionContext) {
  const docs = new Map<string, string>();
  ctx.subscriptions.push(vscode.workspace.registerTextDocumentContentProvider(SCHEME, {
    provideTextDocumentContent: (uri) => docs.get(uri.toString()) ?? "",
  }));
  let acceptAll = false;

  reg.add({
    name: "review_diff",
    description: "(used by BE-Code, not by the model) Show a proposed file change as an editor diff and return the user's decision.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, original: { type: "string" }, proposed: { type: "string" }, summary: { type: "string" } }, required: ["path", "proposed"] },
    handler: async (a) => {
      if (acceptAll) return JSON.stringify({ decision: "accept" });
      const id = Date.now().toString(36);
      const left = vscode.Uri.parse(`${SCHEME}:/${id}/original/${a.path}`);
      const right = vscode.Uri.parse(`${SCHEME}:/${id}/proposed/${a.path}`);
      docs.set(left.toString(), a.original ?? "");
      docs.set(right.toString(), a.proposed ?? "");
      const title = `BE-Code: ${a.path} (proposed change)`;
      await vscode.commands.executeCommand("vscode.diff", left, right, title, { preview: true });
      const choice = await vscode.window.showInformationMessage(a.summary ?? `Apply change to ${a.path}?`, { modal: true }, "Accept", "Accept all this session", "Reject");
      for (const g of vscode.window.tabGroups.all) for (const t of g.tabs) if (t.label === title) await vscode.window.tabGroups.close(t);
      docs.delete(left.toString()); docs.delete(right.toString());
      if (choice === "Accept all this session") { acceptAll = true; return JSON.stringify({ decision: "accept_all" }); }
      return JSON.stringify({ decision: choice === "Accept" ? "accept" : "reject" });
    },
  });
}
```

- [ ] **Step 2: Build** `npm run build`; expected `dist/extension.js` with no type errors (`npx tsc --noEmit` also clean).

---

### Task 12: Packaging, Makefile, docs, and the live checklist

**Files:**
- Modify: `build.mk` (`vscode` and `release` targets), `README.md`, `CHANGELOG.md`, root `CLAUDE.md`
- Create: `vscode/README.md`, `docs/vscode-live-checklist.md`

- [ ] **Step 1: Makefile**

```make
vscode: ## build and package the VS Code extension into dist/
	cd vscode && npm install --no-audit --no-fund && npm test && npm run package

release: build vscode
	...existing cross-compiles...
```

(Keep the existing `release` recipe lines; add `vscode` as a prerequisite so the `.vsix` lands in `dist/` next to the binaries.)

- [ ] **Step 2: Docs** — README "VS Code" section: install from the Extensions view (Install from VSIX → `dist/be-code-1.0.0.vsix`), the status bar item, `BE-Code: Open terminal`, what the agent gains (diagnostics, symbols, debugger, editor diff review, automatic context), config `ide.enabled` / `ide.auto_context`, flags `--ide` / `--no-ide`, and the note that shell approvals stay in the terminal. `vscode/README.md`: a short marketplace-style description. CHANGELOG bullet. Root `CLAUDE.md`: an "Editor bridge" paragraph (`internal/ide`, `ide_` prefix, `ReviewWrite`, `ContextProvider`, `vscode/` layout, `make -f build.mk vscode`).

- [ ] **Step 3: Live checklist** (`docs/vscode-live-checklist.md`) for the user:

```
1. make -f build.mk release; install dist/be-code-1.0.0.vsix (Extensions → ... → Install from VSIX); reload window.
2. Status bar shows "BE-Code: listening". Run "BE-Code: Open terminal". Expect "VS Code connected: 17 tools" and the ⌘ ide marker.
3. Go project: introduce a compile error; ask "what errors are there?" → expect ide_diagnostics with the error line.
4. Ask "where is X defined and who calls it?" → expect ide_definition / ide_references.
5. Ask "set a breakpoint at main.go:NN, start debugging with the 'Launch' config, and tell me the value of Y when it stops" → expect debug_breakpoint, debug_start, debug_variables.
6. Python project (WorldSim): same as 5 with a debugpy launch config.
7. Ask for a small file edit → expect an editor diff and the Accept/Reject dialog; Reject once, then Accept.
8. Select some lines, ask "explain this" → expect the [editor: …] note in the transcript.
9. Close VS Code's window with be-code running → expect the "editor bridge disconnected" behaviour: ide_* calls fail clearly, the next file change asks in the TUI.
```

- [ ] **Step 4: Verify** `make -f build.mk verify`, `cd vscode && npm test && npm run build`, then `make -f build.mk release` and confirm `dist/` holds four binaries and the `.vsix`.
