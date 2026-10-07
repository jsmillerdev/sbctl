package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// ignoredKeys are prose and examples: a reworded description is not drift in what a client
// can send or receive.
var ignoredKeys = map[string]bool{
	"description": true, "summary": true, "title": true, "example": true, "examples": true,
	"externalDocs": true, "x-codeSamples": true,
}

// Change is one operation or schema that exists on both sides but differs.
type Change struct {
	Name  string
	Lines []string // what differs, at most maxLines, the rest counted
	More  int
}

// Report is the difference between a pinned and an upstream OpenAPI document.
type Report struct {
	OpsAdded, OpsRemoved []string
	OpsChanged           []Change
	SchemasAdded         []string
	SchemasRemoved       []string
	SchemasChanged       []Change
	Version              [2]string // info.version, pinned and upstream, when they differ
}

// Empty reports whether the two documents agree on everything the report covers.
func (r *Report) Empty() bool {
	return len(r.OpsAdded) == 0 && len(r.OpsRemoved) == 0 && len(r.OpsChanged) == 0 &&
		len(r.SchemasAdded) == 0 && len(r.SchemasRemoved) == 0 && len(r.SchemasChanged) == 0
}

const maxLines = 6

// Diff compares pinned with upstream.
func Diff(pinned, upstream map[string]any) *Report {
	r := &Report{}
	pv, uv := infoVersion(pinned), infoVersion(upstream)
	if pv != uv {
		r.Version = [2]string{pv, uv}
	}
	po, uo := operations(pinned), operations(upstream)
	for _, k := range sortedKeys(uo) {
		if _, ok := po[k]; !ok {
			r.OpsAdded = append(r.OpsAdded, label(k, uo[k]))
		}
	}
	for _, k := range sortedKeys(po) {
		u, ok := uo[k]
		if !ok {
			r.OpsRemoved = append(r.OpsRemoved, label(k, po[k]))
			continue
		}
		if c := compare(k, po[k], u); c != nil {
			r.OpsChanged = append(r.OpsChanged, *c)
		}
	}
	ps, us := schemas(pinned), schemas(upstream)
	for _, k := range sortedKeys(us) {
		if _, ok := ps[k]; !ok {
			r.SchemasAdded = append(r.SchemasAdded, k)
		}
	}
	for _, k := range sortedKeys(ps) {
		u, ok := us[k]
		if !ok {
			r.SchemasRemoved = append(r.SchemasRemoved, k)
			continue
		}
		if c := compare(k, ps[k], u); c != nil {
			r.SchemasChanged = append(r.SchemasChanged, *c)
		}
	}
	return r
}

func infoVersion(doc map[string]any) string {
	info, _ := doc["info"].(map[string]any)
	v, _ := info["version"].(string)
	return v
}

// operations maps "METHOD /path" to the operation object.
func operations(doc map[string]any) map[string]any {
	out := map[string]any{}
	paths, _ := doc["paths"].(map[string]any)
	for p, item := range paths {
		m, _ := item.(map[string]any)
		for _, method := range httpMethods {
			if op, ok := m[method]; ok {
				out[strings.ToUpper(method)+" "+p] = op
			}
		}
		// Parameters declared once for the whole path item apply to every method.
		if shared, ok := m["parameters"]; ok {
			for _, method := range httpMethods {
				if op, ok := m[method].(map[string]any); ok {
					cp := map[string]any{}
					for k, v := range op {
						cp[k] = v
					}
					cp["x-path-parameters"] = shared
					out[strings.ToUpper(method)+" "+p] = cp
				}
			}
		}
	}
	return out
}

func schemas(doc map[string]any) map[string]any {
	c, _ := doc["components"].(map[string]any)
	s, _ := c["schemas"].(map[string]any)
	if s == nil {
		return map[string]any{}
	}
	return s
}

func label(key string, op any) string {
	if m, ok := op.(map[string]any); ok {
		if id, ok := m["operationId"].(string); ok && id != "" {
			return key + " (" + id + ")"
		}
	}
	return key
}

