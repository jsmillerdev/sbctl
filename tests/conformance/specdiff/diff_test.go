package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const pinned = `{
 "info": {"version": "1"},
 "paths": {
  "/v1/projects": {
   "get": {"operationId": "listProjects", "description": "old words",
     "parameters": [{"name": "limit", "in": "query", "required": false, "schema": {"type": "integer"}}],
     "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Project"}}}}}},
   "post": {"operationId": "createProject", "responses": {"201": {"description": "ok"}}}
  },
  "/v1/gone": {"delete": {"operationId": "gone", "responses": {"200": {"description": "ok"}}}}
 },
 "components": {"schemas": {
  "Project": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}, "name": {"type": "string", "description": "x"}}},
  "Old": {"type": "string"},
  "Status": {"type": "string", "enum": ["A", "B"]}
 }}}`

func TestNoDriftWhenOnlyProseChanges(t *testing.T) {
	up := strings.Replace(pinned, "old words", "new words", 1)
	up = strings.Replace(up, `"description": "x"`, `"description": "y"`, 1)
	if r := Diff(doc(t, pinned), doc(t, up)); !r.Empty() {
		t.Fatalf("reworded descriptions reported as drift: %+v", r)
	}
}

func TestDriftIsReadable(t *testing.T) {
	up := `{
 "info": {"version": "2"},
 "paths": {
  "/v1/projects": {
   "get": {"operationId": "listProjects",
     "parameters": [{"name": "limit", "in": "query", "required": true, "schema": {"type": "integer"}}, {"name": "cursor", "in": "query"}],
     "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/ProjectV2"}}}}}},
   "post": {"operationId": "createProject", "responses": {"201": {"description": "ok"}}}
  },
  "/v1/new": {"get": {"operationId": "fresh", "responses": {"200": {"description": "ok"}}}}
 },
 "components": {"schemas": {
  "Project": {"type": "object", "required": ["id", "name"], "properties": {"id": {"type": "integer"}, "name": {"type": "string"}}},
  "Status": {"type": "string", "enum": ["A", "B", "C"]},
  "Fresh": {"type": "string"}
 }}}`
	r := Diff(doc(t, pinned), doc(t, up))
	if len(r.OpsAdded) != 1 || r.OpsAdded[0] != "GET /v1/new (fresh)" {
		t.Errorf("added: %v", r.OpsAdded)
	}
	if len(r.OpsRemoved) != 1 || r.OpsRemoved[0] != "DELETE /v1/gone (gone)" {
		t.Errorf("removed: %v", r.OpsRemoved)
	}
	if len(r.OpsChanged) != 1 || r.OpsChanged[0].Name != "GET /v1/projects" {
		t.Fatalf("changed: %+v", r.OpsChanged)
	}
	got := strings.Join(r.OpsChanged[0].Lines, "\n")
	for _, want := range []string{"parameters[query:cursor]: added", "parameters[query:limit].required: false -> true", "#/components/schemas/ProjectV2"} {
		if !strings.Contains(got, want) {
			t.Errorf("operation change lacks %q:\n%s", want, got)
		}
	}
	if len(r.SchemasAdded) != 1 || r.SchemasAdded[0] != "Fresh" || len(r.SchemasRemoved) != 1 || r.SchemasRemoved[0] != "Old" {
		t.Errorf("schemas added %v removed %v", r.SchemasAdded, r.SchemasRemoved)
	}
	var names []string
	for _, c := range r.SchemasChanged {
		names = append(names, c.Name+"\n"+strings.Join(c.Lines, "\n"))
	}
	all := strings.Join(names, "\n")
	for _, want := range []string{"required: added name", "properties.id.type: \"string\" -> \"integer\"", "enum: added C"} {
		if !strings.Contains(all, want) {
			t.Errorf("schema changes lack %q:\n%s", want, all)
		}
	}
	md := r.Markdown("v1", 0)
	for _, want := range []string{"Operations added upstream (1)", "Operations removed upstream (1)", "Schemas changed (2)", "info.version"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestIdenticalDocumentsAreEmpty(t *testing.T) {
	if r := Diff(doc(t, pinned), doc(t, pinned)); !r.Empty() || r.Version[0] != r.Version[1] {
		t.Fatalf("identical documents differ: %+v", r)
	}
}

func TestLongDifferencesAreCounted(t *testing.T) {
	var lines []string
	a := map[string]any{}
	b := map[string]any{}
	for i := 0; i < 10; i++ {
		b[string(rune('a'+i))] = 1.0
	}
	deepDiff("", a, b, &lines)
	if len(lines) != 10 {
		t.Fatalf("lines: %v", lines)
	}
	c := compare("S", a, b)
	if c == nil || len(c.Lines) != maxLines || c.More != 4 {
		t.Fatalf("change: %+v", c)
	}
}

// "description" and "title" are prose as a key of a schema, and field names as a key of its
// properties. Dropping or retyping such a field changes what a client can send.
func TestFieldsNamedLikeProseAreDrift(t *testing.T) {
	const a = `{"components": {"schemas": {"Key": {"type": "object", "description": "prose",
	  "properties": {"description": {"type": "string"}, "title": {"type": "string"}, "name": {"type": "string", "title": "Name"}}}}}}`
	const b = `{"components": {"schemas": {"Key": {"type": "object", "description": "reworded",
	  "properties": {"title": {"type": "integer"}, "name": {"type": "string", "title": "Renamed"}}}}}}`
	r := Diff(doc(t, a), doc(t, b))
	if len(r.SchemasChanged) != 1 {
		t.Fatalf("schemas changed: %+v", r.SchemasChanged)
	}
	got := strings.Join(r.SchemasChanged[0].Lines, "\n")
	for _, want := range []string{"properties.description: removed", "properties.title.type: \"string\" -> \"integer\""} {
		if !strings.Contains(got, want) {
			t.Errorf("drift lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "properties.name") || strings.Contains(got, "reworded") {
		t.Errorf("prose reported as drift:\n%s", got)
	}
	// A field called "properties" holds a schema, whose own prose is still ignored.
	const c = `{"components": {"schemas": {"S": {"properties": {"properties": {"type": "string", "description": "x"}}}}}}`
	const d = `{"components": {"schemas": {"S": {"properties": {"properties": {"type": "string", "description": "y"}}}}}}`
	if r := Diff(doc(t, c), doc(t, d)); !r.Empty() {
		t.Errorf("prose under a field named properties reported as drift: %+v", r)
	}
}
