package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// parkJobs makes every co-worker's provider park until its run is cancelled.
func parkJobs(t *testing.T) *parkingProvider {
	t.Helper()
	bp := &parkingProvider{entered: make(chan struct{}, 8)}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return bp, 0, nil
	}
	return bp
}

func nodeOf(t *testing.T, st *engine.Store, id string) *engine.Node {
	t.Helper()
	tr := st.Tree()
	n := tr.Find(id)
	if n == nil {
		t.Fatalf("no node %s", id)
	}
	return n
}

func TestDispatchJobRunsAndHandsBack(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	root := f.st.Plan("port the scanner", nil)
	ctx := context.Background()
	id, err := f.ag.DispatchJob(ctx, "big", "port the scanner\nkeep the API", "internal/scan", "", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, root+".") || strings.Count(id, ".") != strings.Count(root, ".")+1 {
		t.Fatalf("job id %q is not a child of the active root %q", id, root)
	}
	n := nodeOf(t, f.st, id)
	if !n.Job || n.Owner != "big" || len(n.Scope) != 1 || n.Scope[0] != "internal/scan" || n.Text != "port the scanner" {
		t.Fatalf("job step: %+v", n)
	}
	noted := false
	for _, nt := range n.Evidence.Notes {
		if strings.Contains(nt.Text, "keep the API") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the rest of the job was not noted: %+v", n.Evidence.Notes)
	}
	d := wait(t, f.start, "start")
	if d.Node != id || !strings.Contains(d.Text, "keep the API") {
		t.Fatalf("dispatch: %+v", d)
	}
	wait(t, f.ends, "hand-back")
	f.ag.WaitSubAgents(ctx)
	q := strings.Join(f.ag.Peek(), "\n")
	if !strings.Contains(q, "big") || !strings.Contains(q, id) {
		t.Fatalf("hand-back not queued: %q", q)
	}
	if n := nodeOf(t, f.st, id); n.Status != engine.StatusDone {
		t.Fatalf("step status %s, want done", n.Status)
	}
}

func TestDispatchJobRefusals(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) {
		c.Coworkers = append(c.Coworkers,
			config.CoworkerConfig{Name: "docsy", Provider: "ollama", Model: "m", MaxScope: []string{"docs"}},
			config.CoworkerConfig{Name: "odd", Provider: "ollama", Model: "m", MaxScope: []string{"../x"}})
	})
	ctx := context.Background()
	step := f.assign(t) // big owns internal/scan, not dispatched
	cases := []struct {
		who, job string
		scope    []string
		want     string
	}{
		{"nobody", "x", []string{"a"}, "nobody is not a co-worker"},
		{"big", "  \n ", []string{"a"}, "a job needs a description"},
		{"big", "x", nil, "a job needs a scope"},
		{"big", "x", []string{"../x"}, "scope ../x escapes the workspace"},
		{"docsy", "x", []string{"internal"}, "scope is outside docsy's max_scope (docs)"},
		{"odd", "x", []string{"internal"}, "odd cannot be dispatched: its max_scope is invalid"},
		{"big", "x", []string{"internal/scan/a.go"}, "scope overlaps " + step},
	}
	for _, c := range cases {
		_, err := f.ag.DispatchJob(ctx, c.who, c.job, strings.Join(c.scope, ", "), "", c.scope)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s %q %q: got %v, want %q", c.who, c.job, c.scope, err, c.want)
		}
	}

	// No task record.
	g := newSubFixture(t, &scriptedProvider{}, nil)
	g.ag.SetEngine(nil)
	if _, err := g.ag.DispatchJob(ctx, "big", "x", "a", "", []string{"a"}); err == nil || err.Error() != "dispatch needs the task record" {
		t.Errorf("no engine: %v", err)
	}

	// max_concurrent.
	h := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) { c.SubAgents.MaxConcurrent = 1 })
	bp := parkJobs(t)
	h.st.Plan("work", nil)
	if _, err := h.ag.DispatchJob(ctx, "big", "first", "a", "", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	wait(t, bp.entered, "first job's request")
	if _, err := h.ag.DispatchJob(ctx, "big", "second", "b", "", []string{"b"}); err == nil || err.Error() != "1 of 1 jobs already running; jobs wait first" {
		t.Errorf("max_concurrent: %v", err)
	}
}

