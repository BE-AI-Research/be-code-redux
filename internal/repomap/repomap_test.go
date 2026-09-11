package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildExtractsSymbols(t *testing.T) {
	ws := t.TempDir()
	files := map[string]string{
		"main.go":           "package main\n\nfunc main() {}\n\nfunc helperFunc() {}\n\ntype ServerConfig struct{}\n",
		"lib/util.py":       "class Helper:\n    pass\n\ndef process_data(x):\n    return x\n",
		"web/app.ts":        "export function renderApp() {}\nexport class Store {}\nexport const useThing = () => {}\n",
		"src/lib.rs":        "pub fn compute() {}\npub struct Engine {}\n",
		"README.md":         "# hello\n",
		"node_modules/x.go": "package x\nfunc ShouldNotAppear() {}\n",
	}
	for rel, content := range files {
		p := filepath.Join(ws, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	m := Build(ws, 0)
	for _, want := range []string{"helperFunc", "ServerConfig", "Helper", "process_data", "renderApp", "Store", "useThing", "compute", "Engine"} {
		if !strings.Contains(m, want) {
			t.Errorf("map missing %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "ShouldNotAppear") {
		t.Error("node_modules leaked into the map")
	}
}

func TestBuildRespectsBudget(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(ws, filepath.Clean(strings.Repeat("x", 1)+string(rune('a'+i%26))+".go")+".go"),
			[]byte("package p\nfunc SomeVeryLongFunctionNameForBudgetTesting() {}\n"), 0o644)
	}
	m := Build(ws, 300)
	if len(m) > 400 {
		t.Fatalf("map over budget: %d bytes", len(m))
	}
}

func TestEmptyWorkspace(t *testing.T) {
	if m := Build(t.TempDir(), 0); m != "" {
		t.Fatalf("expected empty map, got %q", m)
	}
}
