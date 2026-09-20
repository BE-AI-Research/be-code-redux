package engine

import (
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusTodo    Status = "todo"
	StatusDoing   Status = "doing"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusDropped Status = "dropped"
)

// terminal reports whether a status means the node will not be worked on
// again. blocked and dropped stay distinct all the way into the report:
// "could not" and "decided not to" are different facts.
func (s Status) terminal() bool {
	return s == StatusDone || s == StatusBlocked || s == StatusDropped
}

type Node struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Status   Status    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	Opened   time.Time `json:"opened"`
	Closed   time.Time `json:"closed,omitempty"`
	Children []*Node   `json:"children,omitempty"`
	Evidence Evidence  `json:"evidence,omitempty"`
}

type Tree struct {
	Roots []*Node `json:"roots"`
	// nudge is Limits.StepNudge, carried on the copy Render makes so the
	// renderer stays a function of the tree it is given. Never persisted.
	nudge int
}

// Add appends a child under parent ("" for a new root) and returns it. Ids
// are positional, so a node's id never changes while it exists: adding to
// one branch cannot renumber another.
func (t *Tree) Add(parent, text string) *Node {
	n := &Node{Text: strings.TrimSpace(text), Status: StatusTodo, Opened: time.Now()}
	var p *Node
	if parent != "" {
		p = t.Find(parent)
	}
	// An unknown parent must not lose the node: file it as a root.
	if p == nil {
		n.ID = strconv.Itoa(nextIndex(t.Roots))
		t.Roots = append(t.Roots, n)
		return n
	}
	n.ID = p.ID + "." + strconv.Itoa(nextIndex(p.Children))
	p.Children = append(p.Children, n)
	return n
}

// nextIndex is one past the highest index this generation has ever handed
// out, not one past its current size. They are the same until a node is
// removed — which adoption does to an unfiled step — and after that only
// this answer avoids handing a live sibling's id to a new node. It applies
// to roots as well as children: a root id that already names a live task
// would make Find resolve the wrong one, so a status change would close it.
func nextIndex(ns []*Node) int {
	max := 0
	for _, n := range ns {
		i := 0
		last := n.ID
		if dot := strings.LastIndexByte(last, '.'); dot >= 0 {
			last = last[dot+1:]
		}
		i, _ = strconv.Atoi(last)
		if i > max {
			max = i
		}
	}
	return max + 1
}

// Remove detaches one node (and anything under it) from its parent or from
// the roots. Surviving siblings keep their ids, which is why Add counts
// past the highest index rather than the child count.
func (t *Tree) Remove(n *Node) bool {
	if n == nil {
		return false
	}
	for i, r := range t.Roots {
		if r == n {
			t.Roots = append(t.Roots[:i], t.Roots[i+1:]...)
			return true
		}
	}
	var rec func(ns []*Node) bool
	rec = func(ns []*Node) bool {
		for _, p := range ns {
			for i, c := range p.Children {
				if c == n {
					p.Children = append(p.Children[:i], p.Children[i+1:]...)
					return true
				}
			}
			if rec(p.Children) {
				return true
			}
		}
		return false
	}
	return rec(t.Roots)
}

// Find is a value receiver so a copy handed out by Store.Tree() can be
// searched directly: tree.Find(id) reads the same either way.
func (t Tree) Find(id string) *Node {
	var found *Node
	t.Walk(func(n *Node, _ int) {
		if n.ID == id {
			found = n
		}
	})
	return found
}

// Walk visits every node, parents before children, in document order.
func (t *Tree) Walk(fn func(n *Node, depth int)) {
	var rec func(ns []*Node, depth int)
	rec = func(ns []*Node, depth int) {
		for _, n := range ns {
			fn(n, depth)
			rec(n.Children, depth+1)
		}
	}
	rec(t.Roots, 0)
}

func (t *Tree) Doing() *Node {
	var d *Node
	t.Walk(func(n *Node, _ int) {
		if n.Status == StatusDoing {
			d = n
		}
	})
	return d
}

