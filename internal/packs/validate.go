package packs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file enforces the closed v1.0 manifest contract
// (be-skill-pack-format-1.0, schema/pack.schema.json). The rules below are a
// Go transcription of that schema, not of the Go struct in packs.go: the
// struct is forgiving so an older pack still parses, while the contract is
// closed so a non-conforming pack is rejected with an actionable error.
//
// The validator reads the raw bytes, not the typed manifest, so it can see keys
// the struct simply has no field for (the old "extend", "hook_pre_exec", ...):
// those are exactly the drift the closed contract exists to catch.

var (
	validateNameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	validateChunkRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	validateVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	validateTimeRe    = regexp.MustCompile(`^[0-9]+(ms|s|m|h)$`)
	validateFormatRe  = regexp.MustCompile(`^1\.[0-9]+$`)
)

// closed objects: the exact key set each object accepts. Anything else is an
// error, mirroring additionalProperties:false in the schema.
var (
	topLevelKeys = keys(
		"schema", "name", "version", "description", "license", "authors",
		"homepage", "extends", "detect", "core", "chunks", "verify", "hooks",
		"playbooks", "playbook_scope", "commands", "mcp_servers", "requires",
	)
	detectKeys   = keys("any", "always", "shell")
	coreKeys     = keys("file", "budget_tokens")
	chunkKeys    = keys("file", "summary", "triggers")
	verifyKeys   = keys("name", "cmd", "cmd_windows", "parser", "timeout", "when")
	whenKeys     = keys("changed")
	hooksKeys    = keys("pre_exec")
	preExecKeys  = keys("tool", "shell", "cmd", "cmd_windows", "stdin", "parser", "timeout", "on_error", "on_gate")
	requiresKeys = keys("format", "be_code", "parsers", "hooks", "packs", "tools")
	mcpItemKeys  = keys("name", "command", "optional", "env")
)

// rename hints: the old key a person most likely meant, for the known v1.0
// renames. This is what turns "unknown key" into an actionable message.
var renameHint = map[string]string{
	"extend":        "extends",
	"mcp":           "mcp_servers",
	"hook_pre_exec": "hooks.pre_exec",
	"when_changed":  "verify[].when.changed",
	"all":           "detect.any",
}

func keys(list ...string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, k := range list {
		m[k] = true
	}
	return m
}

// ValidateManifest checks the raw pack.json against the closed v1.0 contract.
// Load calls it on the bytes before its own typed parse, so a mistyped value
// is reported by name here rather than as "not valid JSON" there. err names
// the first violation; callers wrap it with the pack's location.
func ValidateManifest(raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err // Load already guards this path; keep the error intact
	}

	// Required top-level fields (schema "required"). "schema" is confirmed ==1
	// by Load before this is called, so check only its presence and type here.
	for _, req := range []string{"schema", "name", "version", "description", "core"} {
		if _, ok := m[req]; !ok {
			return fmt.Errorf("pack.json: missing required field %q", req)
		}
	}

	if bad := unknown(m, topLevelKeys); bad != "" {
		return fmt.Errorf("pack.json: %s", bad)
	}

	if s, ok := m["schema"]; !ok || s != float64(1) {
		return fmt.Errorf("pack.json: field %q must be the integer 1", "schema")
	}
	if !validateNameRe.MatchString(str(m["name"])) {
		return fmt.Errorf("pack.json: name %q must match %s", str(m["name"]), validateNameRe)
	}
	if !validateVersionRe.MatchString(str(m["version"])) {
		return fmt.Errorf("pack.json: version %q must be a semver like 1.2.3", str(m["version"]))
	}
	if d, ok := m["description"]; ok {
		if s, is := d.(string); !is {
			return fmt.Errorf("pack.json: field %q must be a string", "description")
		} else if n := utf8.RuneCountInString(s); n > 200 {
			return fmt.Errorf("pack.json: description is %d characters; the limit is 200", n)
		}
	}
	if e, ok := m["extends"]; ok && !validateNameRe.MatchString(str(e)) {
		return fmt.Errorf("pack.json: extends %q must match %s", str(e), validateNameRe)
	}

	if err := validateDetect(m["detect"]); err != nil {
		return err
	}
	if err := validateCore(m["core"]); err != nil {
		return err
	}
	if err := validateChunks(m["chunks"]); err != nil {
		return err
	}
	if err := validateVerify(m["verify"]); err != nil {
		return err
	}
	if err := validateHooks(m["hooks"]); err != nil {
		return err
	}
	if err := validateRequires(m["requires"]); err != nil {
		return err
	}
	return validateMCP(m["mcp_servers"])
}