func TestDispatchJobRefusesOverlapAndNormalisesScope(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	f.st.Plan("work", nil)
	ctx := context.Background()
	first, err := f.ag.DispatchJob(ctx, "big", "first", "./internal/x/", "", []string{"./internal/x/"})
	if err != nil {
		t.Fatal(err)
	}
	if n := nodeOf(t, f.st, first); len(n.Scope) != 1 || n.Scope[0] != "internal/x" {
		t.Fatalf("scope not normalised: %q", n.Scope)
	}
	wait(t, bp.entered, "first job's request")
	for _, sc := range [][]string{{"internal/x/y.go"}, {"."}} {
		if _, err := f.ag.DispatchJob(ctx, "big", "second", sc[0], "", sc); err == nil || err.Error() != "scope overlaps "+first {
			t.Fatalf("%q: got %v", sc, err)
		}
	}
	if err := f.ag.StopSubAgent("big"); err != nil {
		t.Fatal(err)
	}
	wait(t, f.ends, "first job's end")
	f.ag.WaitSubAgents(ctx)
	whole, err := f.ag.DispatchJob(ctx, "big", "everything", ".", "", []string{"."})
	if err != nil {
		t.Fatalf("whole workspace refused once nothing runs: %v", err)
	}
	if n := nodeOf(t, f.st, whole); len(n.Scope) != 1 || n.Scope[0] != "." {
		t.Fatalf("whole-workspace scope: %q", n.Scope)
	}
	wait(t, bp.entered, "whole-workspace job's request")
	f.ag.StopAllSubAgents("test over")
}

func TestDispatchJobWorksForANonSubAgentCoworker(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) {
		c.Coworkers = append(c.Coworkers, config.CoworkerConfig{Name: "small", Provider: "ollama", Model: "m"})
	})
	ctx := context.Background()
	root := f.st.Plan("work", []string{"write the docs"})
	step := root + ".1"
	// A step assigned to small by hand (as a document edit would): the store
	// itself refuses that owner, so it is set with a card saying otherwise.
	f.st.SetCards(map[string]subagent.Card{"small": {Name: "small", SubAgent: true}})
	if err := f.st.SetOwner(step, "small", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetScope(step, []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")

	id, err := f.ag.DispatchJob(ctx, "small", "port the scanner", "internal/scan", "", []string{"internal/scan"})
	if err != nil {
		t.Fatalf("non-sub-agent co-worker refused: %v", err)
	}
	if hb := wait(t, f.ends, "hand-back"); hb.Node != id || hb.Status != "done" {
		t.Fatalf("hand-back: %+v", hb)
	}
	found := false
	for _, row := range f.ag.SubAgentStates() {
		if row.Node == step && row.State == "waiting: small is not a sub-agent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the scheduler step is not waiting as before: %+v", f.ag.SubAgentStates())
	}
}

func TestDispatchJobRefusedDuringFiredTurn(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	f.st.Plan("work", nil)
	f.ag.Tools.SetAllowance(nil, time.Minute)
	defer f.ag.Tools.ClearAllowance()
	if _, err := f.ag.DispatchJob(context.Background(), "big", "x", "a", "", []string{"a"}); !errors.Is(err, errFiredSubAgents) {
		t.Fatalf("got %v", err)
	}
	if n := len(f.st.Steps()[0].Children); n != 0 {
		t.Fatalf("a step was filed during a fired turn (%d children)", n)
	}
}

func TestJobStepClosesAtSessionEnd(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	f.st.Plan("work", nil)
	id, err := f.ag.DispatchJob(context.Background(), "big", "port it", "internal/scan", "", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, f.start, "start")
	wait(t, bp.entered, "the job's request")
	f.ag.StopAllSubAgents("session ended")
	wait(t, f.ends, "end")
	n := nodeOf(t, f.st, id)
	if n.Status != engine.StatusBlocked || n.Reason != "session ended" {
		t.Fatalf("job step after session end: %s %q", n.Status, n.Reason)
	}
	st2, err := engine.OpenAt(filepath.Join(t.TempDir(), "eng2"), f.ag.Tools.Root, "s2", false,
		engine.Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	cards := map[string]subagent.Card{"big": {Name: "big", SubAgent: true}}
	ready, waiting := subagent.Ready(st2.Steps(), cards, nil)
	if len(ready) != 0 || len(waiting) != 0 {
		t.Fatalf("a reopened store offers the job: ready %+v waiting %+v", ready, waiting)
	}
	if n := st2.Tree().Find(id); n == nil || n.Status != engine.StatusBlocked {
		t.Fatalf("reopened job step: %+v", n)
	}
}

// Ruling R9 (and the bug behind it): AssignOwner on a job parked on an ask
// used to interrupt it, which closed it "session ended" with nothing queued.
func TestAssignOwnerRefusesAnAskingJob(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
		{Content: "ported"},
	}}
	f := newSubFixture(t, sub, nil)
	f.st.Plan("work", nil)
	id, err := f.ag.DispatchJob(context.Background(), "big", "port it", "internal/scan", "", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, f.asks, "ask")
	if err := f.ag.AssignOwner(id, "big", false); err == nil || err.Error() != id+" is a job; use jobs stop" {
		t.Fatalf("AssignOwner on a job: %v", err)
	}
	rows := f.ag.RunningSubAgents()
	if len(rows) != 1 || rows[0].Node != id {
		t.Fatalf("the job is no longer running: %+v", rows)
	}
	if n := nodeOf(t, f.st, id); n.Status != engine.StatusTodo && n.Status != engine.StatusDoing {
		t.Fatalf("the job's step was closed: %s %q", n.Status, n.Reason)
	}
	if err := f.ag.ReplyAsk(id, "the old one"); err != nil {
		t.Fatal(err)
	}
	if hb := wait(t, f.ends, "hand-back"); hb.Status != "done" {
		t.Fatalf("hand-back: %+v", hb)
	}
}

