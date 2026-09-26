package browser

import (
	"strconv"
	"strings"
	"sync"
)

// RefTable gives each element a short ref ("e14") that stays the same for
// the life of a page load, so the model can act on something it saw two
// snapshots ago (spec §2.2). A navigation resets it. A pending dialog's
// accept/dismiss refs come from the same counter, so they never collide
// with an element's.
type RefTable struct {
	mu              sync.Mutex
	byNode          map[int]string
	byRef           map[string]int
	next            int
	accept, dismiss string
}

// NewRefTable returns an empty table.
func NewRefTable() *RefTable {
	t := &RefTable{}
	t.Reset()
	return t
}

// Reset forgets every ref and restarts the numbering; called when the page
// navigates to a new document.
func (t *RefTable) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byNode, t.byRef, t.next = map[int]string{}, map[string]int{}, 0
	t.accept, t.dismiss = "", ""
}

// Ref returns the node's ref, assigning the next one on first sight.
func (t *RefTable) Ref(backend int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.byNode[backend]; ok {
		return r
	}
	t.next++
	r := "e" + strconv.Itoa(t.next)
	t.byNode[backend], t.byRef[r] = r, backend
	return r
}

// Lookup returns the backend node a ref names, however loosely written.
func (t *RefTable) Lookup(ref string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.byRef[NormalizeRef(ref)]
	return b, ok
}

// DialogRefs returns the accept and dismiss refs for the pending dialog,
// allocating them once per dialog.
func (t *RefTable) DialogRefs() (accept, dismiss string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.accept == "" {
		t.next++
		t.accept = "e" + strconv.Itoa(t.next)
		t.next++
		t.dismiss = "e" + strconv.Itoa(t.next)
	}
	return t.accept, t.dismiss
}

// DialogAction reports whether ref is the pending dialog's accept (true) or
// dismiss (false) ref; ok is false for any other ref.
func (t *RefTable) DialogAction(ref string) (accept, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := NormalizeRef(ref)
	switch {
	case r == "":
		return false, false
	case r == t.accept:
		return true, true
	case r == t.dismiss:
		return false, true
	}
	return false, false
}

// ClearDialog forgets the dialog refs once the dialog has closed.
func (t *RefTable) ClearDialog() {
	t.mu.Lock()
	t.accept, t.dismiss = "", ""
	t.mu.Unlock()
}

// NormalizeRef accepts a ref the way a small model writes it — "e14",
// "E14", "14", "[e14]", "ref=e14" — and returns "e14". Blank stays blank.
func NormalizeRef(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimPrefix(s, "ref="))
	s = strings.TrimSpace(strings.Trim(s, "[]"))
	if s == "" {
		return ""
	}
	if s[0] >= '0' && s[0] <= '9' {
		return "e" + s
	}
	return s
}
