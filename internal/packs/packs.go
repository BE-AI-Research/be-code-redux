// Package packs loads a pack: a folder of Markdown and JSON holding domain
// expertise, which plugs into the mechanisms the harness already has
// (BE-SPEC-CODE-PACKS-2026-001). A pack brings a small block of always-on
// rules, knowledge the model fetches one chunk at a time, verification
// commands and a parser to read their output, and templates.
//
// Nothing here executes anything. A pack names a verify command and names one
// of the compiled-in parsers; it can never ship code that runs inside this
// process. What a pack's commands are allowed to do is decided elsewhere, by
// the trust gate and by the deny rules in internal/verify and internal/tools.
//
// Loading is forgiving about content and strict about structure: a chunk whose
// file is missing is dropped with a warning, because the rest of the pack is
// still worth having, while a manifest that cannot be understood is an error,
// because nothing after it can be trusted.
package packs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// schemaVersion is the manifest layout this package understands. A pack
// declaring a newer major is refused rather than guessed at.
const schemaVersion = 1

var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Chunk is one piece of on-demand knowledge.
type Chunk struct {
	Name     string
	Summary  string
	Triggers []string
	file     string // relative to the pack root
}

// Pack is one loaded pack.
type Pack struct {
	Name        string
	Version     string
	Description string
	Root        string // absolute path to the pack's own directory
	Source      Source // where it came from, which decides how far it is trusted
	Core        string // contents of core.md, already read
	CoreBudget  int    // budget_tokens for Core, 0 when unset

	chunks     map[string]Chunk
	checks     []manifestCheck
	detect     detect
	mcpServers map[string]any
}

type manifest struct {
	Schema      int                   `json:"schema"`
	Name        string                `json:"name"`
	Version     string                `json:"version"`
	Description string                `json:"description"`
	Detect      detect                `json:"detect"`
	Core        core                  `json:"core"`
	Chunks      map[string]chunkEntry `json:"chunks"`
	Verify      []manifestCheck       `json:"verify"`
	Playbooks   []string              `json:"playbooks"`
	Commands    string                `json:"commands"`
	MCPServers  map[string]any        `json:"mcp_servers"`
	Requires    map[string]string     `json:"requires"`
}

type detect struct {
	Any []string `json:"any"`
	All []string `json:"all"`
}

type core struct {
	File         string `json:"file"`
	BudgetTokens int    `json:"budget_tokens"`
}

type chunkEntry struct {
	File     string   `json:"file"`
	Summary  string   `json:"summary"`
	Triggers []string `json:"triggers"`
}

type manifestCheck struct {
	Name       string   `json:"name"`
	Cmd        []string `json:"cmd"`
	CmdWindows []string `json:"cmd_windows"`
	Parser     string   `json:"parser"`
	Timeout    string   `json:"timeout"`
	When       struct {
		Changed []string `json:"changed"`
	} `json:"when"`
}

// Load reads the pack rooted at dir. Warnings name content that was dropped —
// a chunk whose file is gone — and are the pack author's to fix; err means the
// manifest itself could not be used, and nothing was loaded.
func Load(dir string) (*Pack, []string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "pack.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read pack.json in %s: %w", root, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("pack.json in %s is not valid JSON: %w", root, err)
	}
	if m.Schema != schemaVersion {
		return nil, nil, fmt.Errorf("pack %q declares schema %d; this build understands schema %d", m.Name, m.Schema, schemaVersion)
	}
	if !nameRe.MatchString(m.Name) {
		return nil, nil, fmt.Errorf("pack name %q is not usable: lower-case letters, digits and hyphens only", m.Name)
	}

	p := &Pack{
		Name: m.Name, Version: m.Version, Description: m.Description,
		Root: root, CoreBudget: m.Core.BudgetTokens,
		chunks: map[string]Chunk{}, detect: m.Detect, mcpServers: m.MCPServers,
	}

	var warns []string
	if m.Core.File != "" {
		body, err := p.read(m.Core.File)
		if err != nil {
			warns = append(warns, fmt.Sprintf("pack %q: core file %q could not be read: %v", p.Name, m.Core.File, err))
		} else {
			p.Core = strings.TrimSpace(body)
		}
	}
	for name, c := range m.Chunks {
		if !nameRe.MatchString(name) {
			warns = append(warns, fmt.Sprintf("pack %q: chunk name %q is not usable; dropped", p.Name, name))
			continue
		}
		if _, err := p.read(c.File); err != nil {
			warns = append(warns, fmt.Sprintf("pack %q: chunk %q (%s) could not be read: %v; dropped", p.Name, name, c.File, err))
			continue
		}
		p.chunks[name] = Chunk{Name: name, Summary: c.Summary, Triggers: c.Triggers, file: c.File}
	}

	// Verify entries are validated here, where the file they were written in
	// can be named, rather than at the point one is run.
	for i, mc := range m.Verify {
		if len(mc.Cmd) == 0 {
			return nil, warns, fmt.Errorf("pack %q: verify[%d] (%s) has no cmd", p.Name, i, mc.Name)
		}
		if err := verify.ValidateParser(mc.Parser); err != nil {
			return nil, warns, fmt.Errorf("pack %q: verify[%d] (%s): %w", p.Name, i, mc.Name, err)
		}
		if _, err := parseTimeout(mc.Timeout); err != nil {
			return nil, warns, fmt.Errorf("pack %q: verify[%d] (%s): timeout %q: %w", p.Name, i, mc.Name, mc.Timeout, err)
		}
		p.checks = append(p.checks, mc)
	}
	return p, warns, nil
}