// SetStatus moves one node. Exactly one node is doing at a time, so a new
// doing node sends the previous one back to todo; the caller (Task 2) is
// what distils it first.
func (t *Tree) SetStatus(id string, s Status, reason string) *Node {
	n := t.Find(id)
	if n == nil {
		return nil
	}
	if s == StatusDoing {
		if prev := t.Doing(); prev != nil && prev != n {
			prev.Status = StatusTodo
		}
	}
	if s.terminal() && n.Closed.IsZero() {
		n.Closed = time.Now()
	}
	if !s.terminal() {
		n.Closed = time.Time{}
	}
	n.Status = s
	if reason != "" {
		n.Reason = strings.TrimSpace(reason)
	}
	return n
}

// Terminal reports whether this node and everything under it is finished.
func (t *Tree) Terminal(n *Node) bool {
	if n == nil || !n.Status.terminal() {
		return false
	}
	for _, c := range n.Children {
		if !t.Terminal(c) {
			return false
		}
	}
	return true
}

// ActiveBranch is the path from a root down to the doing node, which is
// what the prompt renders in full.
func (t *Tree) ActiveBranch() []*Node {
	d := t.Doing()
	if d == nil {
		return nil
	}
	var path []*Node
	var rec func(ns []*Node, trail []*Node) bool
	rec = func(ns []*Node, trail []*Node) bool {
		for _, n := range ns {
			next := append(append([]*Node{}, trail...), n)
			if n == d {
				path = next
				return true
			}
			if rec(n.Children, next) {
				return true
			}
		}
		return false
	}
	rec(t.Roots, nil)
	return path
}

// FileRef is the durable record of one file touched while a node was doing:
// the ranges of it that were read, its hash and outline as of the last time
// they were refreshed, and whether the node itself edited it.
type FileRef struct {
	Path    string   `json:"path"`
	Hash    string   `json:"hash,omitempty"`
	Ranges  []Range  `json:"ranges,omitempty"`
	Edited  bool     `json:"edited,omitempty"`
	Note    string   `json:"note,omitempty"`
	Outline []string `json:"outline,omitempty"`
	// Turn is when this file was last seen. It is what the redundant-read
	// footer names, so it has to outlive the node being distilled.
	Turn int `json:"turn,omitempty"`
}

// CmdRef is the durable record of one shell/process invocation.
type CmdRef struct {
	Cmd     string `json:"cmd"`
	OK      bool   `json:"ok"`
	Excerpt string `json:"excerpt,omitempty"` // capped, first + last lines
}

// LookupRef is the durable record of one search/lookup/history call.
type LookupRef struct {
	Tool  string `json:"tool"`
	Query string `json:"query"`
	Hits  []Hit  `json:"hits,omitempty"`
}

// NoteRef is a fact or decision recorded against a node, optionally tied to
// a file.
type NoteRef struct {
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
	Decision bool   `json:"decision,omitempty"`
}

// RawItem is one verbatim tool call and its output, kept only while its
// node is doing. It lives in the dotdir state file, never in the workspace
// document, because it is large and transient.
type RawItem struct {
	Tool string `json:"tool"`
	// Path is the root-relative file this call named, resolved from the
	// original event rather than from the excerpted Args, so a long path
	// still matches itself when the buffer is replayed.
	Path string `json:"path,omitempty"`
	Args string `json:"args,omitempty"`
	Out  string `json:"out,omitempty"`
	OK   bool   `json:"ok"`
	Turn int    `json:"turn"`
}

// Evidence is what a node accumulates while it is doing (Raw, verbatim and
// lossless) and what it is distilled into once the node closes (Files,
// Cmds, Lookups, Notes, Errors). Raw is only ever non-empty while the node
// is doing; distill empties it into the rest.
type Evidence struct {
	Files   []FileRef   `json:"files,omitempty"`
	Cmds    []CmdRef    `json:"cmds,omitempty"`
	Lookups []LookupRef `json:"lookups,omitempty"`
	Notes   []NoteRef   `json:"notes,omitempty"`
	Errors  []string    `json:"errors,omitempty"`
	Raw     []RawItem   `json:"raw,omitempty"`     // only while doing
	Dropped int         `json:"dropped,omitempty"` // raw items dropped to a cap
}
