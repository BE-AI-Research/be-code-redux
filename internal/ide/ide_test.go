package ide

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// A zero (or negative) PID can never be a live process; Discover must treat
// it like a dead-process lock and remove the file rather than reporting it
// as an editor session forever (processAlive(0) targets the caller's own
// process group on Linux and would otherwise report "alive").
func TestDiscoverRemovesZeroPIDLock(t *testing.T) {
	dir := t.TempDir()
	p := writeLock(t, dir, Lock{PID: 0, Port: 5, Token: "z", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())
	l, err := Discover(dir, "/tmp/proj")
	if err != nil || l != nil {
		t.Fatalf("got %+v err=%v", l, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("zero-pid lock not removed")
	}
}

func TestLockDirCreatesDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dir, err := LockDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("LockDir did not create %q: %v", dir, err)
	}
}

// fakeIDEServer answers initialize/tools/list over newline JSON-RPC,
// mimicking the editor extension's embedded MCP server (see
// internal/mcp/client_tcp_test.go's fakeTCPServer, adapted here since
// Connect is exercised at the ide package level).
func fakeIDEServer(t *testing.T, token string) (port int, gotToken *string) {
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
				res = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake-ide"}}
			case "tools/list":
				res = map[string]any{"tools": []map[string]any{{"name": "ide_diagnostics", "description": "errors", "inputSchema": map[string]any{"type": "object"}}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			conn.Write(append(b, '\n'))
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestConnectDialsLoopbackWithToken(t *testing.T) {
	port, got := fakeIDEServer(t, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	if *got != "tok" {
		t.Fatalf("token received = %q", *got)
	}
	if len(sess.Client.Tools()) != 1 || sess.Client.Tools()[0].Name != "ide_diagnostics" {
		t.Fatalf("tools = %+v", sess.Client.Tools())
	}
	sess.Close()
	sess.Close() // idempotent

	badPort, _ := fakeIDEServer(t, "tok")
	if _, err := Connect(ctx, &Lock{Port: badPort, Token: "wrong", IDEName: "vscode"}); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}
