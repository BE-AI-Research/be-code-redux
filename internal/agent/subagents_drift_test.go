package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// hookedProvider is a scripted provider that lets a test act between the
// sub-agent's requests — the only place "another writer changed a file
// after the sub-agent read it" can be staged deterministically.
type hookedProvider struct {
	scriptedProvider
	before func(i int, req provider.ChatRequest)
}

func (h *hookedProvider) Chat(ctx context.Context, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	if h.before != nil {
		h.before(h.i, req)
	}
	return h.scriptedProvider.Chat(ctx, req, onDelta)
}

// A file the sub-agent read and somebody else then changed is named in the
// hand-back as having moved under it; a file the sub-agent wrote itself is
// not. Here the workspace is not a repository, so there is no base branch
// and the line says nothing about one.
func TestHandBackNamesFilesThatMovedUnderTheSubAgent(t *testing.T) {
	sub := &hookedProvider{scriptedProvider: scriptedProvider{responses: []provider.ChatResponse{
		toolCall("read_file", `{"path":"internal/scan/a.go"}`),
		toolCall("write_file", `{"path":"internal/scan/token.go","content":"package scan\n"}`),
		toolCall("task", `{"action":"status","id":"1.1","status":"done"}`),
		{Content: "ported the token loop"},
	}}}
	f := newSubFixture(t, &scriptedProvider{}, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return sub, 0, nil
	}
	a := filepath.Join(f.dir, "internal/scan/a.go")
	if err := os.WriteFile(a, []byte("package scan // v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub.before = func(i int, req provider.ChatRequest) {
		if i == 1 {
			// The read result is in this request; the file moves now, under it.
			if err := os.WriteFile(a, []byte("package scan // somebody else\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "done" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if len(hb.Drifted) != 1 || hb.Drifted[0] != "internal/scan/a.go" {
		t.Errorf("drifted = %v, want just internal/scan/a.go", hb.Drifted)
	}
	if hb.Base != "" {
		t.Errorf("no repository, but a base branch %q", hb.Base)
	}
	line := f.ag.DrainInbox()[0]
	if !strings.Contains(line, "changed under it: internal/scan/a.go") || strings.Contains(line, "sub-agent/") {
		t.Errorf("hand-back line: %q", line)
	}
	_ = id
}

// In a repository the dispatch snapshots the tree first and tells the
// sub-agent the branch, so it can read any file as it was when it began;
// the hand-back carries the same branch for the main model.
func TestDispatchSnapshotsTheTreeAndTellsTheSubAgent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("write_file", `{"path":"internal/scan/a.go","content":"package scan // rewritten\n"}`),
		toolCall("task", `{"action":"status","id":"1.1","status":"done"}`),
		{Content: "rewrote a.go"},
	}}
	f := newSubFixture(t, sub, nil)
	a := filepath.Join(f.dir, "internal/scan/a.go")
	if err := os.WriteFile(a, []byte("package scan // original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = f.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f.assign(t)
	f.ag.ScheduleSubAgents()
	d := wait(t, f.start, "start")
	if !strings.HasPrefix(d.Base, "be-code/sub-agent/") {
		t.Fatalf("dispatch base = %q", d.Base)
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Base != d.Base {
		t.Errorf("hand-back base %q, dispatch base %q", hb.Base, d.Base)
	}
	if len(hb.Drifted) != 0 {
		t.Errorf("the sub-agent's own write is not drift: %v", hb.Drifted)
	}
	sys := sub.lastReq.Messages[0].Content
	if !strings.Contains(sys, d.Base) {
		t.Errorf("the sub-agent was not told its base branch:\n%s", sys)
	}
	// The branch holds the file as it was before the sub-agent began.
	cmd := exec.Command("git", "show", d.Base+":internal/scan/a.go")
	cmd.Dir = f.dir
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "package scan // original\n" {
		t.Errorf("base branch content: %q (%v)", out, err)
	}
	// And HEAD, the index and the checkout did not move.
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = f.dir
	out, _ = cmd.CombinedOutput()
	if !strings.Contains(string(out), " M internal/scan/a.go") {
		t.Errorf("the working tree should show the sub-agent's change as unstaged: %q", out)
	}
}
