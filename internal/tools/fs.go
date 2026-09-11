package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brown-enterprises/be-code/internal/diff"
)

const defaultMaxOutput = 24 * 1024 // bytes returned to the model per call (Registry.MaxOutput)

// beforeWrite fires the checkpoint hook (if any) ahead of a modification.
func (r *Registry) beforeWrite(absPath string) error {
	if r.OnBeforeWrite == nil {
		return nil
	}
	return r.OnBeforeWrite(absPath)
}

// approveWrite shows a diff preview through the approval hook when write
// approvals are enabled. Returns (denialResult, false) when the user says no.
func (r *Registry) approveWrite(absPath, newContent string) (Result, bool) {
	if !r.ApproveWrites || r.Approve == nil {
		return Result{}, true
	}
	oldContent := ""
	if data, err := os.ReadFile(absPath); err == nil {
		oldContent = string(data)
	}
	rel, _ := filepath.Rel(r.Root, absPath)
	rejected := Result{IsError: true,
		Content: "user rejected this file change; ask what they want instead or take a different approach"}
	if r.ReviewWrite != nil {
		if r.OnStatus != nil {
			r.OnStatus("reviewing change in VS Code…")
		}
		d := r.ReviewWrite(rel, oldContent, newContent)
		if r.OnStatus != nil {
			r.OnStatus("")
		}
		switch d {
		case ReviewAccept:
			return Result{}, true
		case ReviewAcceptAll:
			r.ApproveWrites = false
			return Result{}, true
		case ReviewReject:
			return rejected, false
		}
		// ReviewUnavailable: fall through to the terminal prompt.
	}
	preview := diff.Preview(rel, oldContent, newContent, false)
	if r.Approve("file_write", preview) {
		return Result{}, true
	}
	return rejected, false
}

// ---- read_file -------------------------------------------------------------

