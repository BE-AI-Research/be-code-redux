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
}

// Add appends a child under parent ("" for a new root) and returns it. Ids
// are positional, so a node's id never changes while it exists: adding to
// one branch cannot renumber another.
func (t *Tree) Add(parent, text string) *Node {
	n := &Node{Text: strings.TrimSpace(text), Status: StatusTodo, Opened: time.Now()}
	if parent == "" {
		n.ID = strconv.Itoa(len(t.Roots) + 1)
		t.Roots = append(t.Roots, n)
		return n
	}
	p := t.Find(parent)
	if p == nil {
		// An unknown parent must not lose the node: file it as a root.
		n.ID = strconv.Itoa(len(t.Roots) + 1)
		t.Roots = append(t.Roots, n)
		return n
	}
	n.ID = p.ID + "." + strconv.Itoa(len(p.Children)+1)
	p.Children = append(p.Children, n)
	return n
}

func (t *Tree) Find(id string) *Node {
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

// Evidence is what a node accumulated while it was doing. Task 2 fills it.
type Evidence struct{}
