package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PackSource is one active pack, as the pack tool needs to see it. The
// interface lives here rather than the concrete type because internal/packs
// depends on internal/verify, which depends on this package: a pack cannot be
// imported here without a cycle. It is also the whole of what the model can
// reach inside a pack — text, by name — which is the point.
type PackSource interface {
	PackName() string
	// Index is the pack's one-line-per-chunk listing.
	Index() string
	ChunkNames() []string
	Chunk(name string) (string, error)
	// Template serves one of the pack's own files. The implementation
	// confines the path to the pack's directory; see packs.Pack.read.
	Template(rel string) (string, error)
}

// SetPacks installs the active packs and registers the pack tool, or removes
// it when there are none — a tool in the schema that can answer nothing wastes
// prompt on every request and invites the model to call it.
func (r *Registry) SetPacks(ps []PackSource) {
	r.packs = ps
	_, had := r.Tool("pack")
	switch {
	case len(ps) > 0 && !had:
		r.AddTool(&packTool{r: r})
	case len(ps) == 0 && had:
		r.removeTool("pack")
	}
}

// Packs are the active packs, for the caller assembling the system prompt.
func (r *Registry) Packs() []PackSource { return r.packs }

type packTool struct{ r *Registry }

func (t *packTool) Name() string { return "pack" }

func (t *packTool) Description() string {
	var names []string
	for _, p := range t.r.packs {
		names = append(names, p.PackName())
	}
	sort.Strings(names)
	return "Fetch a pack's knowledge: a chunk by name (\"ue5/cpp-reflection\"), a pack's index of chunks (\"ue5\"), or one of its template files (\"ue5/template/templates/Actor.h.tmpl\"). Pack files cannot be read with read_file. Active packs: " + strings.Join(names, ", ") + "."
}

func (t *packTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"name":{"type":"string","description":"pack/chunk, a pack name on its own, or pack/template/<path>"}},
		"required":["name"]}`)
}

func (t *packTool) Run(_ context.Context, args map[string]any) Result {
	name := strings.TrimSpace(argString(args, "name", "chunk", "pack", "path"))
	if name == "" {
		return Result{IsError: true, Content: "name is required: " + t.whatIsAvailable()}
	}
	// Forgiving, like every other tool here: a weak model writes the name it
	// half-remembers, and a refusal that only says "no" costs a whole turn.
	ref := strings.ReplaceAll(strings.Trim(strings.TrimSpace(name), "/"), ":", "/")
	head, rest, _ := strings.Cut(ref, "/")

	pack := t.find(head)
	if pack == nil {
		// Not a pack name, so the whole thing may be a chunk name that is
		// unique across the active packs.
		if p, chunk := t.findChunk(ref); p != nil {
			return t.chunk(p, chunk)
		}
		return Result{IsError: true, Content: fmt.Sprintf("no pack or chunk named %q. %s", name, t.whatIsAvailable())}
	}
	switch {
	case rest == "":
		return Result{Content: pack.Index()}
	case rest == "index":
		return Result{Content: pack.Index()}
	case rest == "playbook":
		// Reserved for the playbook query form (spec §7.1); not in this build.
		return Result{IsError: true, Content: fmt.Sprintf("pack %q cannot be queried for playbook entries in this build", pack.PackName())}
	default:
		if after, ok := strings.CutPrefix(rest, "template/"); ok {
			body, err := pack.Template(after)
			if err != nil {
				return Result{IsError: true, Content: fmt.Sprintf("pack %q has no template %q: %v", pack.PackName(), after, err)}
			}
			return Result{Content: truncate(body, t.r.MaxOutput())}
		}
		return t.chunk(pack, rest)
	}
}

func (t *packTool) chunk(p PackSource, name string) Result {
	// "chunks/cpp-reflection.md" is how the name appears in a manifest, and a
	// model that has seen one writes it back.
	name = strings.TrimSuffix(strings.TrimPrefix(name, "chunks/"), ".md")
	body, err := p.Chunk(name)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("pack %q has no chunk %q; it has %s",
			p.PackName(), name, strings.Join(sorted(p.ChunkNames()), ", "))}
	}
	return Result{Content: truncate(body, t.r.MaxOutput())}
}

func (t *packTool) find(name string) PackSource {
	for _, p := range t.r.packs {
		if strings.EqualFold(p.PackName(), name) {
			return p
		}
	}
	return nil
}

// findChunk resolves a bare chunk name, but only when exactly one active pack
// has it: guessing between two packs' advice would be worse than asking.
func (t *packTool) findChunk(name string) (PackSource, string) {
	name = strings.TrimSuffix(strings.TrimPrefix(name, "chunks/"), ".md")
	var found PackSource
	for _, p := range t.r.packs {
		for _, c := range p.ChunkNames() {
			if c == name {
				if found != nil {
					return nil, ""
				}
				found = p
			}
		}
	}
	return found, name
}

func (t *packTool) whatIsAvailable() string {
	var parts []string
	for _, p := range t.r.packs {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.PackName(), strings.Join(sorted(p.ChunkNames()), ", ")))
	}
	sort.Strings(parts)
	return "Active packs: " + strings.Join(parts, "; ") + "."
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
