package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestFileWriteApprovalFlow(t *testing.T) {
	dir := t.TempDir()
	var captured []string
	allow := true
	r, err := NewRegistry(dir, func(action, detail string) bool {
		captured = append(captured, action+"\n"+detail)
		return allow
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveWrites = true
	ctx := context.Background()

	// New file: approval sees a new-file preview; write proceeds on yes.
	res := r.Dispatch(ctx, provider.ToolCall{Name: "write_file",
		Arguments: `{"path":"a.go","content":"package a\n"}`})
	if res.IsError {
		t.Fatalf("approved write failed: %s", res.Content)
	}
	if len(captured) != 1 || !strings.Contains(captured[0], "file_write") ||
		!strings.Contains(captured[0], "+package a") {
		t.Fatalf("approval prompt wrong: %v", captured)
	}

	// Edit: approval sees a unified diff of the change.
	captured = nil
	res = r.Dispatch(ctx, provider.ToolCall{Name: "edit_file",
		Arguments: `{"path":"a.go","old_text":"package a","new_text":"package b"}`})
	if res.IsError {
		t.Fatalf("approved edit failed: %s", res.Content)
	}
	if len(captured) != 1 || !strings.Contains(captured[0], "-package a") ||
		!strings.Contains(captured[0], "+package b") {
		t.Fatalf("diff preview wrong: %v", captured)
	}

	// Denied write leaves the file untouched and tells the model.
	allow = false
	res = r.Dispatch(ctx, provider.ToolCall{Name: "write_file",
		Arguments: `{"path":"a.go","content":"package c\n"}`})
	if !res.IsError || !strings.Contains(res.Content, "rejected") {
		t.Fatalf("denied write result: %v", res)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if string(data) != "package b\n" {
		t.Fatalf("file modified despite denial: %q", data)
	}
}

func TestApproveWritesOffMeansSilent(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	r, _ := NewRegistry(dir, func(action, detail string) bool { calls++; return true })
	// ApproveWrites left false: writes never prompt; shell still does.
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "write_file",
		Arguments: `{"path":"x.txt","content":"hi"}`})
	if res.IsError || calls != 0 {
		t.Fatalf("write should be silent: calls=%d res=%v", calls, res)
	}
	r.Dispatch(context.Background(), provider.ToolCall{Name: "shell",
		Arguments: `{"command":"echo hi"}`})
	if calls != 1 {
		t.Fatalf("shell should still prompt: calls=%d", calls)
	}
}
