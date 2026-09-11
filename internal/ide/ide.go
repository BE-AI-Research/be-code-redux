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
		if json.Unmarshal(data, &l) != nil {
			// Malformed and can never become valid; remove it.
			_ = os.Remove(p)
			continue
		}
		if l.Port == 0 {
			continue
		}
		if l.PID <= 0 || !processAlive(l.PID) {
			// A missing/zero/negative PID is never a live process; treat it
			// the same as a dead-process lock rather than "alive forever"
			// (processAlive(0) targets the caller's own process group).
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
