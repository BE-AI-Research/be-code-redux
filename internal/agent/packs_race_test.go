package agent

import (
	"sync"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
)

// Activating a pack rewrites shared state from whichever goroutine ran the
// command: it writes packNotes/packChecks/packEntries, mutates the registry's
// tool slice in place (SetPacks adds and removes the pack tool), and rebuilds
// the system prompt. Meanwhile a prefill goroutine composes the system prompt
// — reading packNotes — and reads the tool specs off the same slice.
//
// Mutating the registry's tools mid-session is new to this branch, and /packs
// now runs on its own goroutine (it has to: waiting on the trust approval
// under the session lock deadlocked the TUI), so this is reachable in
// ordinary use: switch a pack on while a prefill is in flight.
//
// Run under -race; without it this test passes while the bug is present.
func TestActivatingAPackRacesNothing(t *testing.T) {
	ag := packAgent(t)
	p1 := loadTestPack(t, "one", "rules one", "", packs.SourceGlobal)
	p2 := loadTestPack(t, "two", "rules two", "", packs.SourceGlobal)

	var wg sync.WaitGroup
	// Bounded on both sides: an unbounded reader loop starves the writer and
	// the test runs to the package timeout instead of finishing.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				ag.recomposeSystem("")
				_ = ag.Tools.Specs()
				_ = ag.Tools.Names()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			if i%2 == 0 {
				ag.EnablePacks([]*packs.Pack{p1})
			} else {
				ag.EnablePacks([]*packs.Pack{p1, p2})
			}
		}
	}()
	wg.Wait()
}

// The same shape through the public entry point a UI uses, so the lock covers
// the path /packs actually takes rather than only EnablePacks.
func TestLoadPacksRacesNothing(t *testing.T) {
	ag := packAgent(t)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ag.recomposeSystem("")
				_ = ag.Tools.Specs()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			ag.LoadPacks()
		}
	}()
	wg.Wait()
}