// coreCharsPerToken is the harness's own starting estimate, before any
// calibration from a server's reported token counts. The check uses this
// fixed figure on purpose: the live estimate is calibrated per session, so a
// pack whose core sat near its budget would load against one model and fail
// against another, or load and then stop loading mid-session. A budget that
// means something has to mean the same thing every time.
const coreCharsPerToken = 3

// CoreOverBudget reports whether a pack's core is larger than the budget it
// declared, with the estimate and the budget for the message. A pack with no
// budget declares no limit.
func CoreOverBudget(p *Pack) (over bool, estimate, budget int) {
	estimate = len(p.Core) / coreCharsPerToken
	if p.CoreBudget <= 0 {
		return false, estimate, 0
	}
	return estimate > p.CoreBudget, estimate, p.CoreBudget
}

// PackName is the pack's name, as tools.PackSource asks for it. The method
// cannot be Name: Pack already has a Name field, and the field is what every
// caller in this package reads.
func (p *Pack) PackName() string { return p.Name }

// A loaded pack is what the pack tool serves, so the two must agree. Checked
// here rather than in tools, which cannot import this package: it needs
// verify, and verify needs tools.
var _ tools.PackSource = (*Pack)(nil)

// ChunkNames lists the pack's chunks, sorted, so the index and the tool's
// listing never shuffle between turns — the prompt cache pays for churn.
func (p *Pack) ChunkNames() []string {
	out := make([]string, 0, len(p.chunks))
	for n := range p.chunks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Index is the one-line-per-chunk listing the model is given alongside core,
// so it knows what it can ask for by name.
func (p *Pack) Index() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pack: %s %s", p.Name, p.Version)
	if p.Description != "" {
		fmt.Fprintf(&b, " — %s", p.Description)
	}
	b.WriteString("\n")
	if len(p.chunks) == 0 {
		return b.String()
	}
	b.WriteString("Available chunks (fetch with the pack tool):\n")
	for _, n := range p.ChunkNames() {
		fmt.Fprintf(&b, "  %s/%s — %s\n", p.Name, n, p.chunks[n].Summary)
	}
	return b.String()
}

// Chunk returns one chunk's text.
func (p *Pack) Chunk(name string) (string, error) {
	c, ok := p.chunks[name]
	if !ok {
		return "", fmt.Errorf("pack %q has no chunk %q; it has %s", p.Name, name, strings.Join(p.ChunkNames(), ", "))
	}
	return p.read(c.file)
}

// Template returns one of the pack's template files.
//
// The path comes from the model, and a pack normally lives outside the
// workspace (~/.be-code/packs/<name>), which is why this is served here at
// all: the file tools confine every read to the workspace root and so refuse
// a pack's own files. That makes confining to the pack's directory this
// function's job, and the reason it is the only way in.
func (p *Pack) Template(rel string) (string, error) {
	return p.read(rel)
}

// read resolves one of the pack's own files and reads it. Every path that
// reaches the filesystem from this package goes through here: a path is
// refused unless it stays inside the pack, with symlinks resolved, so neither
// "../../.ssh/id_rsa" nor an absolute path nor a link planted in the pack can
// read anything the pack does not own.
func (p *Pack) read(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("no file named")
	}
	// Backslashes are separators on Windows and ordinary characters
	// elsewhere, so a pack written on one and read on the other would escape
	// a check that only understood the local separator.
	clean := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("%q must be a path inside the pack, not an absolute one", rel)
	}
	full := filepath.Join(p.Root, clean)
	root := p.Root
	if r, err := filepath.EvalSymlinks(p.Root); err == nil {
		root = r
	}
	real := full
	if r, err := filepath.EvalSymlinks(full); err == nil {
		real = r
	}
	in, err := filepath.Rel(root, real)
	if err != nil || in == ".." || strings.HasPrefix(in, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside pack %q", rel, p.Name)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Checks turns the pack's verify entries into checks the verify package can
// run: argument vectors, never shell strings, with {pack} and {workspace}
// substituted. On Windows a cmd_windows entry replaces cmd.
func (p *Pack) Checks(workspace string) []verify.Check {
	out := make([]verify.Check, 0, len(p.checks))
	for _, mc := range p.checks {
		argv := mc.Cmd
		if runtimeIsWindows() && len(mc.CmdWindows) > 0 {
			argv = mc.CmdWindows
		}
		expanded := make([]string, len(argv))
		for i, a := range argv {
			a = strings.ReplaceAll(a, "{pack}", p.Root)
			a = strings.ReplaceAll(a, "{workspace}", workspace)
			expanded[i] = filepath.FromSlash(a)
		}
		timeout, _ := parseTimeout(mc.Timeout)
		out = append(out, verify.Check{
			Name: mc.Name, Argv: expanded, Parser: mc.Parser, Timeout: timeout,
		})
	}
	return out
}

// Changed lists the globs a check is gated on, by check name, for the caller
// that knows what changed. Empty means "always run".
func (p *Pack) Changed(checkName string) []string {
	for _, mc := range p.checks {
		if mc.Name == checkName {
			return mc.When.Changed
		}
	}
	return nil
}

// defaultCheckTimeout is generous because a pack's check is usually a build:
// a cold Unreal build is tens of minutes, and a check killed half way through
// teaches the model nothing.
const defaultCheckTimeout = 30 * time.Minute

func parseTimeout(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return defaultCheckTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}