// validateDetect: closed object of arrays-of-strings plus a boolean.
func validateDetect(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an object", "detect")
	}
	if bad := unknown(o, detectKeys); bad != "" {
		return fmt.Errorf("pack.json: %s", bad)
	}
	for _, k := range []string{"any", "shell"} {
		if err := stringArray("detect", -1, k, o[k]); err != nil {
			return err
		}
	}
	if a, ok := o["always"]; ok {
		if _, is := a.(bool); !is {
			return fmt.Errorf("pack.json: detect.always must be a boolean")
		}
	}
	return nil
}

// validateCore: closed object, both fields required and typed.
func validateCore(v any) error {
	if v == nil {
		return fmt.Errorf("pack.json: missing required field %q", "core")
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an object", "core")
	}
	if bad := unknown(o, coreKeys); bad != "" {
		return fmt.Errorf("pack.json: %s", bad)
	}
	if f, ok := o["file"]; !ok {
		return fmt.Errorf("pack.json: core is missing required field %q", "file")
	} else if _, is := f.(string); !is {
		return fmt.Errorf("pack.json: core.file must be a string")
	}
	bt, ok := o["budget_tokens"]
	if !ok {
		return fmt.Errorf("pack.json: core is missing required field %q", "budget_tokens")
	}
	n, is := asInt(bt)
	if !is {
		return fmt.Errorf("pack.json: core.budget_tokens must be an integer")
	}
	if n < 100 || n > 2000 {
		return fmt.Errorf("pack.json: core.budget_tokens is %d; the limit is 100 to 2000", n)
	}
	return nil
}

// validateChunks: an object of closed chunk objects.
func validateChunks(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an object", "chunks")
	}
	// Name order, so which chunk is reported first does not change from one
	// run to the next.
	names := make([]string, 0, len(o))
	for name := range o {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cv := o[name]
		if !validateChunkRe.MatchString(name) {
			return fmt.Errorf("pack.json: chunk name %q must match %s", name, validateChunkRe)
		}
		co, ok := cv.(map[string]any)
		if !ok {
			return fmt.Errorf("pack.json: chunk %q must be an object", name)
		}
		if bad := unknown(co, chunkKeys); bad != "" {
			return fmt.Errorf("pack.json: chunk %q: %s", name, bad)
		}
		for _, req := range []string{"file", "summary", "triggers"} {
			if _, ok := co[req]; !ok {
				return fmt.Errorf("pack.json: chunk %q is missing required field %q", name, req)
			}
		}
		if _, is := co["file"].(string); !is {
			return fmt.Errorf("pack.json: chunk %q: file must be a string", name)
		}
		if s, ok := co["summary"].(string); !ok {
			return fmt.Errorf("pack.json: chunk %q: summary must be a string", name)
		} else if n := utf8.RuneCountInString(s); n > 200 {
			return fmt.Errorf("pack.json: chunk %q: summary is %d characters; the limit is 200", name, n)
		}
		tr, ok := co["triggers"].([]any)
		if !ok || len(tr) < 1 {
			return fmt.Errorf("pack.json: chunk %q: triggers must be a non-empty array", name)
		}
		for i, t := range tr {
			if _, is := t.(string); !is || t == "" {
				return fmt.Errorf("pack.json: chunk %q: triggers[%d] must be a non-empty string", name, i)
			}
		}
	}
	return nil
}

