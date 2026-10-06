package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Stubs. Every operation of the pinned specs that has no handler answers with the
// smallest valid instance of its success schema: required fields only, arrays empty,
// strings shaped by format/pattern/length, enums at their first member. The Supabase
// CLI and Studio decode strictly, so "{}" is not an acceptable stub.

const maxStubDepth = 12

// MinimalValue returns the smallest JSON-compatible value valid for s.
func MinimalValue(s *openapi3.SchemaRef) any { return minimal(s, 0) }

func minimal(ref *openapi3.SchemaRef, depth int) any {
	if ref == nil || ref.Value == nil || depth > maxStubDepth {
		return nil
	}
	s := ref.Value
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	if s.Default != nil {
		return s.Default
	}
	if len(s.AllOf) > 0 {
		return mergeAllOf(s, depth)
	}
	if len(s.OneOf) > 0 {
		return minimal(s.OneOf[0], depth+1)
	}
	if len(s.AnyOf) > 0 {
		return minimal(s.AnyOf[0], depth+1)
	}
	types := s.Type
	switch {
	case types.Is("object") || (types.Permits("object") && len(s.Properties) > 0) || (types == nil && len(s.Properties) > 0):
		return minimalObject(s, depth)
	case types.Is("array"):
		n := int(s.MinItems)
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, minimal(s.Items, depth+1))
		}
		return out
	case types.Is("string"):
		return minimalString(s)
	case types.Is("integer"):
		return minimalNumber(s, true)
	case types.Is("number"):
		return minimalNumber(s, false)
	case types.Is("boolean"):
		return false
	case types.Is("null"):
		return nil
	}
	// Untyped schema ("any"): an empty object is the most permissive useful value.
	return map[string]any{}
}

func minimalObject(s *openapi3.Schema, depth int) any {
	out := map[string]any{}
	for _, name := range s.Required {
		p, ok := s.Properties[name]
		if !ok {
			out[name] = nil
			continue
		}
		out[name] = minimal(p, depth+1)
	}
	return out
}

func mergeAllOf(s *openapi3.Schema, depth int) any {
	merged := map[string]any{}
	for _, part := range s.AllOf {
		v := minimal(part, depth+1)
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		for k, x := range m {
			merged[k] = x
		}
	}
	for k, x := range minimalObject(s, depth).(map[string]any) {
		merged[k] = x
	}
	return merged
}

func minimalString(s *openapi3.Schema) string {
	var v string
	switch s.Format {
	case "uuid":
		v = "00000000-0000-0000-0000-000000000000"
	case "date-time":
		v = "1970-01-01T00:00:00.000Z"
	case "date":
		v = "1970-01-01"
	case "uri", "url":
		v = "http://localhost"
	case "email":
		v = "nobody@example.com"
	case "ipv4":
		v = "0.0.0.0"
	}
	if v == "" && s.Pattern != "" {
		v = fromPattern(s.Pattern)
	}
	for uint64(len(v)) < s.MinLength {
		v += "a"
	}
	if s.MaxLength != nil && uint64(len(v)) > *s.MaxLength {
		v = v[:*s.MaxLength]
	}
	return v
}

// fromPattern handles the few patterns the Supabase specs use for plain tokens.
func fromPattern(p string) string {
	switch {
	case strings.HasPrefix(p, "^[a-z]+$"):
		return "a"
	case strings.HasPrefix(p, `^[\w-]+$`), strings.HasPrefix(p, "^[a-zA-Z0-9_-]+$"):
		return "a"
	}
	return ""
}

func minimalNumber(s *openapi3.Schema, integer bool) any {
	v := 0.0
	if s.Min != nil && *s.Min > v {
		v = *s.Min
	}
	if s.Max != nil && *s.Max < v {
		v = *s.Max
	}
	if integer {
		return int64(v)
	}
	return v
}

// stubHandler serves op's minimal success response.
func stubHandler(op *Operation) http.HandlerFunc {
	var body []byte
	if op.JSON {
		body, _ = json.Marshal(MinimalValue(op.Response))
	}
	status := op.Status
	if status == 0 {
		status = http.StatusOK
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sbctl-Stub", "true")
		if body == nil {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// StubKeys lists the routes a server with the given implemented keys stubs, for the
// README and the spec-diff job. Keys are "METHOD /path".
func StubKeys(implemented map[string]bool) []string {
	ops, _ := Operations()
	var out []string
	for _, o := range ops {
		if !implemented[o.Key()] {
			out = append(out, o.Key())
		}
	}
	sort.Strings(out)
	return out
}
