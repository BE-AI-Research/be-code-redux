package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// repeatTracker notices a model going round in circles: the same tool, the
// same arguments, the same answer, again. toolFailStreak only sees failures;
// a loop of successful reads and greps that each "confirm" what the model
// already knew is just as stuck and costs the same context.
//
// A call is only a repeat when its *result* is unchanged too. Running the
// tests again after an edit is the same call made on purpose, and the moment
// its output moves the count starts over.
type repeatTracker struct {
	seen map[string]*repeatEntry
}

type repeatEntry struct {
	result [sha256.Size]byte
	n      int
}

// repeatCap bounds the map; a session that has made this many distinct calls
// starts counting afresh rather than growing without limit.
const repeatCap = 256

// repeatFrom is the count at which the result says so.
const repeatFrom = 3

// repeatExempt are tools whose repetition is bookkeeping or is already
// handled: the task record, a consultation, a long-running process being
// polled.
var repeatExempt = map[string]bool{"task": true, "consult": true, "process": true}

// note records one finished call and returns the footer to append, or "".
func (r *repeatTracker) note(call provider.ToolCall, res tools.Result) string {
	if repeatExempt[call.Name] {
		return ""
	}
	key := call.Name + "\x00" + canonicalArgs(call.Arguments)
	sum := sha256.Sum256([]byte(res.Content))
	if r.seen == nil || len(r.seen) >= repeatCap {
		r.seen = map[string]*repeatEntry{}
	}
	e := r.seen[key]
	if e == nil || e.result != sum {
		r.seen[key] = &repeatEntry{result: sum, n: 1}
		return ""
	}
	e.n++
	if e.n < repeatFrom {
		return ""
	}
	return fmt.Sprintf("(you have made this exact call %d times and the result has not changed; making it again will not help. "+
		"Use what it says, try a different approach, or record what is blocking you with a task note.)", e.n)
}

// canonicalArgs makes spacing and key order irrelevant. Arguments that do not
// parse are compared as written.
func canonicalArgs(raw string) string {
	args, ok := tools.ParseArgs(raw)
	if !ok {
		return strings.TrimSpace(raw)
	}
	b, err := json.Marshal(args) // map keys are marshalled in sorted order
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return string(b)
}