func compare(name string, a, b any) *Change {
	var lines []string
	deepDiff("", a, b, &lines)
	if len(lines) == 0 {
		return nil
	}
	c := &Change{Name: name}
	if len(lines) > maxLines {
		c.More = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	c.Lines = lines
	return c
}

// deepDiff appends a line for every difference between a and b, below path.
func deepDiff(path string, a, b any, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", at(path), brief(a), brief(b)))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		for _, k := range sortedBool(keys) {
			if ignoredKeys[k] {
				continue
			}
			sub := join(path, k)
			x, inA := av[k]
			y, inB := bv[k]
			switch {
			case inA && !inB:
				*out = append(*out, fmt.Sprintf("%s: removed (was %s)", sub, brief(x)))
			case !inA && inB:
				*out = append(*out, fmt.Sprintf("%s: added (%s)", sub, brief(y)))
			default:
				deepDiff(sub, x, y, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", at(path), brief(a), brief(b)))
			return
		}
		if ka, kb := keyed(av), keyed(bv); ka != nil && kb != nil {
			keys := map[string]bool{}
			for k := range ka {
				keys[k] = true
			}
			for k := range kb {
				keys[k] = true
			}
			for _, k := range sortedBool(keys) {
				sub := path + "[" + k + "]"
				x, inA := ka[k]
				y, inB := kb[k]
				switch {
				case inA && !inB:
					*out = append(*out, fmt.Sprintf("%s: removed", sub))
				case !inA && inB:
					*out = append(*out, fmt.Sprintf("%s: added", sub))
				default:
					deepDiff(sub, x, y, out)
				}
			}
			return
		}
		// Enumerations and required lists are sets: report members, not positions.
		if allScalars(av) && allScalars(bv) {
			add, del := setDiff(av, bv)
			if len(add) > 0 {
				*out = append(*out, fmt.Sprintf("%s: added %s", at(path), strings.Join(add, ", ")))
			}
			if len(del) > 0 {
				*out = append(*out, fmt.Sprintf("%s: removed %s", at(path), strings.Join(del, ", ")))
			}
			return
		}
		n := len(av)
		if len(bv) > n {
			n = len(bv)
		}
		for i := 0; i < n; i++ {
			sub := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(bv):
				*out = append(*out, fmt.Sprintf("%s: removed", sub))
			case i >= len(av):
				*out = append(*out, fmt.Sprintf("%s: added", sub))
			default:
				deepDiff(sub, av[i], bv[i], out)
			}
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", at(path), brief(a), brief(b)))
		}
	}
}

// keyed indexes a parameter list by "in:name"; nil when the list is not one.
func keyed(list []any) map[string]any {
	out := map[string]any{}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil
		}
		name, ok1 := m["name"].(string)
		in, ok2 := m["in"].(string)
		if !ok1 || !ok2 {
			return nil
		}
		out[in+":"+name] = m
	}
	return out
}

func allScalars(list []any) bool {
	for _, e := range list {
		switch e.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func setDiff(a, b []any) (added, removed []string) {
	in := func(list []any) map[string]bool {
		m := map[string]bool{}
		for _, e := range list {
			m[fmt.Sprint(e)] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	for k := range mb {
		if !ma[k] {
			added = append(added, k)
		}
	}
	for k := range ma {
		if !mb[k] {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

func brief(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			return ref
		}
		return fmt.Sprintf("an object with %d keys", len(t))
	case []any:
		return fmt.Sprintf("a list of %d", len(t))
	case string:
		return fmt.Sprintf("%q", t)
	default:
		return fmt.Sprint(t)
	}
}

func at(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Markdown renders the report for one spec. A limit above zero caps each list.
func (r *Report) Markdown(name string, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s spec\n\n", name)
	if r.Empty() {
		if r.Version[0] != r.Version[1] {
			fmt.Fprintf(&b, "No operation or schema differs; info.version moved from %q to %q.\n\n", r.Version[0], r.Version[1])
		} else {
			b.WriteString("No difference.\n\n")
		}
		return b.String()
	}
	if r.Version[0] != r.Version[1] {
		fmt.Fprintf(&b, "info.version: %q -> %q\n\n", r.Version[0], r.Version[1])
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "**%s (%d)**\n\n", title, len(items))
		for i, it := range items {
			if limit > 0 && i >= limit {
				fmt.Fprintf(&b, "- ... %d more\n", len(items)-limit)
				break
			}
			fmt.Fprintf(&b, "- `%s`\n", it)
		}
		b.WriteString("\n")
	}
	changes := func(title string, items []Change) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "**%s (%d)**\n\n", title, len(items))
		for i, c := range items {
			if limit > 0 && i >= limit {
				fmt.Fprintf(&b, "- ... %d more\n", len(items)-limit)
				break
			}
			fmt.Fprintf(&b, "- `%s`\n", c.Name)
			for _, l := range c.Lines {
				fmt.Fprintf(&b, "  - %s\n", l)
			}
			if c.More > 0 {
				fmt.Fprintf(&b, "  - ... %d more differences\n", c.More)
			}
		}
		b.WriteString("\n")
	}
	list("Operations added upstream", r.OpsAdded)
	list("Operations removed upstream", r.OpsRemoved)
	changes("Operations changed", r.OpsChanged)
	list("Schemas added upstream", r.SchemasAdded)
	list("Schemas removed upstream", r.SchemasRemoved)
	changes("Schemas changed", r.SchemasChanged)
	return b.String()
}
