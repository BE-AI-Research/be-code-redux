package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mentionRe finds @path tokens in user input (@internal/agent/loop.go).
var mentionRe = regexp.MustCompile(`@([\w~][\w./\\-]*)`)

const mentionFileCap = 16 * 1024

// ExpandMentions inlines @file contents into the request so the model
// starts with the right files pinned instead of hunting for them.
func ExpandMentions(root, input string) string {
	matches := mentionRe.FindAllStringSubmatch(input, -1)
	if len(matches) == 0 {
		return input
	}
	var blocks []string
	seen := map[string]bool{}
	for _, m := range matches {
		rel := filepath.Clean(m[1])
		if seen[rel] || strings.HasPrefix(rel, "..") {
			continue
		}
		abs := filepath.Join(root, rel)
		info, err := os.Stat(abs)
		if err != nil {
			continue // not a file; leave the token as plain text
		}
		seen[rel] = true
		if info.IsDir() {
			entries, _ := os.ReadDir(abs)
			var names []string
			for i, e := range entries {
				if i >= 50 {
					names = append(names, "...")
					break
				}
				names = append(names, e.Name())
			}
			blocks = append(blocks, fmt.Sprintf("<dir path=%q>\n%s\n</dir>", rel, strings.Join(names, "\n")))
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		note := ""
		if len(data) > mentionFileCap {
			data = data[:mentionFileCap]
			note = "\n... [truncated; use read_file with offset for more]"
		}
		blocks = append(blocks, fmt.Sprintf("<file path=%q>\n%s%s\n</file>", rel, string(data), note))
	}
	if len(blocks) == 0 {
		return input
	}
	return input + "\n\nPinned context (from @mentions):\n" + strings.Join(blocks, "\n")
}

// CompleteMention returns path completions for a partial @token, for UI
// autocomplete. prefix excludes the '@'.
func CompleteMention(root, prefix string) []string {
	dir, base := filepath.Split(prefix)
	absDir := filepath.Join(root, dir)
	if rel, err := filepath.Rel(root, absDir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil // never list outside the workspace
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && base == "" {
			continue
		}
		if strings.HasPrefix(name, base) {
			s := dir + name
			if e.IsDir() {
				s += "/"
			}
			out = append(out, s)
			if len(out) >= 25 {
				break
			}
		}
	}
	return out
}

// InitFrame is the system message for `init`: the model writes BECODE.md
// from the measured fact sheet and nothing else.
const InitFrame = `You are writing BECODE.md for the repository described below, so that a future coding session can orient itself. Describe THIS project only: what it is, how to build, test and run it (use the measured commands exactly), the layout of important directories, the conventions you can see, and gotchas. Do not describe BE-Code, its tools, its prompts, or how the assistant works. Do not invent commands, files or directories that are not in the facts. At most 150 lines of Markdown, no preamble.`

// planSystemPrompt replaces the normal system prompt during plan mode.
const planSystemPrompt = `You are BE-Code in PLANNING mode. You may ONLY inspect the project (read_file, list_dir, search) — you cannot modify anything.

Produce an implementation plan for the user's request:
1. A one-paragraph approach summary.
2. Numbered steps, each naming the exact file(s) to change and what changes.
3. How the result will be verified (build/test commands).
Keep the plan under 40 lines. When the plan is complete, output it and stop calling tools.`

// planExecutePrefix frames the approved plan for the execution phase.
const planExecutePrefix = "Execute this approved implementation plan. Follow it step by step; if reality contradicts the plan, adapt minimally and note the deviation in your final summary.\n\nRequest: %s\n\nApproved plan:\n%s"
