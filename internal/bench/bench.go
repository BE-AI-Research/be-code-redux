// Package bench is BE-Code's embedded offline eval suite: small, real
// coding tasks run through the actual agent against a live backend, scored
// by the same toolchains verification uses. It answers the question that
// matters for a local-model fleet: "which of my models can actually do the
// work?" — empirically, per machine, fully offline.
package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Task is one benchmark item.
type Task struct {
	Name   string
	Needs  string // required binary on PATH ("" = none)
	Files  map[string]string
	Prompt string
	Check  string // shell command that must exit 0 in the workspace
	// CheckFunc, when set, replaces Check with a Go-side judge (portable
	// across sh and PowerShell).
	CheckFunc func(dir string) error
	Timeout   time.Duration
}

// Result is one task's outcome for one model.
type Result struct {
	Task     string        `json:"task"`
	Passed   bool          `json:"passed"`
	Skipped  bool          `json:"skipped"`
	Duration time.Duration `json:"duration_ns"`
	Requests int           `json:"model_requests"`
	Tokens   int           `json:"completion_tokens"`
	Err      string        `json:"error,omitempty"`
}

// Suite returns the built-in tasks.
func Suite() []Task {
	return []Task{
		{
			Name:  "go-fix-compile",
			Needs: "go",
			Files: map[string]string{
				"go.mod":  "module bench.local/fix\n\ngo 1.22\n",
				"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tmsg := greeting(\"world\"\n\tfmt.Println(msg)\n}\n\nfunc greeting(name string) string {\n\treturn \"hello \" + name\n}\n",
			},
			Prompt:  "The project does not compile. Find and fix the problem.",
			Check:   "go build ./...",
			Timeout: 4 * time.Minute,
		},
		{
			Name:  "go-implement-func",
			Needs: "go",
			Files: map[string]string{
				"go.mod":        "module bench.local/impl\n\ngo 1.22\n",
				"mathx.go":      "package mathx\n\n// Clamp returns v limited to the range [lo, hi].\nfunc Clamp(v, lo, hi int) int {\n\tpanic(\"TODO: implement\")\n}\n",
				"mathx_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestClamp(t *testing.T) {\n\tcases := [][4]int{{5, 0, 10, 5}, {-3, 0, 10, 0}, {42, 0, 10, 10}, {7, 7, 7, 7}}\n\tfor _, c := range cases {\n\t\tif got := Clamp(c[0], c[1], c[2]); got != c[3] {\n\t\t\tt.Fatalf(\"Clamp(%d,%d,%d)=%d want %d\", c[0], c[1], c[2], got, c[3])\n\t\t}\n\t}\n}\n",
			},
			Prompt:  "Implement the Clamp function in mathx.go so the tests pass.",
			Check:   "go test ./...",
			Timeout: 4 * time.Minute,
		},
		{
			Name:  "go-fix-logic-bug",
			Needs: "go",
			Files: map[string]string{
				"go.mod":      "module bench.local/bug\n\ngo 1.22\n",
				"sum.go":      "package sum\n\n// EvenSum returns the sum of the even numbers in xs.\nfunc EvenSum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\tif x%2 == 1 {\n\t\t\ttotal += x\n\t\t}\n\t}\n\treturn total\n}\n",
				"sum_test.go": "package sum\n\nimport \"testing\"\n\nfunc TestEvenSum(t *testing.T) {\n\tif got := EvenSum([]int{1, 2, 3, 4}); got != 6 {\n\t\tt.Fatalf(\"got %d want 6\", got)\n\t}\n\tif got := EvenSum([]int{-2, 0, 5}); got != -2 {\n\t\tt.Fatalf(\"got %d want -2\", got)\n\t}\n}\n",
			},
			Prompt:  "go test fails in this project. Diagnose the bug and fix it.",
			Check:   "go test ./...",
			Timeout: 4 * time.Minute,
		},
		{
			Name:  "py-fix-function",
			Needs: "python3",
			Files: map[string]string{
				"slugify.py":      "def slugify(title):\n    \"\"\"lowercase, words joined by single hyphens, no leading/trailing hyphens\"\"\"\n    return title.replace(' ', '-')\n",
				"test_slugify.py": "from slugify import slugify\n\nassert slugify('Hello World') == 'hello-world', slugify('Hello World')\nassert slugify('  Spaced   Out  ') == 'spaced-out', slugify('  Spaced   Out  ')\nassert slugify('Already-Good') == 'already-good'\nprint('OK')\n",
			},
			Prompt:  "Running `python3 test_slugify.py` fails. Fix slugify so the assertions pass.",
			Check:   "python3 test_slugify.py",
			Timeout: 4 * time.Minute,
		},
		{
			Name:  "edit-precision",
			Needs: "",
			Files: map[string]string{
				"config.ini": "[server]\nport = 8080\nhost = 0.0.0.0\ntimeout = 30\n\n[client]\nport = 8080\nretries = 3\n",
			},
			Prompt: "In config.ini, change ONLY the server section's port to 9443. Leave the client port alone.",
			CheckFunc: func(dir string) error {
				data, err := os.ReadFile(filepath.Join(dir, "config.ini"))
				if err != nil {
					return err
				}
				server, client := iniSection(string(data), "server"), iniSection(string(data), "client")
				if !strings.Contains(server, "port = 9443") {
					return fmt.Errorf("server port not changed to 9443")
				}
				if !strings.Contains(client, "port = 8080") {
					return fmt.Errorf("client port was modified")
				}
				return nil
			},
			Timeout: 2 * time.Minute,
		},
	}
}

