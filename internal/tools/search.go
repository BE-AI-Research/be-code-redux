package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// searchTool is a built-in grep so BE-Code needs no external ripgrep —
// important for a tool meant to run fully offline on minimal machines.
type searchTool struct{ r *Registry }

func (t *searchTool) Name() string { return "search" }
func (t *searchTool) Description() string {
	return "Search file contents with a regular expression. Returns matching lines as path:line:text. Use glob to limit file types (e.g. *.go)."
}
func (t *searchTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"pattern":{"type":"string","description":"Go/RE2 regular expression"},
		"path":{"type":"string","description":"Directory to search (default: workspace root)"},
		"glob":{"type":"string","description":"Filename glob filter, e.g. *.go or *.ts"},
		"max_results":{"type":"integer","description":"Cap on matches (default 100)"}},
		"required":["pattern"]}`)
}

const maxSearchFileSize = 2 * 1024 * 1024

func (t *searchTool) Run(ctx context.Context, args map[string]any) Result {
	pat := argString(args, "pattern", "query", "regex")
	if pat == "" {
		return Result{IsError: true, Content: "pattern is required"}
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("bad regex: %v", err)}
	}
	dir := argString(args, "path", "dir")
	if dir == "" {
		dir = "."
	}
	abs, err := t.r.resolve(dir)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	glob := argString(args, "glob", "include")
	maxResults := argInt(args, 100, "max_results", "limit")

	var out []string
	count := 0
	stop := fmt.Errorf("stop")
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return stop
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxSearchFileSize {
			return nil
		}
		f, ferr := os.Open(p)
		if ferr != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(t.r.Root, p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if strings.ContainsRune(line, 0) {
				return nil // binary file
			}
			if re.MatchString(line) {
				if len(line) > 300 {
					line = line[:300] + "..."
				}
				out = append(out, fmt.Sprintf("%s:%d:%s", rel, lineNo, line))
				count++
				if count >= maxResults {
					return stop
				}
			}
		}
		return nil
	})
	if err != nil && err != stop {
		return Result{IsError: true, Content: err.Error()}
	}
	if len(out) == 0 {
		return Result{Content: "no matches"}
	}
	header := fmt.Sprintf("%d matches", count)
	if count >= maxResults {
		header += " (capped; narrow the pattern or glob)"
	}
	return Result{Content: truncate(header+"\n"+strings.Join(out, "\n"), t.r.MaxOutput())}
}
