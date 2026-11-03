package workledger

import (
	"os"
	"sync"
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

// Concurrent forced writes land in the order they were built: the record
// on disk at the end is the one built last, never an earlier list that a
// slower goroutine saved after it.
func TestConcurrentRefreshesSaveInOrder(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	l := &Ledger{opt: Options{Enabled: true, PID: os.Getpid()}, ws: ws, dir: dir, seen: NewSeen()}
	l.rec = Record{PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339), Workspace: ws}
	var mu sync.Mutex
	jobs := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	l.SetJobs(func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), jobs...)
	})
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mu.Lock()
				if len(jobs) > 0 {
					jobs = jobs[1:]
				}
				mu.Unlock()
				l.Refresh()
			}()
		}
		wg.Wait()
		got := Others(dir, ws, os.Getpid()+1)
		if len(got) != 1 || len(got[0].Jobs) != 0 {
			t.Fatalf("round %d: an earlier record overtook the last: %+v", round, got)
		}
		mu.Lock()
		jobs = []string{"1", "2", "3", "4", "5", "6", "7", "8"}
		mu.Unlock()
	}
}
