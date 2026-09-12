package verify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A workspace virtualenv's interpreter is preferred over the system python
// for the python checks, since that is where the project's pytest lives.
func TestDetectPrefersWorkspaceVenv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix venv layout")
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644)
	os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, ".venv", "bin", "python"), []byte("#!/bin/sh\n"), 0o755)
	proj := Detect(root)
	for _, c := range proj.Checks {
		if !strings.HasPrefix(c.Command, ".venv/bin/python ") {
			t.Fatalf("check %q does not use the venv interpreter: %s", c.Name, c.Command)
		}
	}
	// Without a venv the system python is used.
	proj = Detect(t.TempDir())
	if len(proj.Checks) != 0 {
		t.Fatal("empty dir should have no checks")
	}
}

// pytest missing or no tests collected is a skip, not a failure the model
// is asked to repair.
func TestPytestMissingOrEmptyIsSkipped(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name, cmd string
	}{
		{"no module", `printf '/usr/bin/python3: No module named pytest\n' >&2; exit 1`},
		{"no tests", `printf 'no tests ran in 0.01s\n'; exit 5`},
	}
	for _, c := range cases {
		rep := RunChecks(context.Background(), root, Project{Kind: "python", Checks: []Check{
			{Name: "pytest", Command: c.cmd, Timeout: 10 * time.Second},
		}})
		if !rep.Passed() {
			t.Fatalf("%s: treated as failure: %s", c.name, rep.Human())
		}
		if len(rep.Results) != 1 || !rep.Results[0].Skipped {
			t.Fatalf("%s: not marked skipped: %+v", c.name, rep.Results)
		}
		if !strings.Contains(rep.Human(), "SKIP") {
			t.Fatalf("%s: Human() lacks SKIP: %s", c.name, rep.Human())
		}
	}
	// A real test failure is still a failure.
	rep := RunChecks(context.Background(), root, Project{Kind: "python", Checks: []Check{
		{Name: "pytest", Command: `printf '1 failed\n'; exit 1`, Timeout: 10 * time.Second},
	}})
	if rep.Passed() {
		t.Fatal("real failure passed")
	}
}
