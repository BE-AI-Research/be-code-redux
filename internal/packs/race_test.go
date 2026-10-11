package packs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Spec §2: this is the *mechanical* race — two processes' read-modify-write
// interleave and one delta is lost. Both deltas were wanted and re-reading
// recovers both, so it retries and never prompts.
//
// The interleaving is driven, not hoped for: beforeWriteHook fires after this
// mutate has read and before it writes, and the hook writes the other key, as
// another process would.
func TestMutateDoesNotLoseAConcurrentDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	if err := Trust(path, "first", "hash-1"); err != nil {
		t.Fatal(err)
	}

	// While this SetForced is between its read and its write, another writer
	// records a trust. Without compare-and-retry, that trust is lost.
	once := false
	beforeWriteHook = func() {
		if once {
			return
		}
		once = true
		// A second process: its own read-modify-write, completed inside ours.
		if err := trustOutOfBand(path, "second", "hash-2"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWriteHook = nil })

	if err := SetForced(path, "a-pack", false); err != nil {
		t.Fatal(err)
	}

	if got := TrustedHash(path, "second"); got != "hash-2" {
		t.Errorf("the concurrent writer's trust was lost: %q", got)
	}
	if on, set := Forced(path, "a-pack"); !set || on {
		t.Errorf("our own delta was lost: on=%v set=%v", on, set)
	}
	if got := TrustedHash(path, "first"); got != "hash-1" {
		t.Errorf("the pre-existing trust was lost: %q", got)
	}
}

// The retry is bounded: a file being rewritten continuously must error rather
// than spin.
func TestMutateGivesUpRatherThanSpinning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	beforeWriteHook = func() {
		// Every attempt is interfered with, for ever.
		_ = trustOutOfBand(path, "noisy", randomHash())
	}
	t.Cleanup(func() { beforeWriteHook = nil })

	err := SetForced(path, "a-pack", true)
	if err == nil {
		t.Fatal("mutate succeeded against an endlessly changing file; it must bound its retries")
	}
	if !strings.Contains(err.Error(), "packs.json") {
		t.Errorf("the error should name the file: %v", err)
	}
}

// trustOutOfBand is another process's read-modify-write: it does not take
// trustMu, because a second process does not share it.
func trustOutOfBand(path, pack, hash string) error {
	st := readState(path)
	if st.Trusted == nil {
		st.Trusted = map[string]string{}
	}
	st.Trusted[pack] = hash
	return writeState(path, st)
}

var hashSeq int

func randomHash() string {
	hashSeq++
	return "h" + strconv.Itoa(hashSeq)
}

// What mutate wrote is what this session last saw, or the work ledger's
// record of the trust file is stale from the moment it is written.
func TestMutateNotesWhatItWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	var last []byte
	saved := OnStateRead
	OnStateRead = func(_ string, b []byte) { last = append([]byte(nil), b...) }
	t.Cleanup(func() { OnStateRead = saved })
	if err := Trust(path, "p", "h1"); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(last) != string(onDisk) {
		t.Errorf("the hook last saw %q, the file holds %q", last, onDisk)
	}
}
