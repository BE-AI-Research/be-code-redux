package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestConsultToolDescribesTheRosterAndDelegates(t *testing.T) {
	var got ConsultArgs
	tool := NewConsult([]CoworkerInfo{{"claude", "deep reasoning"}, {"big", "long reads"}},
		func(ctx context.Context, a ConsultArgs) (string, error) {
			got = a
			return "co-worker claude replied:\n\nanswer", nil
		})
	if tool.Name() != "consult" {
		t.Fatal(tool.Name())
	}
	d := tool.Description()
	for _, want := range []string{"claude — deep reasoning", "big — long reads", "keep doing the work"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description lacks %q:\n%s", want, d)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	for _, k := range []string{"question", "who", "files"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("schema lacks %s", k)
		}
	}
	res := tool.Run(context.Background(), map[string]any{"question": "why?", "who": "big", "files": []any{"a.go", "b.go"}})
	if res.IsError || res.Content != "co-worker claude replied:\n\nanswer" {
		t.Fatalf("result = %+v", res)
	}
	if got.Question != "why?" || got.Who != "big" || len(got.Files) != 2 || got.Files[1] != "b.go" {
		t.Fatalf("args = %+v", got)
	}
	if res := tool.Run(context.Background(), map[string]any{}); !res.IsError || res.Content != "consult needs a question" {
		t.Fatalf("empty question: %+v", res)
	}
	tool = NewConsult(nil, func(context.Context, ConsultArgs) (string, error) { return "", errors.New("consultation declined") })
	if res := tool.Run(context.Background(), map[string]any{"question": "q"}); !res.IsError || res.Content != "consultation declined" {
		t.Fatalf("error passthrough: %+v", res)
	}
}
