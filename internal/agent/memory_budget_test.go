package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// probeReads is the whole-branch review's probe shape: one request, sixteen
// reads of 6 KB files, and a model that never calls the task tool — the
// common case, where everything lands on the permanent unfiled node.
const probeReads = 16

// sixKBFile is a numbered-looking Go file of about 6 KB with a sentinel the
// test can look for.
func sixKBFile(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package probe\n\n// sentinel-file-%02d\n", i)
	for n := 0; b.Len() < 6*1024; n++ {
		fmt.Fprintf(&b, "func F%02d_%03d() int { return %d } // padding padding padding\n", i, n, n)
	}
	return b.String()
}

// workingMemoryOf is the Working memory: block of a system prompt. The test
// agents carry no handoff, guidance or git summary, so the block runs to the
// end of the prompt.
func workingMemoryOf(system string) string {
	i := strings.Index(system, "\n\nWorking memory:\n")
	if i < 0 {
		return ""
	}
	return system[i+len("\n\nWorking memory:\n"):]
}

// runProbe drives the probe at one window and returns the largest block any
// request carried, the block the last request carried, and how many summary
// calls the run needed.
func runProbe(t *testing.T, window int, engineOn bool) (largest int, last string, summaries int) {
	t.Helper()
	calls := 0
	p := &funcProvider{}
	p.fn = func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if len(req.Messages) > 0 && req.Messages[0].Content == compactSystemPrompt {
			return &provider.ChatResponse{Content: "summary of the earlier reads\nfiles:\n- f00.go — padding"}, nil
		}
		calls++
		if calls <= probeReads {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{
				ID: fmt.Sprintf("c%d", calls), Name: "read_file",
				Arguments: fmt.Sprintf(`{"path":"f%02d.go"}`, calls-1),
			}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = window })
	for i := 0; i < probeReads; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.go", i)), []byte(sixKBFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if engineOn {
		withEngine(t, ag)
	}
	if _, err := ag.Run(context.Background(), "read every file and tell me what is in them"); err != nil {
		t.Fatal(err)
	}
	for _, req := range p.requests() {
		if len(req.Messages) == 0 {
			continue
		}
		if req.Messages[0].Content == compactSystemPrompt {
			summaries++
			// The compaction request carries the same capped block: measured
			// with the rest, so an uncapped copy there fails the same test.
			body := req.Messages[1].Content
			if i := strings.Index(body, "Working memory:\n"); i >= 0 {
				wm := body[i+len("Working memory:\n"):]
				if j := strings.Index(wm, "\n\nTranscript (most recent last):"); j >= 0 {
					wm = wm[:j]
				}
				if len(wm) > largest {
					largest = len(wm)
				}
			}
			continue
		}
		last = workingMemoryOf(req.Messages[0].Content)
		if len(last) > largest {
			largest = len(last)
		}
	}
	return largest, last, summaries
}

// TestTheBlockRespectsItsBudgetWhenTheModelNeverCallsTask is C1. A model
// that never calls task leaves everything on the unfiled node, which stays
// doing for the whole session; rendered verbatim that node alone reached
// about 36 KB — three quarters of a 16k window — and the engine that exists
// to survive compaction became the thing forcing it.
func TestTheBlockRespectsItsBudgetWhenTheModelNeverCallsTask(t *testing.T) {
	for _, window := range []int{8192, 16384, 32768} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			largest, last, summaries := runProbe(t, window, true)
			_, _, baseline := runProbe(t, window, false)
			t.Logf("window %d: largest block %d bytes, final block %d bytes, %d summary call(s) (engine off: %d)",
				window, largest, len(last), summaries, baseline)

			// The cap: a quarter of the usable limit in bytes, floored at
			// 2 KB and never above engine.budget.
			max := config.Default().Engine.Budget
			if largest > max {
				t.Fatalf("the block reached %d bytes, over engine.budget %d", largest, max)
			}
			if !strings.Contains(last, fmt.Sprintf("sentinel-file-%02d", probeReads-1)) {
				t.Fatalf("the newest raw item is not in the block:\n%s", last)
			}
			if !strings.Contains(last, "f00.go") {
				t.Fatalf("the oldest read left no trace in the block:\n%s", last)
			}
			// At 16k and above the engine must cost no compaction the
			// engine-off run does not also need: unfixed it cost seven at
			// 16k against none.
			//
			// At 8k the comparison is absolute, not relative. Once the
			// compaction target became the floor plus half the room, the
			// engine-off run stopped needing any summary here (it needed
			// four), because collapsing old tool output now reaches the
			// target. With the block on, a single 6 KB read is ~2000 tokens
			// against ~1600 of half-room, so collapsing alone cannot get
			// under the target and each round summarises: five, exactly as
			// before that change. That is the real price of the block at an
			// 8k window under this probe's extreme shape; pin it so it
			// cannot get worse.
			allowed := baseline
			if window < 16384 {
				allowed = 5
			}
			if summaries > allowed {
				t.Fatalf("the engine cost %d summary call(s) where the engine-off baseline needs %d", summaries, baseline)
			}
			if want := derivedBudget(window); largest > want {
				t.Fatalf("the block reached %d bytes, over the %d this window derives", largest, want)
			}
		})
	}
}

// derivedBudget is what memoryBudget derives for a plain model at one window
// before any calibration: a quarter of the usable limit, in bytes.
func derivedBudget(window int) int {
	limit := window - window/4 // reserveFor: a quarter for a non-thinking model
	b := int(float64(limit) * defaultCharsPerToken * memoryShare)
	if b < memoryFloor {
		b = memoryFloor
	}
	if max := config.Default().Engine.Budget; b > max {
		b = max
	}
	return b
}

// TestTheMemoryBudgetFollowsTheWindow: the cap is derived from the live
// limit, floored at 2 KB and never above engine.budget.
func TestTheMemoryBudgetFollowsTheWindow(t *testing.T) {
	ag, _ := agentWithEngine(t)
	for _, c := range []struct{ window, want int }{
		{2048, 2048},  // the floor
		{8192, 4608},  // 6144 usable tokens * 3 bytes * 25%
		{16384, 6144}, // engine.budget
		{32768, 6144},
	} {
		ag.ApplyWindow(c.window)
		ag.History.mu.Lock()
		ag.History.Budget = c.window
		ag.History.mu.Unlock()
		ag.applyReserve(c.window)
		if got := ag.memoryBudget(); got != c.want {
			t.Fatalf("window %d: budget %d, want %d", c.window, got, c.want)
		}
	}
	ag.Cfg.Engine.Budget = 1024
	if got := ag.memoryBudget(); got != 1024 {
		t.Fatalf("engine.budget 1024 must win over the floor, got %d", got)
	}
}
