package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The consult tool lets the primary model ask a configured co-working
// model for help. The tool itself knows nothing about providers: cmd hands
// it the roster to describe and a function that runs the consultation
// (agent.Consult behind it).

type ConsultArgs struct {
	Question string
	Who      string
	Files    []string
}

// ConsultFunc runs one consultation and returns the text handed back to
// the model, or an error the model is told about.
type ConsultFunc func(ctx context.Context, args ConsultArgs) (string, error)

// CoworkerInfo is what the tool description shows for one co-worker.
type CoworkerInfo struct{ Name, Skills string }

type consultTool struct {
	roster []CoworkerInfo
	run    ConsultFunc
}

// NewConsult builds the tool. Register it only when roster is non-empty.
func NewConsult(roster []CoworkerInfo, run ConsultFunc) Tool {
	return &consultTool{roster: roster, run: run}
}

func (t *consultTool) Name() string { return "consult" }

func (t *consultTool) Description() string {
	var b strings.Builder
	b.WriteString("Ask a co-working model for help. Co-working models:\n")
	for _, cw := range t.roster {
		fmt.Fprintf(&b, "- %s — %s\n", cw.Name, cw.Skills)
	}
	b.WriteString("Use when you have tried and failed, when a design question is beyond you, or when you need knowledge you do not have. Say what you tried. The co-worker can read the repository but cannot edit; you keep doing the work.")
	return b.String()
}

func (t *consultTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"question":{"type":"string","description":"What is wrong and what you tried"},
		"who":{"type":"string","description":"Co-worker name (default: the first one)"},
		"files":{"type":"array","items":{"type":"string"},"description":"Workspace paths the co-worker should look at first"}},
		"required":["question"]}`)
}

func (t *consultTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "question", "q"))
	if q == "" {
		return Result{IsError: true, Content: "consult needs a question"}
	}
	a := ConsultArgs{Question: q, Who: strings.TrimSpace(argString(args, "who", "name", "coworker"))}
	switch v := args["files"].(type) {
	case []any:
		for _, f := range v {
			if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
				a.Files = append(a.Files, strings.TrimSpace(s))
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			a.Files = append(a.Files, strings.TrimSpace(v))
		}
	}
	out, err := t.run(ctx, a)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: out}
}