// validateVerify: an array of closed check objects.
func validateVerify(v any) error {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an array", "verify")
	}
	for i, ev := range arr {
		eo, ok := ev.(map[string]any)
		if !ok {
			return fmt.Errorf("pack.json: verify[%d] must be an object", i)
		}
		if bad := unknown(eo, verifyKeys); bad != "" {
			return fmt.Errorf("pack.json: verify[%d]: %s", i, bad)
		}
		for _, req := range []string{"name", "cmd", "parser"} {
			if _, ok := eo[req]; !ok {
				return fmt.Errorf("pack.json: verify[%d] is missing required field %q", i, req)
			}
		}
		nm, _ := eo["name"].(string)
		if !validateChunkRe.MatchString(nm) {
			return fmt.Errorf("pack.json: verify[%d]: name %q must match %s", i, nm, validateChunkRe)
		}
		if err := nonEmptyStringArray("verify", i, "cmd", eo["cmd"]); err != nil {
			return err
		}
		if p, ok := eo["parser"].(string); !ok || p == "" {
			return fmt.Errorf("pack.json: verify[%d]: parser must be a non-empty string", i)
		}
		if t, ok := eo["timeout"].(string); ok && !validateTimeRe.MatchString(t) {
			return fmt.Errorf("pack.json: verify[%d]: timeout %q must be like 30s, 5m or 2h", i, t)
		}
		if w, ok := eo["when"]; ok {
			wo, ok := w.(map[string]any)
			if !ok {
				return fmt.Errorf("pack.json: verify[%d]: when must be an object", i)
			}
			if bad := unknown(wo, whenKeys); bad != "" {
				return fmt.Errorf("pack.json: verify[%d]: when: %s", i, bad)
			}
			if err := stringArray("verify", i, "when.changed", wo["changed"]); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateHooks: a closed object; pre_exec is itself a closed object.
func validateHooks(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an object", "hooks")
	}
	if bad := unknown(o, hooksKeys); bad != "" {
		return fmt.Errorf("pack.json: hooks: %s", bad)
	}
	pe, ok := o["pre_exec"]
	if !ok {
		return nil
	}
	po, ok := pe.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: hooks.pre_exec must be an object")
	}
	if bad := unknown(po, preExecKeys); bad != "" {
		return fmt.Errorf("pack.json: hooks.pre_exec: %s", bad)
	}
	for _, req := range []string{"tool", "cmd", "parser"} {
		if _, ok := po[req]; !ok {
			return fmt.Errorf("pack.json: hooks.pre_exec is missing required field %q", req)
		}
	}
	if err := nonEmptyStringArray("hooks.pre_exec", -1, "cmd", po["cmd"]); err != nil {
		return err
	}
	if t, ok := po["timeout"].(string); ok && !validateTimeRe.MatchString(t) {
		return fmt.Errorf("pack.json: hooks.pre_exec: timeout %q must be like 30s, 5m or 2h", t)
	}
	for _, k := range []string{"on_error", "on_gate"} {
		if s, ok := po[k].(string); ok {
			if !validEnum(s, k) {
				return fmt.Errorf("pack.json: hooks.pre_exec.%s %q is not one of %s", k, s, enumValues(k))
			}
		}
	}
	return nil
}

// validateRequires: closed object of scalars and arrays.
func validateRequires(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("pack.json: field %q must be an object", "requires")
	}
	if bad := unknown(o, requiresKeys); bad != "" {
		return fmt.Errorf("pack.json: requires: %s", bad)
	}
	if f, ok := o["format"].(string); ok && !validateFormatRe.MatchString(f) {
		return fmt.Errorf("pack.json: requires.format %q must be like 1.0", f)
	}
	for _, k := range []string{"parsers", "hooks", "packs", "tools"} {
		if err := stringArray("requires", -1, k, o[k]); err != nil {
			return err
		}
	}
	return nil
}