// RunTask executes one task with a fresh agent in a temp workspace.
func RunTask(ctx context.Context, cfg *config.Config, p provider.Provider, model string, t Task) Result {
	res := Result{Task: t.Name}
	if t.Needs != "" {
		if _, err := exec.LookPath(t.Needs); err != nil {
			res.Skipped = true
			return res
		}
	}
	dir, err := os.MkdirTemp("", "becode-bench-*")
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer os.RemoveAll(dir)
	for name, content := range t.Files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			res.Err = err.Error()
			return res
		}
	}

	// Bench config: fully autonomous, no model-verification overlap (the
	// bench scores the raw agent loop; its own Check is the judge).
	bcfg := *cfg
	bcfg.AutoApproveShell = true
	bcfg.ApproveFileWrites = false
	bcfg.VerifyOnDone = true
	bcfg.ReviewOnDone = false
	bcfg.RepoMap = true

	reg, err := tools.NewRegistry(dir, func(string, string) bool { return true })
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer reg.Close()
	ag := agent.New(&bcfg, p, model, reg, "")

	tctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	start := time.Now()
	_, _, runErr := ag.RunFull(tctx, t.Prompt)
	res.Duration = time.Since(start)
	res.Requests = ag.Stats.Requests
	res.Tokens = ag.Stats.CompletionTokens
	if runErr != nil {
		res.Err = runErr.Error()
	}

	var out string
	var checkErr error
	if t.CheckFunc != nil {
		checkErr = t.CheckFunc(dir)
		if checkErr != nil {
			out = checkErr.Error()
		}
	} else {
		out, checkErr = tools.RunShell(context.Background(), dir, t.Check, 2*time.Minute)
	}
	res.Passed = checkErr == nil
	if !res.Passed && res.Err == "" {
		first := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
		res.Err = "check failed: " + first
	}
	return res
}

// iniSection returns the body of [name] up to the next section header.
func iniSection(ini, name string) string {
	start := strings.Index(ini, "["+name+"]")
	if start < 0 {
		return ""
	}
	body := ini[start+len(name)+2:]
	if next := strings.Index(body, "\n["); next >= 0 {
		body = body[:next]
	}
	return body
}

// Format renders a result table for one model.
func Format(model string, results []Result) string {
	var b strings.Builder
	passed, run := 0, 0
	fmt.Fprintf(&b, "model: %s\n", model)
	for _, r := range results {
		switch {
		case r.Skipped:
			fmt.Fprintf(&b, "  SKIP  %-20s (missing toolchain)\n", r.Task)
		case r.Passed:
			run++
			passed++
			fmt.Fprintf(&b, "  PASS  %-20s %6.1fs  %2d requests  %5d tokens\n",
				r.Task, r.Duration.Seconds(), r.Requests, r.Tokens)
		default:
			run++
			fmt.Fprintf(&b, "  FAIL  %-20s %6.1fs  %2d requests  %s\n",
				r.Task, r.Duration.Seconds(), r.Requests, r.Err)
		}
	}
	if run > 0 {
		fmt.Fprintf(&b, "  score: %d/%d (%.0f%%)\n", passed, run, float64(passed)*100/float64(run))
	}
	return b.String()
}
