// Package discover measures a workspace so BECODE.md can be written from
// facts rather than from the model's guesses: languages, build and test
// commands, layout, entry points, tooling, git state, README and repo map.
package discover

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
	"github.com/brown-enterprises/be-code/internal/repomap"
	"github.com/brown-enterprises/be-code/internal/verify"
)

type LangCount struct {
	Ext   string
	Files int
}
type DirCount struct {
	Path  string
	Files int
}
type GitState struct {
	Branch, Remote string
	Recent         []string
	Dirty          int
}

type Facts struct {
	Root        string
	Languages   []LangCount
	Kind        string
	Checks      []string
	KeyFiles    []string
	Layout      []DirCount
	EntryPoints []string
	TestDirs    []string
	Tooling     []string
	// MakeTargets are the target names declared by the makefiles in
	// MakeFiles, so `make build` and `make -f build.mk verify` count as
	// measured commands (see Commands).
	MakeTargets []string
	MakeFiles   []string
	// NPMScripts and NPMBins come from package.json, for `npm run <script>`
	// and `npx <bin>`.
	NPMScripts []string
	NPMBins    []string
	// FilePaths are the discovered files, in walk order, bounded to
	// maxNamedFiles: the spec's "every discovered file" for validation,
	// without letting a huge repo blow up the names list.
	FilePaths    []string
	Git          GitState
	ReadmeHead   string
	RepoMap      string
	Files, Lines int
	Truncated    bool
}