// validateMCP: the schema types mcp_servers as an array of {name,command,...};
// the loader and the shipped ue5 pack use a name-keyed object. Both are accepted
// so the closed contract does not reject a working pack over this one known
// divergence; the object form is not recursed into. Reconciling the two shapes
// fully is a separate change (see the mcp_servers field).
func validateMCP(v any) error {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case map[string]any:
		return nil // name-keyed object: accepted, not recursed
	case []any:
		for i, iv := range t {
			io, ok := iv.(map[string]any)
			if !ok {
				return fmt.Errorf("pack.json: mcp_servers[%d] must be an object", i)
			}
			if bad := unknown(io, mcpItemKeys); bad != "" {
				return fmt.Errorf("pack.json: mcp_servers[%d]: %s", i, bad)
			}
			if _, ok := io["name"].(string); !ok {
				return fmt.Errorf("pack.json: mcp_servers[%d]: name must be a string", i)
			}
			if _, ok := io["command"].([]any); !ok {
				return fmt.Errorf("pack.json: mcp_servers[%d]: command must be an array", i)
			}
		}
		return nil
	default:
		return fmt.Errorf("pack.json: field %q must be an object or an array", "mcp_servers")
	}
}

func validEnum(s, k string) bool {
	for _, a := range enumList(k) {
		if s == a {
			return true
		}
	}
	return false
}

func enumList(k string) []string {
	switch k {
	case "on_error":
		return []string{"block", "warn"}
	case "on_gate":
		return []string{"approve", "block", "warn"}
	}
	return nil
}

func enumValues(k string) string {
	a := enumList(k)
	return strings.Join(a, ", ")
}

// unknown reports the first key of o not in allowed, with a rename hint when
// one is known. Returns "" when every key is allowed.
func unknown(o map[string]any, allowed map[string]bool) string {
	var bad []string
	for k := range o {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Strings(bad)
	for _, k := range bad {
		if hint, ok := renameHint[k]; ok {
			return fmt.Sprintf("unknown key %q (use %q instead)", k, hint)
		}
	}
	return fmt.Sprintf("unknown key %q", bad[0])
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	n := int(f)
	if float64(n) != f {
		return 0, false
	}
	return n, true
}

// stringArray checks v is absent or an array of strings (empty elements are
// allowed where the schema does not require them). label is "field" or a
// "field" with an index for the message.
func stringArray(label string, idx int, key string, v any) error {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return fmt.Errorf("pack.json: %s must be an array", qualify(label, idx, key))
	}
	for i, e := range arr {
		if s, is := e.(string); !is {
			return fmt.Errorf("pack.json: %s[%d] must be a string", qualify(label, idx, key), i)
		} else if s == "" {
			return fmt.Errorf("pack.json: %s[%d] must not be empty", qualify(label, idx, key), i)
		}
	}
	return nil
}

// nonEmptyStringArray is stringArray but also rejects an absent or empty array
// (the schema's minItems:1).
func nonEmptyStringArray(label string, idx int, key string, v any) error {
	if v == nil {
		return fmt.Errorf("pack.json: %s must be a non-empty array", qualify(label, idx, key))
	}
	arr, ok := v.([]any)
	if !ok {
		return fmt.Errorf("pack.json: %s must be an array", qualify(label, idx, key))
	}
	if len(arr) < 1 {
		return fmt.Errorf("pack.json: %s must have at least one element", qualify(label, idx, key))
	}
	for i, e := range arr {
		if s, is := e.(string); !is {
			return fmt.Errorf("pack.json: %s[%d] must be a string", qualify(label, idx, key), i)
		} else if s == "" {
			return fmt.Errorf("pack.json: %s[%d] must not be empty", qualify(label, idx, key), i)
		}
	}
	return nil
}

// qualify builds the label for an error message: "verify[3].when.changed"
// for an indexed field, "hooks.pre_exec.cmd" for one that is not.
func qualify(label string, idx int, key string) string {
	if idx >= 0 {
		label = fmt.Sprintf("%s[%d]", label, idx)
	}
	if key == "" {
		return label
	}
	return label + "." + key
}
