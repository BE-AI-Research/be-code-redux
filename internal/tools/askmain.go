package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// AskMainFunc parks the sub-agent until the main model or the operator
// answers, or the ask times out.
type AskMainFunc func(ctx context.Context, question string) (string, error)

type askMainTool struct{ ask AskMainFunc }

// NewAskMain is the one tool a sub-agent has that reaches the main model
// (spec §2.7). The agent supplies the round trip.
func NewAskMain(ask AskMainFunc) Tool { return &askMainTool{ask: ask} }

func (t *askMainTool) Name() string { return "ask_main" }
func (t *askMainTool) Description() string {
	return "Ask the main model one precise question and wait for its answer: a file outside your scope, a decision you cannot make, information only it has. Use it once, then continue."
}
func (t *askMainTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"question":{"type":"string","description":"One precise question"}},
		"required":["question"]}`)
}
func (t *askMainTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "question", "text", "q"))
	if q == "" {
		return Result{IsError: true, Content: "question is required"}
	}
	answer, err := t.ask(ctx, q)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: "the main model replied:\n\n" + answer}
}