var (
	maxFiles  = 20000
	maxWalk   = 2 * time.Second
	makeFiles = []string{"Makefile", "makefile", "GNUmakefile", "build.mk"}
	skipDirs  = map[string]bool{".git": true, "vendor": true, "node_modules": true, "dist": true, "build": true, "target": true, ".venv": true, "__pycache__": true, ".idea": true, ".vscode": true}
	sourceExt = map[string]bool{".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".py": true, ".rs": true, ".java": true, ".kt": true, ".c": true, ".h": true, ".cpp": true, ".cs": true, ".rb": true, ".sh": true, ".md": true, ".yaml": true, ".yml": true, ".toml": true, ".json": true, ".sql": true, ".html": true, ".css": true}
	keyFiles  = []string{"README", "LICENSE", "CONTRIBUTING", "Makefile", "build.mk", "go.mod", "package.json", "pyproject.toml", "Cargo.toml", "Dockerfile", "docker-compose"}
	toolFiles = []string{".golangci.yml", ".golangci.yaml", ".eslintrc", ".eslintrc.json", ".eslintrc.js", ".prettierrc", ".prettierrc.json", "rustfmt.toml", ".editorconfig", "tsconfig.json", ".pre-commit-config.yaml"}
	testDirs  = map[string]bool{"test": true, "tests": true, "e2e": true, "__tests__": true, "spec": true}
)

// Scan measures root. Errors are only returned for an unreadable root; a
// partial scan sets Truncated.
func Scan(root string) (Facts, error) {
	root, _ = filepath.Abs(root)
	if _, err := os.Stat(root); err != nil {
		return Facts{}, err
	}
	f := Facts{Root: root}
	langs := map[string]int{}
	dirs := map[string]int{}
	goTests := map[string]int{}
	ignoreDirs := gitignoreDirs(root)
	start := time.Now()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Since(start) > maxWalk {
			f.Truncated = true
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if p != root && isTopLevel(rel) && ignoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			if p != root && testDirs[d.Name()] {
				f.TestDirs = append(f.TestDirs, filepath.ToSlash(rel))
			}
			return nil
		}
		if f.Files >= maxFiles {
			f.Truncated = true
			return filepath.SkipAll
		}
		f.Files++
		if len(f.FilePaths) < maxNamedFiles {
			f.FilePaths = append(f.FilePaths, filepath.ToSlash(rel))
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != "" {
			langs[ext]++
		}
		if sourceExt[ext] {
			f.Lines += countLines(p)
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 1 {
			dirs[parts[0]]++
			if len(parts) > 2 {
				dirs[parts[0]+"/"+parts[1]]++
			}
		}
		if strings.HasSuffix(p, "_test.go") {
			goTests[filepath.ToSlash(filepath.Dir(rel))]++
		}
		if len(parts) == 1 {
			for _, k := range keyFiles {
				match := strings.HasPrefix(d.Name(), k)
				if !match && k == "README" {
					match = strings.HasPrefix(strings.ToLower(d.Name()), "readme")
				}
				if match {
					f.KeyFiles = append(f.KeyFiles, d.Name())
				}
			}
			for _, k := range toolFiles {
				if d.Name() == k || strings.HasPrefix(d.Name(), k+".") {
					f.Tooling = append(f.Tooling, d.Name())
				}
			}
		}
		if strings.HasPrefix(filepath.ToSlash(rel), ".github/workflows/") && (ext == ".yml" || ext == ".yaml") {
			f.KeyFiles = append(f.KeyFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	for ext, n := range langs {
		f.Languages = append(f.Languages, LangCount{ext, n})
	}
	sort.Slice(f.Languages, func(i, j int) bool {
		if f.Languages[i].Files != f.Languages[j].Files {
			return f.Languages[i].Files > f.Languages[j].Files
		}
		return f.Languages[i].Ext < f.Languages[j].Ext
	})
	for d, n := range dirs {
		f.Layout = append(f.Layout, DirCount{d, n})
	}
	sort.Slice(f.Layout, func(i, j int) bool { return f.Layout[i].Path < f.Layout[j].Path })
	for d, n := range goTests {
		f.TestDirs = append(f.TestDirs, fmt.Sprintf("%s (%d _test.go)", d, n))
	}
	sort.Strings(f.TestDirs)
	sort.Strings(f.KeyFiles)
	sort.Strings(f.Tooling)

	proj := verify.Detect(root)
	f.Kind = proj.Kind
	for _, c := range proj.Checks {
		f.Checks = append(f.Checks, c.Command)
	}
	f.EntryPoints = entryPoints(root)
	f.MakeFiles, f.MakeTargets = makeTargets(root)
	f.NPMScripts, f.NPMBins = nodePackage(root)
	f.Tooling = append(f.Tooling, tomlTooling(root)...)
	f.Git = gitState(root)
	f.ReadmeHead = readmeHead(root)
	f.RepoMap = repomap.Build(root, 8000)
	return f, nil
}

// maxLineBytes caps how much of one file countLines will read, so a single
// giant generated file (a lockfile, a bundled asset) cannot stall the scan.
const maxLineBytes = 4 << 20

// maxNamedFiles bounds Facts.FilePaths: every discovered file is a citable
// name, but a 20,000-file repo must not turn the names list into megabytes.
const maxNamedFiles = 400

// maxReadBytes caps every whole-file read the scan makes (README,
// package.json, pyproject.toml, main.go, the makefiles): a generated or
// pathological file in one of those places must not be pulled into memory
// whole.
const maxReadBytes = 1 << 20

// readCapped reads at most maxReadBytes of p. A missing or unreadable file
// is an error, exactly as os.ReadFile would report it, so callers keep the
// `if b, err := …; err == nil` shape.
func readCapped(p string) ([]byte, error) {
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b := make([]byte, maxReadBytes)
	n, err := io.ReadFull(fh, b)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return b[:n], nil
}

// makeTargets parses target names out of the makefiles at the root, so the
// commands a project documents (`make build`, `make -f build.mk verify`)
// validate as measured. It is a deliberately shallow parse: a line whose
// first token is a plain name followed by ":" (not ":=", not a special
// .TARGET, not a `%` pattern rule) declares a target.
func makeTargets(root string) (files []string, targets []string) {
	seen := map[string]bool{}
	for _, name := range makeFiles {
		b, err := readCapped(filepath.Join(root, name))
		if err != nil {
			continue
		}
		files = append(files, name)
		for _, line := range strings.Split(string(b), "\n") {
			if line == "" || line[0] == '\t' || line[0] == ' ' || line[0] == '#' {
				continue
			}
			i := strings.Index(line, ":")
			if i <= 0 || strings.Contains(line[:i], "%") {
				continue
			}
			// ":=" / "::=" is a variable assignment, not a target.
			if rest := line[i+1:]; strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":=") {
				continue
			}
			name := strings.TrimSpace(line[:i])
			if name == "" || strings.ContainsAny(name, " \t$") || strings.HasPrefix(name, ".") {
				continue // .PHONY and friends, pattern lists, variable refs
			}
			if !plainTarget.MatchString(name) || seen[name] {
				continue
			}
			seen[name] = true
			targets = append(targets, name)
		}
	}
	sort.Strings(targets)
	return files, targets
}

var plainTarget = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// nodePackage lists package.json's script and bin names for `npm run …`
// and `npx …`.
func nodePackage(root string) (scripts []string, bins []string) {
	b, err := readCapped(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, nil
	}
	var pkg struct {
		Bin     json.RawMessage   `json:"bin"`
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return nil, nil
	}
	for n := range pkg.Scripts {
		scripts = append(scripts, n)
	}
	var binMap map[string]string
	if json.Unmarshal(pkg.Bin, &binMap) == nil {
		for n := range binMap {
			bins = append(bins, n)
		}
	}
	sort.Strings(scripts)
	sort.Strings(bins)
	return scripts, bins
}

func countLines(p string) int {
	fh, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer fh.Close()
	n := 0
	read := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		n++
		read += len(sc.Bytes()) + 1
		if read >= maxLineBytes {
			break
		}
	}
	return n
}

// isTopLevel reports whether rel (a path relative to the scan root) names a
// direct child of the root, i.e. it contains no path separator.
func isTopLevel(rel string) bool {
	return !strings.Contains(filepath.ToSlash(rel), "/")
}

// gitignoreDirs parses simple root-scope directory patterns out of
// <root>/.gitignore: a line naming a directory (trailing "/", or a bare
// name that exists as a directory at the root) is skipped for this scan.
// Negated lines and glob patterns are left alone — this is not a full
// gitignore matcher, only the common top-level "build output" case the
// spec calls out.
func gitignoreDirs(root string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		name := strings.TrimPrefix(line, "/")
		isDirPattern := strings.HasSuffix(name, "/")
		name = strings.TrimSuffix(name, "/")
		if name == "" || strings.Contains(name, "/") {
			continue // only simple root-level names
		}
		if isDirPattern {
			out[name] = true
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil && fi.IsDir() {
			out[name] = true
		}
	}
	return out
}

// entryPoints finds where the project starts: Go main packages under cmd/
// or the root, package.json main/bin/scripts, pyproject scripts, src/index.*.
func entryPoints(root string) []string {
	var out []string
	if entries, err := os.ReadDir(filepath.Join(root, "cmd")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, "cmd/"+e.Name())
			}
		}
	}
	if b, err := readCapped(filepath.Join(root, "main.go")); err == nil && strings.Contains(string(b), "package main") {
		out = append(out, "main.go")
	}
	if b, err := readCapped(filepath.Join(root, "package.json")); err == nil {
		var pkg struct {
			Main    string            `json:"main"`
			Bin     json.RawMessage   `json:"bin"`
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			if pkg.Main != "" {
				out = append(out, pkg.Main)
			}
			var bins map[string]string
			if json.Unmarshal(pkg.Bin, &bins) == nil {
				names := make([]string, 0, len(bins))
				for n := range bins {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					out = append(out, "bin "+n)
				}
			}
			names := make([]string, 0, len(pkg.Scripts))
			for n := range pkg.Scripts {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				out = append(out, "script "+n)
			}
		}
	}
	if b, err := readCapped(filepath.Join(root, "pyproject.toml")); err == nil {
		in := false
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				in = line == "[project.scripts]"
				continue
			}
			if in && strings.Contains(line, "=") {
				out = append(out, "script "+strings.TrimSpace(strings.SplitN(line, "=", 2)[0]))
			}
		}
	}
	for _, idx := range []string{"src/index.ts", "src/index.js", "src/main.ts", "src/main.py", "app.py", "main.py"} {
		if _, err := os.Stat(filepath.Join(root, idx)); err == nil && !containsStr(out, idx) {
			out = append(out, idx)
		}
	}
	return out
}

