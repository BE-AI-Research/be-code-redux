package discover

import (
	"fmt"
	"strings"
)

// Markdown renders the fact sheet the model writes from: everything
// measured, including the repo map under "## Symbols".
func (f Facts) Markdown() string {
	return f.markdown(true)
}

// NotesFallback renders the fact sheet for use *as* BECODE.md when the
// model's overview was rejected twice. It differs from Markdown in two
// ways, both because this text enters the system prompt: the "## Symbols"
// repo map (only ever meant for the model's one-shot request) is left out,
// and the result is trimmed at a line boundary to fit limit bytes, so the
// notes are never cut off mid-sentence by the caller's cap. A limit of 0 or
// less means no trimming.
func (f Facts) NotesFallback(limit int) string {
	s := f.markdown(false)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		return cut[:i+1]
	}
	return cut
}

func (f Facts) markdown(symbols bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project facts (measured)\n\nRoot: %s\n", f.Root)
	if f.Files == 0 {
		b.WriteString("\nno source files found\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Files: %d, source lines: %d", f.Files, f.Lines)
	if f.Truncated {
		b.WriteString(" (scan truncated)")
	}
	b.WriteString("\n\n## Languages\n")
	for i, l := range f.Languages {
		if i == 12 {
			break
		}
		fmt.Fprintf(&b, "- %s: %d files\n", l.Ext, l.Files)
	}
	fmt.Fprintf(&b, "\n## Build and test\nKind: %s\n", f.Kind)
	for _, c := range f.Checks {
		fmt.Fprintf(&b, "- `%s`\n", c)
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	if len(f.MakeTargets) > 0 {
		fmt.Fprintf(&b, "\n## Make targets (%s)\n", strings.Join(f.MakeFiles, ", "))
		for _, t := range f.MakeTargets {
			fmt.Fprintf(&b, "- `%s`\n", t)
		}
	}
	section("Key files", f.KeyFiles)
	b.WriteString("\n## Layout\n")
	for _, d := range f.Layout {
		fmt.Fprintf(&b, "- %s/ (%d files)\n", d.Path, d.Files)
	}
	section("Entry points", f.EntryPoints)
	section("Tests", f.TestDirs)
	section("Tooling", f.Tooling)
	if f.Git.Branch != "" {
		fmt.Fprintf(&b, "\n## Git\nBranch: %s", f.Git.Branch)
		if f.Git.Remote != "" {
			fmt.Fprintf(&b, ", remote: %s", f.Git.Remote)
		}
		fmt.Fprintf(&b, ", uncommitted files: %d\n", f.Git.Dirty)
		for _, c := range f.Git.Recent {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if f.ReadmeHead != "" {
		fmt.Fprintf(&b, "\n## README (first lines)\n%s\n", f.ReadmeHead)
	}
	if symbols && f.RepoMap != "" {
		fmt.Fprintf(&b, "\n## Symbols\n%s\n", f.RepoMap)
	}
	return b.String()
}

// Names lists everything the model may legitimately cite: every measured
// file, directory and command (see Commands), as the spec promises.
func (f Facts) Names() []string {
	var out []string
	out = append(out, f.KeyFiles...)
	for _, d := range f.Layout {
		out = append(out, d.Path)
		if i := strings.Index(d.Path, "/"); i > 0 {
			out = append(out, d.Path[:i])
		}
	}
	out = append(out, f.EntryPoints...)
	for _, t := range f.TestDirs {
		out = append(out, strings.SplitN(t, " ", 2)[0])
	}
	out = append(out, f.Tooling...)
	out = append(out, f.Commands()...)
	out = append(out, f.FilePaths...)
	return out
}

// Commands lists the invocations this project actually supports, so the
// document may show them: verify.Detect's checks plus the ones every real
// project documents and no check string contains — `make <target>` for each
// measured makefile target, `npm run <script>`, `go run .`, `cargo test`,
// `pytest`. Validation matches a shown command against this list (see
// agent.ValidateProjectNotes), so a missing entry pushes a perfectly good
// document onto the fact-sheet fallback; err toward listing what the
// toolchain plainly offers.
func (f Facts) Commands() []string {
	out := append([]string{}, f.Checks...)
	for _, t := range f.MakeTargets {
		out = append(out, "make "+t)
		for _, mf := range f.MakeFiles {
			out = append(out, "make -f "+mf+" "+t)
		}
	}
	if f.hasKeyFile("package.json") {
		out = append(out, "npm install", "npm test", "npm ci")
		for _, s := range f.NPMScripts {
			out = append(out, "npm run "+s)
		}
		for _, b := range f.NPMBins {
			out = append(out, "npx "+b)
		}
	}
	if f.Kind == "go" || f.hasKeyFile("go.mod") {
		out = append(out, "go run .", "go build ./...", "go test ./...", "go vet ./...", "go mod tidy")
		for _, e := range f.EntryPoints {
			if strings.HasPrefix(e, "cmd/") {
				out = append(out, "go run ./"+e, "go build ./"+e)
			}
		}
		for _, d := range f.Layout {
			out = append(out, "go test ./"+d.Path, "go build ./"+d.Path)
		}
	}
	if f.Kind == "rust" || f.hasKeyFile("Cargo.toml") {
		out = append(out, "cargo build", "cargo test", "cargo run", "cargo check")
	}
	if (f.Kind == "python" || f.hasKeyFile("pyproject.toml")) && len(f.TestDirs) > 0 {
		out = append(out, "pytest", "python -m pytest", "python3 -m pytest")
	}
	return out
}

func (f Facts) hasKeyFile(name string) bool {
	for _, k := range f.KeyFiles {
		if k == name {
			return true
		}
	}
	return false
}
