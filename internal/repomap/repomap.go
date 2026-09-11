// Package repomap builds a compact symbol-level outline of the workspace
// for injection into the system prompt. Small local models waste turns
// exploring; handing them the map up front converts exploration turns into
// editing turns (the Aider repo-map insight, regex edition — no tree-sitter
// so the binary stays dependency-free and fully offline).
package repomap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"vendor": true, "__pycache__": true, ".venv": true, "target": true,
	".idea": true, ".becode": true,
}

type extractor struct {
	re    *regexp.Regexp
	group int
}

var extractors = map[string][]extractor{
	".go": {
		{regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?([A-Za-z_]\w*)`), 0},
		{regexp.MustCompile(`(?m)^type\s+([A-Za-z_]\w*)`), 0},
	},
	".py": {
		{regexp.MustCompile(`(?m)^(?:async\s+)?def\s+([A-Za-z_]\w*)`), 0},
		{regexp.MustCompile(`(?m)^class\s+([A-Za-z_]\w*)`), 0},
	},
	".js":  jsExtractors,
	".jsx": jsExtractors,
	".ts":  jsExtractors,
	".tsx": jsExtractors,
	".rs": {
		{regexp.MustCompile(`(?m)^\s*(?:pub\s+)?fn\s+([A-Za-z_]\w*)`), 0},
		{regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:struct|enum|trait)\s+([A-Za-z_]\w*)`), 0},
		{regexp.MustCompile(`(?m)^impl(?:<[^>]*>)?\s+([A-Za-z_]\w*)`), 0},
	},
	".java": {
		{regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:class|interface|enum)\s+([A-Za-z_]\w*)`), 0},
	},
	".c":   cExtractors,
	".h":   cExtractors,
	".cpp": cExtractors,
	".hpp": cExtractors,
}

var jsExtractors = []extractor{
	{regexp.MustCompile(`(?m)^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), 0},
	{regexp.MustCompile(`(?m)^(?:export\s+)?class\s+([A-Za-z_$][\w$]*)`), 0},
	{regexp.MustCompile(`(?m)^(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\(|function)`), 0},
}

var cExtractors = []extractor{
	{regexp.MustCompile(`(?m)^[A-Za-z_][\w\s\*]*?\b([A-Za-z_]\w*)\s*\([^;]*$`), 0},
	{regexp.MustCompile(`(?m)^(?:typedef\s+)?struct\s+([A-Za-z_]\w*)`), 0},
}

const (
	maxFileSize    = 512 * 1024
	maxSymbolsFile = 12
	defaultBudget  = 6 * 1024 // bytes of map text
	maxFilesWalked = 3000
)

// Build walks root and returns the outline, capped at budget bytes
// (<=0 uses the default). Returns "" for unrecognized/empty workspaces.
func Build(root string, budget int) string {
	if budget <= 0 {
		budget = defaultBudget
	}
	type entry struct {
		rel  string
		syms []string
	}
	var entries []entry
	walked := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		walked++
		if walked > maxFilesWalked {
			return fmt.Errorf("cap")
		}
		exts, ok := extractors[strings.ToLower(filepath.Ext(d.Name()))]
		if !ok {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxFileSize {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		src := string(data)
		var syms []string
		seen := map[string]bool{}
		for _, ex := range exts {
			for _, match := range ex.re.FindAllStringSubmatch(src, -1) {
				name := match[len(match)-1]
				if name == "" || seen[name] || name == "init" || name == "main" && len(syms) > 0 {
					continue
				}
				seen[name] = true
				syms = append(syms, name)
				if len(syms) >= maxSymbolsFile {
					break
				}
			}
			if len(syms) >= maxSymbolsFile {
				break
			}
		}
		rel, _ := filepath.Rel(root, p)
		if len(syms) > 0 {
			entries = append(entries, entry{rel: rel, syms: syms})
		} else {
			entries = append(entries, entry{rel: rel})
		}
		return nil
	})
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	var b strings.Builder
	for _, e := range entries {
		line := e.rel
		if len(e.syms) > 0 {
			line += ": " + strings.Join(e.syms, ", ")
		}
		if b.Len()+len(line)+1 > budget {
			b.WriteString(fmt.Sprintf("... (%d more files)\n", len(entries)))
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
