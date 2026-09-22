package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Qwen's own tool-call layout:
//
//	<tool_call>
//	<function=write_file>
//	<parameter=path>
//	src/x.py
//	</parameter>
//	<parameter=content>
//	...the file, verbatim...
//	</parameter>
//	</function>
//	</tool_call>
//
// It is what the Qwen3-Coder and Qwen3.5+ chat templates teach, so it is what
// such a model writes when it is under pressure — a full context, a long
// argument — whatever format the prompt asked for. A server that parses tool
// calls (Ollama) sometimes passes it through as plain content instead, and a
// harness that does not recognise it takes a tool call for a final answer:
// the run ends mid-task and the file is never written, which is how the
// owner's VM run stopped on 2026-09-20. The premise of this harness is that
// the model is fallible and the parsing forgiving, so this form is accepted
// wherever the JSON form is.

var (
	xmlCallRe  = regexp.MustCompile(`(?s)(?:<tool_call>\s*)?<function=([^>\s]+)\s*>(.*?)</function>(?:\s*</tool_call>)?`)
	xmlParamRe = regexp.MustCompile(`(?s)<parameter=([^>\s]+)\s*>(.*?)</parameter>`)
)

// ParamTypeFunc reports the JSON-schema type of one tool parameter ("string",
// "integer", "array", …), or "" when unknown.
type ParamTypeFunc func(tool, param string) string

// parseXMLCalls extracts Qwen-layout calls to known tools from content and
// returns the text with them removed. Parameter values are text by nature:
// they stay strings unless the tool's schema says the parameter is something
// else, because a file's content that happens to be valid JSON must reach
// write_file exactly as written.
func parseXMLCalls(content string, known map[string]bool, typeOf ParamTypeFunc, firstID int) (string, []provider.ToolCall) {
	var calls []provider.ToolCall
	clean := content
	for _, m := range xmlCallRe.FindAllStringSubmatch(content, -1) {
		name := strings.TrimSpace(m[1])
		if !known[name] {
			continue
		}
		args := map[string]any{}
		for _, p := range xmlParamRe.FindAllStringSubmatch(m[2], -1) {
			key := strings.TrimSpace(p[1])
			args[key] = xmlValue(unframe(p[2]), paramType(typeOf, name, key))
		}
		raw, err := json.Marshal(args)
		if err != nil {
			continue
		}
		calls = append(calls, provider.ToolCall{
			ID:        fmt.Sprintf("embedded_%d", firstID+len(calls)),
			Name:      name,
			Arguments: string(raw),
		})
		clean = strings.Replace(clean, m[0], "", 1)
	}
	return strings.TrimSpace(clean), calls
}

// unframe removes the one newline the layout puts after the opening tag and
// the one before the closing tag — no more, so a file keeps its own leading
// and trailing blank lines.
func unframe(v string) string {
	v = strings.TrimPrefix(v, "\r\n")
	v = strings.TrimPrefix(v, "\n")
	v = strings.TrimSuffix(v, "\n")
	v = strings.TrimSuffix(v, "\r")
	return v
}

func paramType(typeOf ParamTypeFunc, tool, param string) string {
	if typeOf == nil {
		return ""
	}
	return typeOf(tool, param)
}

// xmlValue turns a parameter's text into the value the schema asks for. Only
// a declared non-string type is decoded, and only when the text really is
// that JSON; anything else goes through as text, which every built-in tool's
// argument helpers already accept for numbers and booleans.
func xmlValue(text, typ string) any {
	switch typ {
	case "integer", "number", "boolean", "array", "object":
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(text)), &v) == nil {
			return v
		}
	}
	return text
}

// SchemaParamTypes builds a ParamTypeFunc from tool specs.
func SchemaParamTypes(specs []provider.ToolSpec) ParamTypeFunc {
	types := map[string]string{}
	for _, s := range specs {
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if json.Unmarshal(s.Parameters, &schema) != nil {
			continue
		}
		for param, p := range schema.Properties {
			if t, ok := p.Type.(string); ok {
				types[s.Name+"\x00"+param] = t
			}
		}
	}
	return func(tool, param string) string { return types[tool+"\x00"+param] }
}
