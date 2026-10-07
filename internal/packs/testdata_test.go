package packs

import (
	"path/filepath"
	"testing"
)

// testdataPack is a copy of the shipped ue5 pack, so the tests do not depend
// on a checkout outside this repository.
func testdataPack(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "ue5"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
