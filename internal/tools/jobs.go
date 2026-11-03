package tools

import "time"

// JobState is one job as the jobs tool reports it (dispatch-jobs spec §4):
// a running job's live state, or a finished one's outcome. Declared here,
// not in internal/agent, because the agent imports tools and never the
// reverse; Agent.JobStates returns it.
type JobState struct {
	// ID is the job's step id; Owner the co-worker running it. State is
	// running | waiting (<reason>) | asking | stalled | done | blocked |
	// stopped.
	ID, Owner, State string
	Elapsed          time.Duration
	Calls            int
	// At is the doing node inside the job's subtree (the job's own id when
	// nothing narrower is known); LastTool and LastActivity are its last
	// sign of life.
	At, LastTool string
	LastActivity time.Time
	// Summary is a finished job's hand-back summary, or its reason when it
	// ended without one.
	Summary string
}
