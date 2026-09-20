package ide

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

// manifestEntry mirrors one entry of vscode/tools.manifest.json — the single
// source of truth BECode.Bridge.ToolManifest reads back out of its embedded
// copy (visualstudio/src/BECode.Bridge/ToolManifest.cs). Kept minimal and
// local to this test rather than imported: this package must not depend on
// vscode/'s own tooling.
type manifestEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Hidden      bool            `json:"hidden"`
}

// overriddenDescriptions are the two tool names whose descriptions
// visualstudio/src/BECode.Bridge/ToolOverrides.cs deliberately replaces
// (the shared manifest's wording steers a model toward vscode's
// program+type launch shape, which the Visual Studio bridge refuses).
// Every other listed tool's description must be the manifest's own,
// verbatim.
var overriddenDescriptions = map[string]bool{
	"debug_configs": true,
	"debug_start":   true,
}

// repoRoot locates the repository root (the directory containing both
// internal/ and visualstudio/) from this source file's own path, so the
// test does not depend on the working directory `go test` happens to run
// from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// internal/ide/contract_test.go -> repo root is two levels up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "visualstudio", "BECode.VisualStudio.sln")); err != nil {
		t.Fatalf("could not locate repo root from %s: %v", file, err)
	}
	return root
}

func loadNonHiddenManifest(t *testing.T, root string) []manifestEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "vscode", "tools.manifest.json"))
	if err != nil {
		t.Fatalf("reading vscode/tools.manifest.json: %v", err)
	}
	var all []manifestEntry
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatalf("parsing vscode/tools.manifest.json: %v", err)
	}
	var out []manifestEntry
	for _, e := range all {
		if !e.Hidden {
			out = append(out, e)
		}
	}
	return out
}

func schemasEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var ao, bo any
	if err := json.Unmarshal(a, &ao); err != nil {
		t.Fatalf("unmarshalling schema a: %v", err)
	}
	if err := json.Unmarshal(b, &bo); err != nil {
		t.Fatalf("unmarshalling schema b: %v", err)
	}
	return reflect.DeepEqual(ao, bo)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readReady reads a single line from r (buffered) or times out.
func readLineWithTimeout(t *testing.T, r *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			t.Fatalf("reading fake host stdout: %v", res.err)
		}
		return strings.TrimRight(res.line, "\r\n")
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for a line from the fake host", timeout)
		return ""
	}
}

