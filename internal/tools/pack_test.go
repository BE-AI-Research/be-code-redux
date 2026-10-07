package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakePack stands in for a loaded pack. The tool is defined against this
// interface so internal/tools does not import internal/packs — packs needs
// verify, and verify needs tools.
type fakePack struct {
	name      string
	index     string
	chunks    map[string]string
	templates map[string]string
}

func (f *fakePack) PackName() string { return f.name }
func (f *fakePack) Index() string    { return f.index }
func (f *fakePack) ChunkNames() []string {
	var out []string
	for n := range f.chunks {
		out = append(out, n)
	}
	return out
}
func (f *fakePack) Chunk(name string) (string, error) {
	if b, ok := f.chunks[name]; ok {
		return b, nil
	}
	return "", errNoSuch
}
func (f *fakePack) Template(rel string) (string, error) {
	if b, ok := f.templates[rel]; ok {
		return b, nil
	}
	return "", errNoSuch
}

var errNoSuch = &packErr{"no such thing in this pack"}

type packErr struct{ s string }

func (e *packErr) Error() string { return e.s }

func packRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(t.TempDir(), func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	r.SetPacks([]PackSource{&fakePack{
		name:  "ue5",
		index: "Pack: ue5 0.1.0\nAvailable chunks (fetch with the pack tool):\n  ue5/cpp-reflection — UCLASS and friends\n",
		chunks: map[string]string{
			"cpp-reflection": "# reflection\nUCLASS rules.",
		},
		templates: map[string]string{
			"templates/Actor.h.tmpl": "// {{MODULE_API}} actor header",
		},
	}})
	return r
}

func runPack(t *testing.T, r *Registry, args map[string]any) Result {
	t.Helper()
	tool, ok := r.Tool("pack")
	if !ok {
		t.Fatal("pack tool is not registered")
	}
	return tool.Run(context.Background(), args)
}

// A pack's templates cannot be read with read_file: a pack usually lives in
// ~/.be-code/packs, outside the workspace, and every file tool confines to
// the workspace root. Serving them here is what makes a pack's commands
// ("read {pack}/templates/Actor.h.tmpl") work at all.
func TestPackToolServesATemplate(t *testing.T) {
	r := packRegistry(t)
	// Both the literal path inside the pack and the natural short form: a
	// pack keeps its templates in templates/, so repeating the word is the
	// mistake anyone would make, and "pack ue5/template/Actor.h.tmpl" is what
	// a command file should be able to say.
	for _, name := range []string{
		"ue5/template/templates/Actor.h.tmpl",
		"ue5/template/Actor.h.tmpl",
		"ue5:template/Actor.h.tmpl",
	} {
		res := runPack(t, r, map[string]any{"name": name})
		if res.IsError || !strings.Contains(res.Content, "MODULE_API") {
			t.Errorf("%q did not resolve: %+v", name, res)
		}
	}
	// A path that is not there still names what went wrong.
	if res := runPack(t, r, map[string]any{"name": "ue5/template/Nope.tmpl"}); !res.IsError {
		t.Error("a missing template must be an error")
	}
}

func TestPackToolServesChunksAndTheIndex(t *testing.T) {
	r := packRegistry(t)
	if res := runPack(t, r, map[string]any{"name": "ue5/cpp-reflection"}); res.IsError || !strings.Contains(res.Content, "UCLASS rules") {
		t.Fatalf("chunk: %+v", res)
	}
	if res := runPack(t, r, map[string]any{"name": "ue5"}); res.IsError || !strings.Contains(res.Content, "Available chunks") {
		t.Fatalf("index: %+v", res)
	}
}

// Forgiving argument parsing, as everywhere else: a small model writes the
// name it half-remembers.
func TestPackToolResolvesNamesForgivingly(t *testing.T) {
	r := packRegistry(t)
	for _, name := range []string{
		"ue5/cpp-reflection",
		"ue5:cpp-reflection",
		"cpp-reflection", // unique across the active packs
		"ue5/chunks/cpp-reflection.md",
	} {
		if res := runPack(t, r, map[string]any{"name": name}); res.IsError || !strings.Contains(res.Content, "UCLASS rules") {
			t.Errorf("%q did not resolve: %+v", name, res)
		}
	}
}

// An error has to say what the model could have asked for instead, or a weak
// model guesses again with no new information.
func TestPackToolErrorsNameTheAlternatives(t *testing.T) {
	r := packRegistry(t)
	res := runPack(t, r, map[string]any{"name": "ue5/nonexistent"})
	if !res.IsError {
		t.Fatal("expected an error")
	}
	if !strings.Contains(res.Content, "cpp-reflection") {
		t.Errorf("error does not list what is available: %q", res.Content)
	}
	if res := runPack(t, r, map[string]any{"name": "godot/anything"}); !res.IsError || !strings.Contains(res.Content, "ue5") {
		t.Errorf("an unknown pack must name the active ones: %+v", res)
	}
	if res := runPack(t, r, map[string]any{}); !res.IsError {
		t.Error("a missing name must be an error")
	}
}

// With no packs active the tool is not registered at all, so its schema never
// reaches the prompt and the model is not told about something it cannot use.
func TestPackToolAbsentWithoutPacks(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Tool("pack"); ok {
		t.Fatal("pack tool registered with no packs active")
	}
}

// During a scheduled event there is nobody to ask, so a gated tool is a tool
// the model cannot use. pack only returns text out of a pack the person
// installed — no execution, and the path cannot leave the pack — so it is
// self-gated like read_file rather than asking tool_call.
func TestPackToolIsExemptDuringAFiredTurn(t *testing.T) {
	if !firedExempt(&packTool{}) {
		t.Error("the pack tool should not need approval during a scheduled event")
	}
	// The name alone must not be enough: something else registered as "pack"
	// is still gated.
	if firedExempt(&fakeNamed{"pack"}) {
		t.Error("a foreign tool named \"pack\" must still be gated")
	}
}

type fakeNamed struct{ n string }

func (f *fakeNamed) Name() string                               { return f.n }
func (f *fakeNamed) Description() string                        { return "" }
func (f *fakeNamed) Schema() json.RawMessage                    { return nil }
func (f *fakeNamed) Run(context.Context, map[string]any) Result { return Result{} }
