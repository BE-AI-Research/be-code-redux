package live

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordSaveLoadListPrune(t *testing.T) {
	dir := t.TempDir()
	me := Record{Code: "ABC123", PID: os.Getpid(), Socket: SocketPath(dir, "ABC123"), Workspace: "/w", Model: "m", StartedAt: time.Now(), Token: "t"}
	if err := me.Save(dir); err != nil {
		t.Fatal(err)
	}
	dead := Record{Code: "DEAD00", PID: 999999999, Socket: SocketPath(dir, "DEAD00"), Token: "x"}
	dead.Save(dir)
	os.WriteFile(dead.Socket, nil, 0o600) // stale socket file
	got, err := Load(dir, "ABC123")
	if err != nil || got.Token != "t" || got.Workspace != "/w" {
		t.Fatalf("load: %+v %v", got, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "ABC123.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %o", info.Mode().Perm())
	}
	live, err := List(dir)
	if err != nil || len(live) != 1 || live[0].Code != "ABC123" {
		t.Fatalf("list: %+v %v", live, err)
	}
	if _, err := os.Stat(dead.Socket); !os.IsNotExist(err) {
		t.Fatal("stale socket not removed")
	}
	if err := Remove(dir, "ABC123"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "ABC123"); err == nil {
		t.Fatal("record still loadable after Remove")
	}
}

// LiveCode is what every "is this session already running?" check goes
// through, so a record left behind by a host that is gone must not answer
// yes (Load alone would).
func TestLiveCodeIgnoresRecordsWhoseHostIsGone(t *testing.T) {
	dir := t.TempDir()
	alive := Record{Code: "ABC123", PID: os.Getpid(), Socket: SocketPath(dir, "ABC123"), Token: "t"}
	if err := alive.Save(dir); err != nil {
		t.Fatal(err)
	}
	dead := Record{Code: "DEAD00", PID: 999999999, Socket: SocketPath(dir, "DEAD00"), Token: "x"}
	if err := dead.Save(dir); err != nil {
		t.Fatal(err)
	}
	if got := LiveCode(dir, "ABC123"); got == nil || got.Token != "t" {
		t.Fatalf("live code = %+v", got)
	}
	if got := LiveCode(dir, "DEAD00"); got != nil {
		t.Fatalf("dead host's code = %+v, want nil", got)
	}
	if got := LiveCode(dir, "NOPE11"); got != nil {
		t.Fatalf("unknown code = %+v, want nil", got)
	}
}
