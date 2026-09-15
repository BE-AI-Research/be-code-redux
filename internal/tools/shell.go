package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// shellTool runs commands in the workspace. Every invocation goes through
// the registry's ApproveFunc unless auto-approval was configured — the
// model never gets silent shell access by default.
type shellTool struct{ r *Registry }

func (t *shellTool) Name() string { return "shell" }
func (t *shellTool) Description() string {
	return "Run a shell command in the workspace root and return stdout/stderr. Use for builds, tests, and installs. Not for reading/editing files (use the file tools)."
}
func (t *shellTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"command":{"type":"string","description":"The command line to run"},
		"timeout_seconds":{"type":"integer","description":"Kill after this many seconds (default 120, max 600)"}},
		"required":["command"]}`)
}

func (t *shellTool) Run(ctx context.Context, args map[string]any) Result {
	command := strings.TrimSpace(argString(args, "command", "cmd", "script"))
	if command == "" {
		return Result{IsError: true, Content: "command is required"}
	}
	switch classifyCommand(command, t.r.ShellAllow, t.r.ShellDeny) {
	case cmdDenied:
		return Result{IsError: true, Content: "this command matches the deny list and will never run; use a safer alternative"}
	case cmdAllowed:
		// pre-approved by allowlist; no prompt
	default:
		if t.r.Approve != nil && !t.r.Approve("shell", command) {
			return Result{IsError: true, Content: "user denied this command; propose an alternative or ask what to do"}
		}
	}
	if note := t.r.runHooks(ctx, "pre_shell", "COMMAND", command); note != "" {
		return Result{IsError: true, Content: "pre_shell hook blocked or failed:\n" + note}
	}
	timeout := argInt(args, 120, "timeout_seconds", "timeout")
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 600 {
		timeout = 600
	}
	out, err := RunShell(ctx, t.r.Root, command, time.Duration(timeout)*time.Second)
	if err != nil {
		return Result{IsError: true, Content: truncate(out+"\n[exit error] "+err.Error(), t.r.MaxOutput)}
	}
	if strings.TrimSpace(out) == "" {
		out = "(command succeeded with no output)"
	}
	return Result{Content: truncate(out, t.r.MaxOutput)}
}

type cmdClass int

const (
	cmdPrompt cmdClass = iota
	cmdAllowed
	cmdDenied
)

// classifyCommand matches a command against deny (first) then allow glob
// patterns. Patterns use path.Match semantics against the whole command
// string ("go *", "rm -rf *", "sudo *").
//
// Compound commands (;, &&, ||, |, &, newlines, redirects, backticks, $())
// are never auto-allowed: "go build*" must not approve "go build; rm -rf ~".
// Deny patterns are checked against every segment of a compound so a denied
// command cannot hide behind an allowed prefix either.
func classifyCommand(command string, allow, deny []string) cmdClass {
	segments := shellSegments(command)
	for _, p := range deny {
		for _, seg := range segments {
			if globMatch(p, seg) {
				return cmdDenied
			}
		}
	}
	if isCompoundCommand(command) {
		return cmdPrompt
	}
	for _, p := range allow {
		if globMatch(p, command) {
			return cmdAllowed
		}
	}
	return cmdPrompt
}

// shellControlOps are the operators that chain, pipe, background, or
// redirect — anything that lets a second command ride on the first.
var shellControlOps = []string{"&&", "||", ";", "|", "&", "\n", ">", "<", "`", "$("}

func isCompoundCommand(command string) bool {
	for _, op := range shellControlOps {
		if strings.Contains(command, op) {
			return true
		}
	}
	return false
}

// shellSegments splits a command on control operators (whole command first)
// so deny globs can be matched against each piece.
func shellSegments(command string) []string {
	out := []string{command}
	cur := []string{command}
	for _, op := range []string{"&&", "||", ";", "|", "&", "\n", "`", "$("} {
		var next []string
		for _, c := range cur {
			next = append(next, strings.Split(c, op)...)
		}
		cur = next
	}
	for _, c := range cur {
		if c = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(c), ")")); c != "" && c != command {
			out = append(out, c)
		}
	}
	return out
}

// globMatch is a simple glob where '*' matches ANY run of characters
// (including spaces and slashes, unlike filepath.Match) and matching is
// anchored at both ends.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// RunShell executes a command line under the platform shell, merging
// stdout/stderr the way a developer sees it. Exported for the verify package.
func RunShell(ctx context.Context, dir, command string, timeout time.Duration) (string, error) {
	if runtime.GOOS == "windows" {
		return runCmd(ctx, dir, timeout, "powershell", "-NoProfile", "-Command", command)
	}
	return runCmd(ctx, dir, timeout, "sh", "-c", command)
}

// RunArgv executes one program directly, with no shell between the caller
// and the argument vector: nothing in args is ever word-split, globbed,
// substituted or quoted away, on any platform. Tools that build a command
// from model-supplied text (the git-backed lookups) must use this rather
// than composing a shell line, because quoting rules differ between sh and
// PowerShell and any mismatch is an injection. Timeout, process-group kill
// and merged stdout/stderr behave exactly as in RunShell.
func RunArgv(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	return runCmd(ctx, dir, timeout, name, args...)
}

// runCmd is the shared body of RunShell and RunArgv: the timeout, the
// process-group teardown and the merged output buffer live here so the two
// entry points cannot drift apart.
func runCmd(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// Kill the whole process group on timeout/cancel, and stop waiting on
	// the output pipes shortly after even if a grandchild still holds them.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return buf.String(), fmt.Errorf("timed out after %s", timeout)
	}
	return buf.String(), err
}
