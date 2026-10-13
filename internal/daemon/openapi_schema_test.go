package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadSpec parses docs/api/openapi.yaml into generic maps.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// resolve follows a local "$ref" ("#/a/b/c") until it reaches a concrete node.
func resolve(t *testing.T, spec map[string]any, node map[string]any) map[string]any {
	t.Helper()
	for {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		var cur any = spec
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("bad $ref %s", ref)
			}
			cur = m[part]
		}
		next, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("$ref %s does not resolve", ref)
		}
		node = next
	}
}

// responseSchemaFor returns the application/json schema documented for the
// response with status code of "method path".
func responseSchemaFor(t *testing.T, spec map[string]any, method, path, code string) map[string]any {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, _ := item[strings.ToLower(method)].(map[string]any)
	resps, _ := op["responses"].(map[string]any)
	r200, _ := resps[code].(map[string]any)
	if r200 == nil {
		t.Fatalf("%s %s documents no %s response", method, path, code)
	}
	r200 = resolve(t, spec, r200)
	content, _ := r200["content"].(map[string]any)
	js, _ := content["application/json"].(map[string]any)
	schema, _ := js["schema"].(map[string]any)
	if schema == nil {
		t.Fatalf("%s %s %s has no application/json schema", method, path, code)
	}
	return schema
}

// validate checks v against schema for the subset of OpenAPI the spec uses
// (type, nullable, enum, required, properties, items, $ref). It returns a list
// of violations with their JSON path.
func validate(t *testing.T, spec map[string]any, schema map[string]any, v any, at string) []string {
	t.Helper()
	schema = resolve(t, spec, schema)
	if v == nil {
		if n, _ := schema["nullable"].(bool); n {
			return nil
		}
		return []string{at + ": null is not allowed (no nullable: true)"}
	}
	var errs []string
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: %v not in enum %v", at, v, enum))
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want object, got %T", at, v))
		}
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				if _, present := obj[k.(string)]; !present {
					errs = append(errs, fmt.Sprintf("%s: missing required %q", at, k))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, pv := range obj {
			if ps, ok := props[k].(map[string]any); ok {
				errs = append(errs, validate(t, spec, ps, pv, at+"."+k)...)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want array, got %T", at, v))
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range arr {
				errs = append(errs, validate(t, spec, items, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			errs = append(errs, fmt.Sprintf("%s: want string, got %T", at, v))
		}
	case "integer", "number":
		if _, ok := v.(float64); !ok {
			errs = append(errs, fmt.Sprintf("%s: want number, got %T", at, v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("%s: want boolean, got %T", at, v))
		}
	}
	return errs
}

// assertConforms decodes raw as JSON and checks it against the 200 schema the
// OpenAPI file documents for method+docPath.
func assertConforms(t *testing.T, method, docPath, raw string) {
	t.Helper()
	assertConformsStatus(t, method, docPath, "200", raw)
}

// assertConformsStatus is assertConforms for the response documented under code.
func assertConformsStatus(t *testing.T, method, docPath, code, raw string) {
	t.Helper()
	spec := loadSpec(t)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("%s %s: not JSON: %v (%s)", method, docPath, err, raw)
	}
	for _, e := range validate(t, spec, responseSchemaFor(t, spec, method, docPath, code), v, "$") {
		t.Errorf("%s %s response %s violates openapi.yaml: %s", method, docPath, raw, e)
	}
}

// TestValidatorRejectsViolations keeps the checker honest: it must flag a
// null without nullable, an out-of-enum value and a missing required field.
func TestValidatorRejectsViolations(t *testing.T) {
	spec := map[string]any{}
	schema := map[string]any{
		"type":     "object",
		"required": []any{"a", "k"},
		"properties": map[string]any{
			"a": map[string]any{"type": "array"},
			"k": map[string]any{"type": "string", "enum": []any{"x", "y"}},
		},
	}
	got := validate(t, spec, schema, map[string]any{"a": nil, "k": ""}, "$")
	if len(got) != 2 {
		t.Fatalf("want 2 violations (null, enum), got %v", got)
	}
	got = validate(t, spec, schema, map[string]any{"k": "x"}, "$")
	if len(got) != 1 || !strings.Contains(got[0], "missing required") {
		t.Fatalf("want missing required, got %v", got)
	}
}

// The daemon's real answers for the edge cases the contract must cover:
// POST /projects echoes the body before defaults, and GET /detect on a
// directory with nothing recognisable returns null/empty fields.
func TestResponsesConformToOpenAPI(t *testing.T) {
	_, srv := newTestDaemon(t)

	_, _, raw := do(t, srv, "POST", "/api/v1/projects", `{"name":"x"}`)
	assertConforms(t, "POST", "/projects", raw)

	_, _, raw = do(t, srv, "POST", "/api/v1/projects", `{"name":"y","kind":"docker","tier":"service","cmd":["a"]}`)
	assertConforms(t, "POST", "/projects", raw)

	_, _, raw = do(t, srv, "GET", "/api/v1/projects", "")
	assertConforms(t, "GET", "/projects", raw)

	_, obj, raw := do(t, srv, "GET", "/api/v1/detect?path="+t.TempDir(), "")
	if obj["ok"] != false {
		t.Fatalf("empty dir should not be detected: %s", raw)
	}
	assertConforms(t, "GET", "/detect", raw)

	_, _, raw = do(t, srv, "POST", "/api/v1/groups", `{"name":"g","projects":["x"]}`)
	assertConforms(t, "POST", "/groups", raw)
}

// Documented gap, not a feature: the daemon does not validate group names yet,
// and openapi.yaml says so on the Group schema. If validation is added, update
// both together.
func TestGroupNameIsNotValidatedYet(t *testing.T) {
	_, srv := newTestDaemon(t)
	code, _, raw := do(t, srv, "POST", "/api/v1/groups", `{}`)
	if code != 200 {
		t.Fatalf("POST /groups {} = %d %s; update the Group description in openapi.yaml if this is now validated", code, raw)
	}
}
