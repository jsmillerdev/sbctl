package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
)

// The three public Supabase OpenAPI documents, pinned (see gen/doc.go). They drive
// two things: the route table (every operation is served, implemented or stubbed)
// and the shape of the stub responses.
//
//go:embed gen/specs/v1.json gen/specs/v2.json gen/specs/platform.json
var specFS embed.FS

// Operation is one operation of the pinned specs.
type Operation struct {
	Spec     string // "v1", "v2" or "platform"
	Method   string
	Path     string // OpenAPI template, e.g. /v1/projects/{ref}
	ID       string // operationId
	Status   int    // lowest 2xx response code, 0 when the operation declares none
	Response *openapi3.SchemaRef
	// JSON is true when the success response is application/json.
	JSON bool
	// Streams is true for non-JSON success bodies (files); stubs send them empty.
	Streams bool
	// Scope is the OAuth scope the spec annotates the operation with (x-oauth-scope), "" when it has
	// none. An OAuth access token may call only an annotated operation (oauth_scopes.go).
	Scope string
}

// Key is "METHOD /path", the ServeMux pattern of the operation.
func (o *Operation) Key() string { return o.Method + " " + o.Path }

var (
	opsOnce sync.Once
	opsAll  []*Operation
	opsErr  error
)

// Operations returns every operation of the three specs, sorted by path then method.
func Operations() ([]*Operation, error) {
	opsOnce.Do(func() { opsAll, opsErr = loadOperations() })
	return opsAll, opsErr
}

func loadOperations() ([]*Operation, error) {
	var out []*Operation
	for _, name := range []string{"v1", "v2", "platform"} {
		b, err := specFS.ReadFile("gen/specs/" + name + ".json")
		if err != nil {
			return nil, err
		}
		doc, err := openapi3.NewLoader().LoadFromData(b)
		if err != nil {
			return nil, fmt.Errorf("api: load %s spec: %w", name, err)
		}
		for path, item := range doc.Paths.Map() {
			for method, op := range item.Operations() {
				o := &Operation{Spec: name, Method: strings.ToUpper(method), Path: path, ID: op.OperationID,
					Scope: extensionString(op.Extensions, "x-oauth-scope")}
				fillResponse(o, op)
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}

// extensionString reads a string-valued OpenAPI extension. The loader decodes extensions to plain
// Go values (a string here); raw JSON is accepted too. Anything else reads as "".
func extensionString(ext map[string]any, key string) string {
	switch v := ext[key].(type) {
	case string:
		return v
	case json.RawMessage:
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return ""
}

// fillResponse picks the lowest 2xx response of op as the success response.
func fillResponse(o *Operation, op *openapi3.Operation) {
	if op.Responses == nil {
		return
	}
	for code, r := range op.Responses.Map() {
		var n int
		if _, err := fmt.Sscanf(code, "%d", &n); err != nil || n < 200 || n > 299 || r.Value == nil {
			continue
		}
		if o.Status != 0 && n >= o.Status {
			continue
		}
		o.Status, o.Response, o.JSON, o.Streams = n, nil, false, false
		if mt := r.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil {
			o.Response, o.JSON = mt.Schema, true
		} else if len(r.Value.Content) > 0 {
			o.Streams = true
		}
	}
}

// pathParams returns the names of the {wildcards} of a ServeMux pattern path.
func pathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"))
		}
	}
	return out
}
