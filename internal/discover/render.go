package discover

import (
	"fmt"
	"strings"
)

// Markdown renders the fact sheet the model writes from (and the fallback
// BECODE.md when the model's overview is rejected).
func (f Facts) Markdown() string {
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
	if f.RepoMap != "" {
		fmt.Fprintf(&b, "\n## Symbols\n%s\n", f.RepoMap)
	}
	return b.String()
}

// Names lists everything the model may legitimately cite.
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
	out = append(out, f.Checks...)
	return out
}
