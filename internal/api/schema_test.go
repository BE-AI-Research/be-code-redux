package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateSubset(t *testing.T) {
	schema := json.RawMessage(`{
	  "type": "object",
	  "required": ["name", "count"],
	  "properties": {
	    "name":  {"type": "string"},
	    "count": {"type": "integer"},
	    "tags":  {"type": "array", "items": {"type": "string"}},
	    "kind":  {"type": "string", "enum": ["a", "b"]},
	    "inner": {"type": "object", "properties": {"ok": {"type": "boolean"}}}
	  }
	}`)
	for doc, ok := range map[string]bool{
		`{"name":"x","count":2}`: true,
		`{"name":"x","count":2,"tags":["a"],"kind":"a","inner":{"ok":true}}`: true,
		`{"name":"x"}`:                                false, // missing count
		`{"name":"x","count":2.5}`:                    false, // not an integer
		`{"name":1,"count":2}`:                        false,
		`{"name":"x","count":2,"tags":[1]}`:           false,
		`{"name":"x","count":2,"kind":"c"}`:           false,
		`{"name":"x","count":2,"inner":{"ok":"yes"}}`: false,
		`not json`: false,
		`[]`:       false,
	} {
		err := Validate(schema, []byte(doc))
		if (err == nil) != ok {
			t.Errorf("%s: want ok=%v, got %v", doc, ok, err)
		}
	}
	if err := Validate(json.RawMessage(`{"type": "nonsense"}`), []byte(`{}`)); err == nil {
		t.Error("an unknown type is a schema error, not a pass")
	}
}

func TestValidateRefusesANonStringType(t *testing.T) {
	if err := Validate(json.RawMessage(`{"type": ["string","null"]}`), []byte(`1`)); err == nil {
		t.Error("an array type is a schema error, not a pass")
	}
	if err := CompileSchema(json.RawMessage(`{"type": ["string"]}`)); err == nil {
		t.Error("CompileSchema must refuse an array type")
	}
	if err := CompileSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"integer"}}},"required":["a"]}`)); err != nil {
		t.Error(err)
	}
}

func TestSchemaBooleanSubschemasAndTupleItems(t *testing.T) {
	ok := json.RawMessage(`{"type":"array","items":true}`)
	if err := CompileSchema(ok); err != nil {
		t.Fatal(err)
	}
	if err := Validate(ok, []byte(`[1,"a",null]`)); err != nil {
		t.Errorf("items:true is unconstrained: %v", err)
	}
	props := json.RawMessage(`{"type":"object","properties":{"a":true},"additionalProperties":false}`)
	if err := Validate(props, []byte(`{"a":[1]}`)); err != nil {
		t.Errorf("a true property schema is unconstrained: %v", err)
	}
	err := CompileSchema(json.RawMessage(`{"type":"array","items":[{"type":"string"}]}`))
	if err == nil || !strings.Contains(err.Error(), "items") {
		t.Errorf("tuple items is a compile error naming items: %v", err)
	}
	if err := CompileSchema(json.RawMessage(`{"properties":{"a":false}}`)); err == nil || !strings.Contains(err.Error(), "properties") {
		t.Errorf("a false subschema is refused naming the keyword: %v", err)
	}
	if CompileSchema(json.RawMessage(` null `)) != nil || Validate(json.RawMessage(`null`), []byte(`x`)) != nil {
		t.Error("null is no schema")
	}
}

func TestCompileSchemaTopLevelMustBeAnObject(t *testing.T) {
	for _, raw := range []string{`true`, `false`, `[]`, `"x"`} {
		err := CompileSchema(json.RawMessage(raw))
		if err == nil || !strings.Contains(err.Error(), "json_schema must be a JSON object") {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
