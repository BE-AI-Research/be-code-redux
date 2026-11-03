package workledger

import (
	"os"
	"testing"
	"time"
)

// A session's running jobs ride its record, written at once by Refresh,
// and another session reads them back through Others.
func TestRefreshWritesTheRunningJobsAtOnce(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	l := &Ledger{opt: Options{Enabled: true, PID: os.Getpid()}, ws: ws, dir: dir, seen: NewSeen()}
	l.rec = Record{PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339), Workspace: ws}
	jobs := []string{"2.3"}
	l.SetJobs(func() []string { return jobs })
	l.Refresh()
	got := Others(dir, ws, os.Getpid()+1)
	if len(got) != 1 || len(got[0].Jobs) != 1 || got[0].Jobs[0] != "2.3" {
		t.Fatalf("records: %+v", got)
	}
	// A second Refresh inside the heartbeat still writes: the list changed.
	jobs = nil
	l.Refresh()
	got = Others(dir, ws, os.Getpid()+1)
	if len(got) != 1 || len(got[0].Jobs) != 0 {
		t.Fatalf("after the job ended: %+v", got)
	}
}