// TestVisualStudioBridgeSpeaksTheHarnesssProtocol drives the real Go client
// (ide.LockDir/Discover/Connect, mcp.Client.CallTool) against the real C#
// bridge (BECode.Bridge, via the BECode.Bridge.FakeHost console app built
// from this test) — the only thing in this repository that exercises the
// two halves of the Visual Studio bridge together before the owner installs
// the extension on Windows.
func TestVisualStudioBridgeSpeaksTheHarnesssProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the Visual Studio bridge contract test in -short mode")
	}

	dotnetPath, err := exec.LookPath("dotnet")
	if err != nil {
		t.Skip("dotnet not on PATH; the Visual Studio bridge contract test needs the .NET SDK")
	}

	root := repoRoot(t)
	fakeHostProj := filepath.Join(root, "visualstudio", "src", "BECode.Bridge.FakeHost")
	fakeHostDLL := filepath.Join(fakeHostProj, "bin", "Release", "net8.0", "BECode.Bridge.FakeHost.dll")

	// Never let either side touch the real ~/.be-code: TestMain (see
	// testmain_test.go) already points HOME/USERPROFILE at a throwaway
	// directory for the whole package's test run; assert that here, before
	// starting anything, rather than trusting it silently.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if !strings.Contains(home, "be-code-test-home") {
		t.Fatalf("refusing to run: HOME %q does not look like TestMain's throwaway home", home)
	}
	lockDir, err := LockDir()
	if err != nil {
		t.Fatalf("LockDir: %v", err)
	}
	if !strings.HasPrefix(lockDir, home+string(filepath.Separator)) {
		t.Fatalf("refusing to run: LockDir() %q is not under the temp home %q", lockDir, home)
	}

	// Build the fake host ONCE for this test run; `dotnet build` (not `dotnet
	// run`) so the process we later start and signal is the real dotnet
	// process running the built dll, with no extra startup latency.
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelBuild()
	buildCmd := exec.CommandContext(buildCtx, dotnetPath, "build", "-c", "Release", fakeHostProj)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("dotnet build fake host failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(fakeHostDLL); err != nil {
		t.Fatalf("fake host build did not produce %s: %v", fakeHostDLL, err)
	}

	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "sample.go"), "package main\n\nfunc main() {}\n")
	mustWriteFile(t, filepath.Join(workspace, "accept.txt"), "old\n")
	mustWriteFile(t, filepath.Join(workspace, "reject.txt"), "old\n")
	mustWriteFile(t, filepath.Join(workspace, "pending.txt"), "old\n")

	// A pipe whose write end this test holds open for the fake host's
	// stdin: os/exec gives a nil Stdin /dev/null, which the host reads as
	// immediate EOF (a legitimate shutdown trigger, but not one this test
	// wants mid-run) — so stdin must stay open until this test explicitly
	// tears the process down.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	cmd := exec.Command(dotnetPath, fakeHostDLL, "--home", home, "--workspace", workspace)
	cmd.Stdin = stdinR
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fake host: %v", err)
	}
	stdinR.Close() // the child has its own dup of the read end now

	t.Cleanup(func() {
		_ = stdinW.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		if s := stderr.String(); s != "" {
			t.Logf("fake host stderr:\n%s", s)
		}
	})

	reader := bufio.NewReader(stdout)
	readyLine := readLineWithTimeout(t, reader, 30*time.Second)
	if !strings.HasPrefix(readyLine, "READY ") {
		t.Fatalf("expected a line starting %q, got %q", "READY ", readyLine)
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(readyLine, "READY ")); err != nil {
		t.Fatalf("READY line %q did not carry a valid port: %v", readyLine, err)
	}

	// Discover must find the lock, naming the Visual Studio bridge.
	lock, err := Discover(lockDir, workspace)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if lock == nil {
		t.Fatal("Discover found no lock for the fake host")
	}
	if lock.IDEName != "visualstudio" {
		t.Fatalf("lock.IDEName = %q, want %q", lock.IDEName, "visualstudio")
	}

	ctx := context.Background()
	sess, err := Connect(ctx, lock)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	// ---- tool list vs. the manifest's non-hidden entries, names and
	// schemas, in order (debug_configs/debug_start's descriptions are
	// deliberately overridden — see ToolOverrides.cs — so descriptions are
	// compared for the other fourteen only). ----
	manifest := loadNonHiddenManifest(t, root)
	advertised := sess.Client.Tools()
	if len(advertised) != len(manifest) {
		t.Fatalf("advertised %d tools, manifest lists %d non-hidden entries", len(advertised), len(manifest))
	}
	for i, entry := range manifest {
		got := advertised[i]
		if got.Name != entry.Name {
			t.Fatalf("tool[%d].Name = %q, want %q (order must match the manifest)", i, got.Name, entry.Name)
		}
		if !overriddenDescriptions[entry.Name] && got.Description != entry.Description {
			t.Errorf("tool %q description = %q, want the manifest's own %q", entry.Name, got.Description, entry.Description)
		}
		if !schemasEqual(t, got.InputSchema, entry.InputSchema) {
			t.Errorf("tool %q inputSchema = %s, want %s", entry.Name, got.InputSchema, entry.InputSchema)
		}
	}

	// ---- every one of the eighteen tools, called once, isError == false,
	// the scripted answer ScriptedEditorHost.cs hard-codes. ----
	type call struct {
		name string
		args string
		want string // exact expected text; "" means "checked separately below"
	}
	calls := []call{
		{"context", `{}`, ""}, // checked separately: needs JSON decoding
		{"open", `{"path":"sample.go","line":3}`, "opened sample.go:3"},
		{"definition", `{"path":"sample.go","line":1,"col":1}`, "sample.go:3:5"},
		{"references", `{"path":"sample.go","line":1,"col":1}`,
			"2 reference(s)\nsample.go:3: var x = scripted\nsample.go:9: return x"},
		{"hover", `{"path":"sample.go","line":1,"col":1}`, "func Scripted() int"},
		{"diagnostics", `{}`, "1 error, 0 warnings in 1 file\nsample.go:1:1 error fakehost: scripted diagnostic"},
		{"debug_configs", `{}`, "FakeApp (startup project)"},
		{"debug_start", `{}`, "stopped (breakpoint)\n#0 main.main sample.go:10  [frame 1]"},
		{"debug_breakpoint", `{"path":"sample.go","line":5,"action":"add","condition":"x > 1"}`, "sample.go:5 if x > 1"},
		{"debug_continue", `{}`, "stopped (step)\n#0 main.main sample.go:10  [frame 1]"},
		{"debug_step", `{"step":"over"}`, "stopped (step)\n#0 main.main sample.go:10  [frame 1]"},
		{"debug_stack", `{}`, "#0 main.main sample.go:10  [frame 1]"},
		{"debug_variables", `{"scope":"all"}`, "Locals:\n  x = 42 (int)"},
		{"debug_evaluate", `{"expression":"1+41"}`, "42 (int)"},
		{"debug_output", `{}`, "scripted output line\n[cursor 1]"},
		{"debug_stop", `{}`, "stopped"},
		// review_diff/review_cancel are hidden from tools/list but still
		// callable through tools/call.
		{"review_cancel", `{"path":"nope.txt"}`, `{"cancelled":false}`},
	}

	for _, c := range calls {
		out, isErr, err := sess.Client.CallTool(ctx, c.name, json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s: CallTool error: %v", c.name, err)
		}
		if isErr {
			t.Fatalf("%s: isError = true, text = %q", c.name, out)
		}
		if c.want != "" && out != c.want {
			t.Fatalf("%s: text = %q, want %q", c.name, out, c.want)
		}
	}

	// context: checked by decoding, not by exact string (field order is
	// not guaranteed).
	{
		out, isErr, err := sess.Client.CallTool(ctx, "context", json.RawMessage(`{}`))
		if err != nil || isErr {
			t.Fatalf("context: isErr=%v err=%v out=%q", isErr, err, out)
		}
		var c Context
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			t.Fatalf("context: decoding %q: %v", out, err)
		}
		if c.File != "sample.go" || c.Line != 7 || c.SelStart != 2 || c.SelEnd != 4 || c.Selection != "hello" {
			t.Fatalf("context decoded = %+v, want File=sample.go Line=7 SelStart=2 SelEnd=4 Selection=hello", c)
		}
		if len(c.Open) != 1 || c.Open[0] != "sample.go" {
			t.Fatalf("context.Open = %v, want [sample.go]", c.Open)
		}
	}

	// review_diff (the 18th tool): a decision it returns must be one the
	// harness's own ReviewDiff parser (Session.ReviewWrite) accepts;
	// accept.txt is scripted to answer Accept immediately.
	{
		decision := sess.ReviewWrite(ctx, "accept.txt", "old\n", "new content\n", false)
		if decision != tools.ReviewAccept {
			t.Fatalf("ReviewWrite(accept.txt) = %v, want ReviewAccept", decision)
		}
	}

	// Extra: reject.txt answers Reject immediately too (not one of the
	// eighteen required calls, but free coverage of the other scripted
	// branch review_diff's regression case below does not exercise).
	{
		decision := sess.ReviewWrite(ctx, "reject.txt", "old\n", "new content\n", false)
		if decision != tools.ReviewReject {
			t.Fatalf("ReviewWrite(reject.txt) = %v, want ReviewReject", decision)
		}
	}

	// Extra: IEditorHost.OpenAsync's own contract — a missing file throws
	// FileNotFoundException, which EditorTools.Open turns into an ordinary
	// isError result naming the path as given.
	{
		out, isErr, err := sess.Client.CallTool(ctx, "open", json.RawMessage(`{"path":"missing.go"}`))
		if err != nil {
			t.Fatalf("open(missing.go): CallTool error: %v", err)
		}
		if !isErr {
			t.Fatalf("open(missing.go): isError = false, text = %q, want isError=true", out)
		}
		if out != "file not found: missing.go" {
			t.Fatalf("open(missing.go): text = %q, want %q", out, "file not found: missing.go")
		}
	}

	// ---- the regression this whole branch turned on: tools/call now runs
	// concurrently per connection. Start review_diff for pending.txt, which
	// the scripted host blocks on until its token is cancelled, then on the
	// SAME session call review_cancel for that path; the review must return
	// the cancelled outcome and the cancel call must itself return. Bounded
	// by ctx2's deadline (inherited by both calls through the mcp client),
	// so a regression (the old per-connection single in-flight call) fails
	// this test instead of hanging it. ----
	{
		ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel2()

		reviewDone := make(chan tools.ReviewDecision, 1)
		go func() {
			reviewDone <- sess.ReviewWrite(ctx2, "pending.txt", "old\n", "new\n", false)
		}()

		// The pending review is registered (in ReviewTools._pending) before
		// the host call even starts, but there is still a short window
		// between this goroutine sending review_diff and the server
		// actually dispatching it; poll review_cancel until it claims the
		// pending review rather than assuming a fixed delay is enough.
		cancelArgs, _ := json.Marshal(map[string]string{"path": "pending.txt"})
		deadline := time.Now().Add(15 * time.Second)
		var lastCancelOut string
		for {
			out, isErr, err := sess.Client.CallTool(ctx2, "review_cancel", cancelArgs)
			if err != nil {
				t.Fatalf("review_cancel(pending.txt): CallTool error: %v", err)
			}
			if isErr {
				t.Fatalf("review_cancel(pending.txt): isError = true, text = %q", out)
			}
			lastCancelOut = out
			if strings.Contains(out, `"cancelled":true`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("review_cancel(pending.txt) never observed a pending review (last reply %q) — "+
					"regression: tools/call may no longer be running concurrently per connection", lastCancelOut)
			}
			time.Sleep(20 * time.Millisecond)
		}

		select {
		case decision := <-reviewDone:
			if decision != tools.ReviewCancelled {
				t.Fatalf("ReviewWrite(pending.txt) after cancel = %v, want ReviewCancelled", decision)
			}
		case <-ctx2.Done():
			t.Fatal("regression: review_diff(pending.txt) never returned after review_cancel — " +
				"tools/call is not running concurrently per connection")
		}
	}

	// ---- a connection with a wrong token is refused. ----
	{
		badLock := *lock
		badLock.Token = "definitely-the-wrong-token"
		if _, err := Connect(ctx, &badLock); err == nil {
			t.Fatal("Connect with a wrong token succeeded, want a refusal")
		}
	}

	// ---- after the process is stopped, the lock file is gone. ----
	lockPath := lock.path
	if lockPath == "" {
		t.Fatal("lock.path is empty; cannot verify removal")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling fake host: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("fake host exited with error after SIGTERM: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fake host did not exit within 15s of SIGTERM")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file %s still present after the fake host exited (err=%v)", lockPath, err)
	}
}
