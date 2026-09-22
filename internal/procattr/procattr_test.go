package procattr

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Wherever it runs, a command passed through Hide still runs and its output
// is still read.
func TestAHiddenCommandStillRunsAndIsRead(t *testing.T) {
	cmd := exec.Command("go", "version")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "echo go version")
	}
	Hide(cmd)
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "go version") {
		t.Fatalf("output %q, err %v", out, err)
	}
}

// Every place the harness starts a child process must hide its window, or a
// hosted session on Windows flashes a console each time that path runs. This
// reads the source rather than trusting memory: a file that calls
// exec.Command must also call Hide (or the tools package's setProcessGroup,
// which is Hide on Windows). The session host's own spawn is exempt — it is
// started detached on purpose and has no console to show.
func TestEveryChildProcessIsHidden(t *testing.T) {
	root := filepath.Join("..", "..")
	exempt := map[string]bool{
		filepath.Join("internal", "live", "spawn_unix.go"):    true,
		filepath.Join("internal", "live", "spawn_windows.go"): true,
	}
	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "vscode", "visualstudio", "node_modules", "test":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || exempt[rel] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		if !strings.Contains(src, "exec.Command(") && !strings.Contains(src, "exec.CommandContext(") {
			return nil
		}
		if !strings.Contains(src, "procattr.Hide(") && !strings.Contains(src, "setProcessGroup(") {
			missing = append(missing, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Fatalf("these files start a child process without procattr.Hide; on Windows each would open a console window:\n  %s", strings.Join(missing, "\n  "))
	}
}
