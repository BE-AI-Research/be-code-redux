package packs

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/tools"
)

// The names the ue5 pack's own command files tell the model to use must be
// the names the pack tool answers to. The two are written in different
// places — pack content in one repository, the tool in this one — so this is
// the test that keeps them honest.
func TestCommandFileNamesResolveThroughTheTool(t *testing.T) {
	root := testdataPack(t)
	p, warns, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	r, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPacks([]tools.PackSource{p})
	tool, ok := r.Tool("pack")
	if !ok {
		t.Fatal("pack tool not registered")
	}
	run := func(name string) tools.Result {
		return tool.Run(context.Background(), map[string]any{"name": name})
	}

	// Exactly as ue-actor.md and ue-component.md now say.
	for _, name := range []string{
		"ue5/template/Actor.h.tmpl",
		"ue5/template/Actor.cpp.tmpl",
		"ue5/template/ActorComponent.h.tmpl",
		"ue5/template/ActorComponent.cpp.tmpl",
	} {
		res := run(name)
		if res.IsError || strings.TrimSpace(res.Content) == "" {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// And as ue-dep.md and ue-diagnose.md say.
	for _, name := range []string{"ue5/build-system", "ue5/cpp-reflection"} {
		res := run(name)
		if res.IsError || len(res.Content) < 500 {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// The templates really are unreachable the old way, which is why the
	// command files had to change.
	if rf, ok := r.Tool("read_file"); ok {
		res := rf.Run(context.Background(), map[string]any{"path": root + "/templates/Actor.h.tmpl"})
		if !res.IsError {
			t.Error("read_file reached into a pack outside the workspace")
		}
	}
}
