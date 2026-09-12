package live

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpawnHostDetachesAndWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "x.sock")
	// A stand-in "host": sleep long enough for the check, in its own session.
	pid, err := SpawnHost("/bin/sh", "-c", filepath.Join(dir, "log"), []string{"BE_TEST=1"}, "sleep 2")
	if err != nil {
		t.Skip("cannot spawn detached process here: " + err.Error())
	}
	if !processAlive(pid) {
		t.Fatal("spawned process not alive")
	}
	go func() { time.Sleep(100 * time.Millisecond); os.WriteFile(sock, nil, 0o600) }()
	if err := WaitForSocket(sock, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitForSocket(filepath.Join(dir, "never.sock"), 150*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestNewTokenIsUniqueAndHex(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a == b {
		t.Fatal("tokens repeat")
	}
	if len(a) != 48 {
		t.Fatalf("token length %d, want 48 hex chars", len(a))
	}
	for _, c := range a {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("non-hex character %q in token %q", c, a)
		}
	}
}
