package tools

import (
	"fmt"
	"strings"
)

// maxCapturedOutput bounds how much of a command's combined output is kept in
// memory. A pack's verify command is usually a build, and its default timeout
// is 30 minutes: a failing verbose Unreal or webpack build emits hundreds of
// megabytes, which used to be held whole and then split line by line on the
// unrecognised-failure path. 2 MiB is far more than any diagnostic needs and
// bounds both the memory and the parse time, which runs at roughly a third of
// a second per megabyte.
const maxCapturedOutput = 2 << 20

// headShare is how much of the cap is kept from the start. Compiler and
// linker errors lead and test failures trail, so both ends are worth keeping
// — the same reasoning verify.headTail already applies to what reaches the
// model.
const headShare = 3 * maxCapturedOutput / 4

// cappedBuffer collects a command's output, keeping the first headShare bytes
// and the last of the remainder, and counting what it dropped in between.
// Below the cap it is byte-exact, because every parser reads this output.
type cappedBuffer struct {
	head    []byte
	tail    []byte // ring of the most recent bytes, once head is full
	start   int    // ring start, valid once wrapped
	wrapped bool
	dropped int64
}

const tailCap = maxCapturedOutput - headShare

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	// Fill the head first.
	if len(b.head) < headShare {
		take := headShare - len(b.head)
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	// Everything after that goes into a ring of the newest tailCap bytes.
	for len(p) > 0 {
		if b.tail == nil {
			b.tail = make([]byte, 0, tailCap)
		}
		if len(b.tail) < tailCap {
			take := tailCap - len(b.tail)
			if take > len(p) {
				take = len(p)
			}
			b.tail = append(b.tail, p[:take]...)
			p = p[take:]
			continue
		}
		// Full: overwrite oldest.
		b.wrapped = true
		for _, c := range p {
			b.tail[b.start] = c
			b.start = (b.start + 1) % tailCap
			b.dropped++
		}
		p = nil
	}
	return n, nil
}

// String is the collected output, with an explicit marker where bytes were
// dropped so a reader — model or person — can tell it is partial.
func (b *cappedBuffer) String() string {
	if !b.wrapped && b.dropped == 0 {
		return string(b.head) + string(b.tail)
	}
	var sb strings.Builder
	sb.Write(b.head)
	fmt.Fprintf(&sb, "\n... [%d bytes elided: output exceeded %d bytes] ...\n", b.dropped, maxCapturedOutput)
	// The ring, oldest first.
	sb.Write(b.tail[b.start:])
	sb.Write(b.tail[:b.start])
	return sb.String()
}