type readFileTool struct{ r *Registry }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read a file. Returns numbered lines. Use offset/limit for large files."
}
func (t *readFileTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File path, relative to the workspace root"},
		"offset":{"type":"integer","description":"1-based first line to read (default 1)"},
		"limit":{"type":"integer","description":"Max lines to read (default 400)"}},
		"required":["path"]}`)
}
func (t *readFileTool) Run(_ context.Context, args map[string]any) Result {
	p, err := t.r.resolve(argString(args, "path", "file", "filename"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	lines := strings.Split(string(data), "\n")
	offset := argInt(args, 1, "offset", "start")
	limit := argInt(args, 400, "limit", "count")
	if offset < 1 {
		offset = 1
	}
	if offset > len(lines) {
		return Result{IsError: true, Content: fmt.Sprintf("offset %d beyond end of file (%d lines)", offset, len(lines))}
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%5d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "... (%d more lines; call read_file again with offset=%d)\n", len(lines)-end, end+1)
	}
	return Result{Content: truncate(b.String(), t.r.MaxOutput)}
}

// ---- write_file ------------------------------------------------------------

type writeFileTool struct{ r *Registry }

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Create or overwrite a file with the given content. Creates parent directories."
}
func (t *writeFileTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File path, relative to the workspace root"},
		"content":{"type":"string","description":"Full file content"}},
		"required":["path","content"]}`)
}
func (t *writeFileTool) Run(ctx context.Context, args map[string]any) Result {
	p, err := t.r.resolve(argString(args, "path", "file", "filename"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	content, has := argStringPresent(args, "content", "text", "data")
	if !has {
		return Result{IsError: true, Content: "write_file requires a 'content' argument (missing key would have written an empty file); pass the full file content"}
	}
	if res, ok := t.r.approveWrite(p, content); !ok {
		return res
	}
	if err := t.r.beforeWrite(p); err != nil {
		return Result{IsError: true, Content: "checkpoint failed: " + err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	rel, _ := filepath.Rel(t.r.Root, p)
	msg := fmt.Sprintf("wrote %d bytes to %s", len(content), rel)
	if note := t.r.runHooks(ctx, "post_write", "FILE", rel); note != "" {
		msg += "\n" + note
	}
	return Result{Content: msg}
}

// ---- edit_file -------------------------------------------------------------

type editFileTool struct{ r *Registry }

func (t *editFileTool) Name() string { return "edit_file" }
func (t *editFileTool) Description() string {
	return "Replace an exact text snippet in a file. old_text must appear exactly once (include surrounding lines to disambiguate)."
}
func (t *editFileTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File path, relative to the workspace root"},
		"old_text":{"type":"string","description":"Exact existing text to replace (must match exactly once)"},
		"new_text":{"type":"string","description":"Replacement text"}},
		"required":["path","old_text","new_text"]}`)
}
func (t *editFileTool) Run(ctx context.Context, args map[string]any) Result {
	p, err := t.r.resolve(argString(args, "path", "file", "filename"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	oldText := argString(args, "old_text", "old_string", "old", "search")
	newText := argString(args, "new_text", "new_string", "new", "replace")
	if oldText == "" {
		return Result{IsError: true, Content: "old_text is required and must not be empty"}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	s := string(data)
	n := strings.Count(s, oldText)
	switch {
	case n == 0:
		return Result{IsError: true, Content: "old_text not found in file. Read the file again and copy the text exactly, including whitespace."}
	case n > 1:
		return Result{IsError: true, Content: fmt.Sprintf("old_text matches %d locations; include more surrounding lines so it matches exactly once", n)}
	}
	updated := strings.Replace(s, oldText, newText, 1)
	if res, ok := t.r.approveWrite(p, updated); !ok {
		return res
	}
	if err := t.r.beforeWrite(p); err != nil {
		return Result{IsError: true, Content: "checkpoint failed: " + err.Error()}
	}
	if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	rel, _ := filepath.Rel(t.r.Root, p)
	msg := fmt.Sprintf("edited %s (1 replacement)", rel)
	if note := t.r.runHooks(ctx, "post_write", "FILE", rel); note != "" {
		msg += "\n" + note
	}
	return Result{Content: msg}
}

// ---- list_dir --------------------------------------------------------------

type listDirTool struct{ r *Registry }

func (t *listDirTool) Name() string { return "list_dir" }
func (t *listDirTool) Description() string {
	return "List a directory. Directories end with '/'. Skips common junk (node_modules, .git, dist)."
}
func (t *listDirTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"Directory path (default: workspace root)"},
		"recursive":{"type":"boolean","description":"Walk subdirectories (depth 4, default false)"}},
		"required":[]}`)
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"vendor": true, "__pycache__": true, ".venv": true, "target": true,
	".idea": true, ".vscode": false,
}

func (t *listDirTool) Run(_ context.Context, args map[string]any) Result {
	p := argString(args, "path", "dir", "directory")
	if p == "" {
		p = "."
	}
	abs, err := t.r.resolve(p)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	recursive := argBool(args, false, "recursive")
	var out []string
	maxDepth := 1
	if recursive {
		maxDepth = 4
	}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			rel, _ := filepath.Rel(abs, filepath.Join(dir, e.Name()))
			if e.IsDir() {
				if skipDirs[e.Name()] {
					continue
				}
				out = append(out, rel+"/")
				if depth < maxDepth {
					_ = walk(filepath.Join(dir, e.Name()), depth+1)
				}
			} else {
				info, ierr := e.Info()
				size := int64(0)
				if ierr == nil {
					size = info.Size()
				}
				out = append(out, fmt.Sprintf("%s (%d bytes)", rel, size))
			}
			if len(out) > 500 {
				out = append(out, "... (listing capped at 500 entries)")
				return fmt.Errorf("cap")
			}
		}
		return nil
	}
	_ = walk(abs, 1)
	if len(out) == 0 {
		return Result{Content: "(empty directory)"}
	}
	return Result{Content: truncate(strings.Join(out, "\n"), t.r.MaxOutput)}
}
