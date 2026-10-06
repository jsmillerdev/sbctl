package api

import (
	"encoding/json"
	"testing"
)

// Every stub must at least be valid JSON with its required top-level fields; the
// checks below also pin the shapes the CLI is known to be strict about.
func TestMinimalValueAllOperations(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		if !o.JSON {
			continue
		}
		v := MinimalValue(o.Response)
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("%s: %v", o.Key(), err)
		}
		if o.Response.Value != nil && o.Response.Value.Type.Is("array") {
			if _, ok := v.([]any); !ok {
				t.Errorf("%s: array schema produced %T", o.Key(), v)
			}
		}
	}
}

func TestMinimalValueShapes(t *testing.T) {
	ops, _ := Operations()
	by := map[string]*Operation{}
	for _, o := range ops {
		by[o.Key()] = o
	}
	got := MinimalValue(by["GET /v1/projects/{ref}/health"].Response)
	if arr, ok := got.([]any); !ok || len(arr) != 0 {
		t.Errorf("health: %v", got)
	}
	got = MinimalValue(by["GET /v1/projects/{ref}/types/typescript"].Response)
	if m, ok := got.(map[string]any); !ok || m["types"] != "" {
		t.Errorf("types: %v", got)
	}
	got = MinimalValue(by["GET /v1/projects/{ref}"].Response)
	m := got.(map[string]any)
	if ref, _ := m["ref"].(string); len(ref) != 20 {
		t.Errorf("ref should satisfy minLength 20: %q", ref)
	}
	if m["status"] != "INACTIVE" {
		t.Errorf("enum should use first member: %v", m["status"])
	}
}