func TestDispatchJobConcurrentOverlapOneWins(t *testing.T) {
	for round := 0; round < 20; round++ {
		f := newSubFixture(t, &scriptedProvider{}, nil)
		bp := parkJobs(t)
		f.st.Plan("work", nil)
		errs := make(chan error, 2)
		start := make(chan struct{})
		for _, sc := range []string{"internal/x", "internal/x/y.go"} {
			go func(sc string) {
				<-start
				_, err := f.ag.DispatchJob(context.Background(), "big", "job "+sc, sc, "", []string{sc})
				errs <- err
			}(sc)
		}
		close(start)
		var ok, overlap int
		for i := 0; i < 2; i++ {
			err := <-errs
			switch {
			case err == nil:
				ok++
			case strings.HasPrefix(err.Error(), "scope overlaps "):
				overlap++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 || overlap != 1 {
			t.Fatalf("round %d: %d dispatched, %d refused for overlap", round, ok, overlap)
		}
		wait(t, bp.entered, "the job's request")
		f.ag.StopAllSubAgents("test over")
	}
}

// Ruling R8: a job step a crashed session left open is closed by the next
// session's resume pass, so it never refuses a later dispatch by overlap.
func TestResumeClosesAnOrphanJob(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	root := f.st.Plan("work", nil)
	// Filed as a job with no run for it: what a crash leaves behind.
	orphan, err := f.st.AddJob(root, "port it", "", "big", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Flush(); err != nil {
		t.Fatal(err)
	}
	// A fresh session on the same workspace.
	st2, err := engine.OpenAt(filepath.Join(t.TempDir(), "eng2"), f.ag.Tools.Root, "s2", false,
		engine.Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	f.ag.SetEngine(st2)
	f.st = st2
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")
	if _, err := f.ag.DispatchJob(context.Background(), "big", "again", "internal/scan", "", []string{"internal/scan"}); err == nil {
		t.Fatal("the orphan did not hold its scope before the resume pass (test premise)")
	}
	f.ag.StartSubAgents()
	if n := nodeOf(t, st2, orphan); n.Status != engine.StatusBlocked || n.Reason != "interrupted" {
		t.Fatalf("orphan after resume: %s %q", n.Status, n.Reason)
	}
	id, err := f.ag.DispatchJob(context.Background(), "big", "again", "internal/scan", "", []string{"internal/scan"})
	if err != nil {
		t.Fatalf("dispatch over the closed orphan refused: %v", err)
	}
	if hb := wait(t, f.ends, "hand-back"); hb.Node != id {
		t.Fatalf("hand-back: %+v", hb)
	}
}

// dispatchJob files and dispatches one job for big, failing the test on a
// refusal.
func dispatchJob(t *testing.T, f *subFixture, text, scope string) string {
	t.Helper()
	id, err := f.ag.DispatchJob(context.Background(), "big", text, scope, "", []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func queued(f *subFixture) string { return strings.Join(f.ag.Peek(), "\n---\n") }

func TestJobWaitReturnsTheHandBackAndConsumesIt(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ported"}}}, nil)
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	line, err := f.ag.JobWait(context.Background(), id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "job "+id+" ") || !strings.Contains(line, "done") || !strings.Contains(line, "ported") {
		t.Fatalf("JobWait returned %q", line)
	}
	f.ag.WaitSubAgents(context.Background())
	if q := queued(f); strings.Contains(q, id) {
		t.Fatalf("the hand-back JobWait returned is still queued: %q", q)
	}
	// A late wait on a finished job answers at once with its hand-back.
	start := time.Now()
	again, err := f.ag.JobWait(context.Background(), id, time.Minute)
	if err != nil || again != line || time.Since(start) > 2*time.Second {
		t.Fatalf("late JobWait: %q %v after %s", again, err, time.Since(start))
	}
	if _, err := f.ag.JobWait(context.Background(), "9.9", time.Second); err == nil || err.Error() != "no job 9.9" {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestJobWaitReturnsOnAnAsk(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
		{Content: "used the old one"},
	}}
	f := newSubFixture(t, sub, nil)
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	ctx := context.Background()
	line, err := f.ag.JobWait(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if line != "job "+id+" asks: which tokenizer?" {
		t.Fatalf("JobWait on an ask: %q", line)
	}
	if q := queued(f); strings.Contains(q, "which tokenizer?") {
		t.Fatalf("the ask JobWait returned is still queued: %q", q)
	}
	if err := f.ag.JobReply(id, "yes"); err != nil {
		t.Fatal(err)
	}
	line, err = f.ag.JobWait(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "job "+id+" ") || !strings.Contains(line, "done") {
		t.Fatalf("second JobWait: %q", line)
	}
	var sawReply bool
	for _, m := range sub.lastReq.Messages {
		sawReply = sawReply || strings.Contains(m.Content, "yes")
	}
	if !sawReply {
		t.Fatal("the reply never reached the job")
	}
}

func TestJobWaitOnOneJobKeepsAnothersHandBack(t *testing.T) {
	// Two co-workers on two servers: one server's lane carries one request
	// at a time, and the slow job holds big's.
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) {
		c.Providers["other"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://other:11434"}
		c.Coworkers = append(c.Coworkers, config.CoworkerConfig{Name: "quick", Provider: "other", Model: "m", SubAgent: true})
	})
	fast := &scriptedProvider{responses: []provider.ChatResponse{{Content: "fast work"}}}
	slow := &parkingProvider{entered: make(chan struct{}, 4)}
	CoworkerFactory = func(_ context.Context, _ *config.Config, cw config.CoworkerConfig) (provider.Provider, int, error) {
		if cw.Name == "quick" {
			return fast, 0, nil
		}
		return slow, 0, nil
	}
	f.st.Plan("work", nil)
	slowID := dispatchJob(t, f, "slow job", "internal/scan")
	wait(t, f.start, "slow start")
	wait(t, slow.entered, "slow request")
	fastID, err := f.ag.DispatchJob(context.Background(), "quick", "fast job", "docs", "", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	if hb := wait(t, f.ends, "fast hand-back"); hb.Node != fastID {
		t.Fatalf("first hand-back: %+v", hb)
	}
	got := make(chan string, 1)
	go func() {
		line, _ := f.ag.JobWait(context.Background(), slowID, time.Minute)
		got <- line
	}()
	time.Sleep(50 * time.Millisecond)
	if err := f.ag.JobStop(slowID, "taking too long"); err != nil {
		t.Fatal(err)
	}
	line := wait(t, got, "JobWait on the slow job")
	if !strings.HasPrefix(line, "job "+slowID+" ") || !strings.Contains(line, "taking too long") {
		t.Fatalf("JobWait on the slow job: %q", line)
	}
	if q := queued(f); !strings.Contains(q, "job "+fastID+" ") || strings.Contains(q, "job "+slowID+" ") {
		t.Fatalf("queue after waiting on the slow job: %q", q)
	}
}

func TestJobWaitWithNothingRunningReturnsAtOnce(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	start := time.Now()
	line, err := f.ag.JobWait(context.Background(), "", time.Minute)
	if line != "" || err != nil || time.Since(start) > time.Second {
		t.Fatalf("JobWait with nothing running: %q %v after %s", line, err, time.Since(start))
	}
	// With a job running, an id-less wait returns its first event.
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	line, err = f.ag.JobWait(context.Background(), "", time.Minute)
	if err != nil || !strings.HasPrefix(line, "job "+id+" ") {
		t.Fatalf("id-less JobWait: %q %v", line, err)
	}
	// A wait that times out returns nothing and consumes nothing.
	bp := parkJobs(t)
	other := dispatchJob(t, f, "park", "docs")
	wait(t, bp.entered, "parked request")
	line, err = f.ag.JobWait(context.Background(), other, 30*time.Millisecond)
	if line != "" || err != nil {
		t.Fatalf("timed-out JobWait: %q %v", line, err)
	}
}

func TestJobStopClosesBlockedWithReason(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	wait(t, bp.entered, "the job's request")
	if err := f.ag.JobStop(id, "wrong approach"); err != nil {
		t.Fatal(err)
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "wrong approach" {
		t.Fatalf("hand-back: %+v", hb)
	}
	f.ag.WaitSubAgents(context.Background())
	if n := nodeOf(t, f.st, id); n.Status != engine.StatusBlocked || n.Reason != "wrong approach" {
		t.Fatalf("step: %s %q", n.Status, n.Reason)
	}
	if q := queued(f); !strings.Contains(q, "job "+id+" ") || !strings.Contains(q, "wrong approach") {
		t.Fatalf("the stop's hand-back was not queued: %q", q)
	}
	if err := f.ag.JobStop(id, ""); err == nil {
		t.Fatal("stopping a finished job must fail")
	}
	if err := f.ag.JobStop("9.9", ""); err == nil || err.Error() != "no job 9.9" {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestJobReplyWithoutAnAskLandsInTheJobsInbox(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	wait(t, bp.entered, "the job's request")
	if err := f.ag.JobReply(id, "use the new tokenizer"); err != nil {
		t.Fatal(err)
	}
	f.ag.subs.mu.Lock()
	scratch := f.ag.subs.runs[id].scratch
	f.ag.subs.mu.Unlock()
	if scratch.Pending() != 1 || !strings.Contains(scratch.Peek()[0], "use the new tokenizer") {
		t.Fatalf("the job's inbox: %q", scratch.Peek())
	}
	if f.ag.Pending() != 0 {
		t.Fatalf("the reply reached the main inbox: %q", f.ag.Peek())
	}
	if err := f.ag.JobReply("9.9", "x"); err == nil || err.Error() != "no job 9.9" {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestJobExtendClearsStalled(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	wait(t, bp.entered, "the job's request")
	f.ag.subs.mu.Lock()
	run := f.ag.subs.runs[id]
	run.stalled = true
	f.ag.subs.mu.Unlock()
	before := time.Now()
	if err := f.ag.JobExtend(id); err != nil {
		t.Fatal(err)
	}
	f.ag.subs.mu.Lock()
	stalled, act, iv := run.stalled, run.lastActivity, run.lastIntervention
	f.ag.subs.mu.Unlock()
	if stalled || act.Before(before) || iv.Before(before) {
		t.Fatalf("after extend: stalled %v activity %s intervention %s", stalled, act, iv)
	}
	if err := f.ag.JobExtend("9.9"); err == nil || err.Error() != "no job 9.9" {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestJobStatesShapes(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) { c.SubAgents.MaxConcurrent = 3 })
	f.st.Plan("work", nil)
	ctx := context.Background()

	done := &scriptedProvider{responses: []provider.ChatResponse{{Content: "all ported"}}}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return done, 0, nil
	}
	doneID := dispatchJob(t, f, "finish me", "docs")
	wait(t, f.ends, "done hand-back")
	f.ag.WaitSubAgents(ctx)

	asking := &scriptedProvider{responses: []provider.ChatResponse{toolCall("ask_main", `{"question":"which one?"}`)}}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return asking, 0, nil
	}
	askID := dispatchJob(t, f, "ask me", "internal/scan")
	wait(t, f.asks, "ask")

	bp := parkJobs(t)
	runID := dispatchJob(t, f, "run me", "cmd")
	wait(t, bp.entered, "running request")

	rows := f.ag.JobStates()
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	by := map[string]tools.JobState{}
	for _, r := range rows {
		by[r.ID] = r
	}
	if r := by[runID]; r.Owner != "big" || r.State != "running" || r.Elapsed <= 0 || r.At == "" {
		t.Fatalf("running row: %+v", r)
	}
	if r := by[askID]; r.State != "asking" || r.Owner != "big" || r.At == "" {
		t.Fatalf("asking row: %+v", r)
	}
	if r := by[doneID]; r.State != "done" || r.Summary != "all ported" || r.Elapsed <= 0 {
		t.Fatalf("done row: %+v", r)
	}
	// Running rows first, then finished, each by id.
	if rows[2].ID != doneID {
		t.Fatalf("order: %+v", rows)
	}
	f.ag.StopAllSubAgents("test over")
}
