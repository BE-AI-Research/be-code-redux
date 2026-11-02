package api

import (
	"encoding/json"
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
