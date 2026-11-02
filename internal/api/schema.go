package api

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

// Validate checks doc against the JSON-schema subset this build understands:
// type, properties, required, items, enum and additionalProperties:false.
// Other keywords are ignored, so a schema using them is checked loosely,
// never refused. The point is to tell a caller "the model did not produce
// what you asked for" rather than to be a full validator.
func Validate(schema json.RawMessage, doc []byte) error {
	var sc map[string]any
	if err := json.Unmarshal(schema, &sc); err != nil {
		return fmt.Errorf("json_schema is not a JSON object: %w", err)
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return fmt.Errorf("the reply is not JSON: %w", err)
	}
	return validate(sc, v, "$")
}

func validate(sc map[string]any, v any, at string) error {
	if t, ok := sc["type"].(string); ok {
		if err := checkType(t, v, at); err != nil {
			return err
		}
	}
	if enum, ok := sc["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of the enum values", at, v)
		}
	}
	if obj, ok := v.(map[string]any); ok {
		if req, ok := sc["required"].([]any); ok {
			for _, r := range req {
				if name, _ := r.(string); name != "" {
					if _, has := obj[name]; !has {
						return fmt.Errorf("%s: missing required property %q", at, name)
					}
				}
			}
		}
		props, _ := sc["properties"].(map[string]any)
		for name, val := range obj {
			ps, has := props[name].(map[string]any)
			if !has {
				if ap, ok := sc["additionalProperties"].(bool); ok && !ap {
					return fmt.Errorf("%s: property %q is not allowed", at, name)
				}
				continue
			}
			if err := validate(ps, val, at+"."+name); err != nil {
				return err
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := sc["items"].(map[string]any); ok {
			for i, e := range arr {
				if err := validate(items, e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(t string, v any, at string) error {
	ok := false
	switch t {
	case "string":
		_, ok = v.(string)
	case "boolean":
		_, ok = v.(bool)
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	case "null":
		ok = v == nil
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, is := v.(float64)
		ok = is && f == math.Trunc(f)
	default:
		return fmt.Errorf("%s: schema type %q is not understood", at, t)
	}
	if !ok {
		return fmt.Errorf("%s: want %s, got %T", at, t, v)
	}
	return nil
}
