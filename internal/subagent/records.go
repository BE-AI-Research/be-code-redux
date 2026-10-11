// Package subagent holds what the sub-agent design shares between the task
// engine, the tools and the agent: the records a dispatch and a hand-back
// are made of, the path-scope arithmetic, the ready rule and the per-server
// lanes. It imports nothing of the harness, so a later transport can carry
// its records to another host without dragging the agent along.
package subagent

import "time"

// Card is what the scheduler knows about one co-worker.
type Card struct {
	Name     string
	Provider string
	Server   string // LaneKey of the provider's base_url
	Online   bool
	SubAgent bool
	MaxScope []string
}

// Dispatch is one step handed to a sub-agent (spec §1.6).
type Dispatch struct {
	Session     string
	Node        string
	Owner       string
	Text        string
	Children    []string
	Scope       []string
	Checks      []string
	Context     string
	MaxTurns    int
	Interrupted bool
	Touched     []string
	// Base is the git branch holding the working tree as it was when the
	// step was dispatched ("" outside a repository, or when the snapshot
	// failed). The sub-agent can read any file as it was then with the
	// show tool, and the main model can diff against it.
	Base string
}

// HandBack is what comes back when the sub-agent's run ends.
type HandBack struct {
	Node    string
	Owner   string
	Status  string // "done" | "blocked"
	Reason  string
	Summary string
	Files   []string
	Elapsed time.Duration
	Calls   int
	// Drifted is every file the sub-agent read that somebody else — another
	// session, or this session's own main model — changed before the run
	// ended, workspace-relative. Its summary was written against the older
	// versions of these. A file the sub-agent itself wrote is never here.
	Drifted []string
	// Base is Dispatch.Base: where the tree it began from is.
	Base string
}

// Ask is one question from a sub-agent to the main model.
type Ask struct {
	Node, Owner, Question string
}

// Step is the ready rule's view of a tree node.
type Step struct {
	ID          string
	Text        string
	Status      string // todo | doing | done | blocked | dropped
	Owner       string // nearest owner up the tree; "" is the main model
	Scope       []string
	After       []string
	Interrupted bool
	Touched     []string
	Children    []Step
}

// Candidate is an assigned step and why it is not ready yet (Reason == ""
// means it is).
type Candidate struct {
	ID     string
	Owner  string
	Reason string
}
