package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
)

// The service is built for every interactive session but starts only when
// asked; a start with api.enabled off is a no-op, /api on works regardless.
func TestAPIServiceStartsOnlyWhenAsked(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "beapi")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := config.Default()
	ag, _ := testAgentFor(t, cfg, stubProvider{}, "m")
	svc := buildAPI(cfg, ag)
	t.Cleanup(svc.Stop)
	svc.StartIfEnabled()
	if st := svc.Status(); st.Serving {
		t.Fatal("api.enabled is off: nothing serves")
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	st := svc.Status()
	if !st.Serving || st.Socket == "" {
		t.Fatalf("after Start the session serves: %+v", st)
	}
	dir, _ := api.Dir()
	if rec, ok := api.Serving(dir); !ok || rec.PID != os.Getpid() {
		t.Fatal("the record names this process")
	}
	svc.Stop()
	svc.Stop() // idempotent
	if _, ok := api.Serving(dir); ok {
		t.Fatal("Stop releases the record")
	}
	if _, err := os.Stat(filepath.Join(dir, "api.sock")); err == nil {
		t.Fatal("Stop removes the socket")
	}
}

func TestAPIDoctorLines(t *testing.T) {
	home, _ := os.MkdirTemp("/tmp", "beapi")
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := config.Default()
	lines := strings.Join(apiDoctorLines(cfg), "\n")
	if !strings.Contains(lines, "api: off") || !strings.Contains(lines, "apps registered: 0") {
		t.Fatalf("doctor says the API is off and counts apps:\n%s", lines)
	}
}