func tomlTooling(root string) []string {
	var out []string
	if b, err := readCapped(filepath.Join(root, "pyproject.toml")); err == nil {
		for _, sec := range []string{"[tool.black]", "[tool.ruff]", "[tool.isort]", "[tool.mypy]"} {
			if strings.Contains(string(b), sec) {
				out = append(out, "pyproject "+sec)
			}
		}
	}
	return out
}

func gitState(root string) GitState {
	var g GitState
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return g
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		procattr.Hide(cmd)
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	g.Branch = run("rev-parse", "--abbrev-ref", "HEAD")
	g.Remote = run("remote", "get-url", "origin")
	if log := run("log", "-5", "--format=%s"); log != "" {
		g.Recent = strings.Split(log, "\n")
	}
	if st := run("status", "--porcelain"); st != "" {
		g.Dirty = len(strings.Split(st, "\n"))
	}
	return g
}

var readmeNames = regexp.MustCompile(`(?i)^readme(\.md|\.rst|\.txt)?$`)

func readmeHead(root string) string {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() && readmeNames.MatchString(e.Name()) {
			b, err := readCapped(filepath.Join(root, e.Name()))
			if err != nil {
				return ""
			}
			lines := strings.Split(string(b), "\n")
			if len(lines) > 60 {
				lines = lines[:60]
			}
			return strings.Join(lines, "\n")
		}
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
