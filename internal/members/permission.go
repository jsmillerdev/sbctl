package members

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Permission is one entry of GET /platform/profile/permissions (the platform spec's
// AccessControlPermission). Actions and Resources may contain "%" as a wildcard. A nil
// Condition is unconditional; otherwise it is a json-logic rule evaluated against the data of
// the check ({"resource_name": <resource>, "resource": {"role_id": n}}).
type Permission struct {
	Actions          []string
	Resources        []string
	Condition        any
	OrganizationID   int64
	OrganizationSlug string
	// ProjectRefs non-empty makes the entry apply to those projects only.
	ProjectRefs []string
	// Restrictive entries deny: a matching restrictive entry beats any number of grants.
	Restrictive bool
}

var (
	wildcardMu    sync.Mutex
	wildcardCache = map[string]*regexp.Regexp{}
)

// wildcard compiles a permission action or resource ("%" matches anything), as Studio's
// useCheckPermissions does: the pattern must match the whole string.
func wildcard(pattern string) *regexp.Regexp {
	wildcardMu.Lock()
	defer wildcardMu.Unlock()
	if re, ok := wildcardCache[pattern]; ok {
		return re
	}
	parts := strings.Split(pattern, "%")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
	wildcardCache[pattern] = re
	return re
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == s || wildcard(p).MatchString(s) {
			return true
		}
	}
	return false
}

func (p *Permission) matches(action, resource string) bool {
	return matchAny(p.Actions, action) && matchAny(p.Resources, resource)
}

func (p *Permission) holds(data map[string]any) bool {
	return p.Condition == nil || truthy(applyLogic(p.Condition, data))
}

// Check answers whether permissions allow action on resource, with the exact semantics of
// Studio's doPermissionsCheck (apps/studio/hooks/misc/useCheckPermissions.ts) so the
// dashboard and the server agree: for a project check, entries scoped to that project take
// over when any of them matches the action and resource; otherwise only the organization-wide
// entries count. Within the chosen entries a matching restrictive entry denies, and
// otherwise one matching grant allows. An empty list denies everything. data is merged into
// the condition data under resource, e.g. {"resource": {"role_id": 1}}.
func Check(perms []Permission, action, resource string, data map[string]any, orgSlug, projectRef string) bool {
	d := map[string]any{"resource_name": resource}
	for k, v := range data {
		d[k] = v
	}
	if projectRef != "" {
		var scoped []*Permission
		for i := range perms {
			p := &perms[i]
			if p.OrganizationSlug == orgSlug && p.matches(action, resource) && slices.Contains(p.ProjectRefs, projectRef) {
				scoped = append(scoped, p)
			}
		}
		if len(scoped) > 0 {
			return decide(scoped, d)
		}
	}
	var org []*Permission
	for i := range perms {
		p := &perms[i]
		if len(p.ProjectRefs) == 0 && p.OrganizationSlug == orgSlug && p.matches(action, resource) {
			org = append(org, p)
		}
	}
	return decide(org, d)
}

func decide(perms []*Permission, data map[string]any) bool {
	for _, p := range perms {
		if p.Restrictive && p.holds(data) {
			return false
		}
	}
	for _, p := range perms {
		if !p.Restrictive && p.holds(data) {
			return true
		}
	}
	return false
}

// ---- json-logic ------------------------------------------------------------

// applyLogic evaluates the subset of json-logic the role conditions use: var, ==, !=, in,
// and, or, !. Anything else evaluates to nil (false), so an unknown operator denies a
// grant and, on a restrictive entry, does not deny.
func applyLogic(rule any, data map[string]any) any {
	switch r := rule.(type) {
	case []any:
		out := make([]any, len(r))
		for i, x := range r {
			out[i] = applyLogic(x, data)
		}
		return out
	case map[string]any:
		if len(r) != 1 {
			return nil
		}
		for op, raw := range r {
			args, _ := raw.([]any)
			if _, isList := raw.([]any); !isList {
				args = []any{raw}
			}
			switch op {
			case "var":
				if len(args) == 0 {
					return nil
				}
				path, _ := applyLogic(args[0], data).(string)
				return lookup(data, path)
			case "==", "!=":
				if len(args) != 2 {
					return nil
				}
				eq := looseEqual(applyLogic(args[0], data), applyLogic(args[1], data))
				if op == "!=" {
					return !eq
				}
				return eq
			case "in":
				if len(args) != 2 {
					return nil
				}
				needle := applyLogic(args[0], data)
				switch hay := applyLogic(args[1], data).(type) {
				case []any:
					for _, x := range hay {
						if looseEqual(needle, x) {
							return true
						}
					}
				case string:
					s, _ := needle.(string)
					return s != "" && strings.Contains(hay, s)
				}
				return false
			case "and":
				var last any = true
				for _, a := range args {
					last = applyLogic(a, data)
					if !truthy(last) {
						return last
					}
				}
				return last
			case "or":
				var last any = false
				for _, a := range args {
					last = applyLogic(a, data)
					if truthy(last) {
						return last
					}
				}
				return last
			case "!":
				if len(args) == 0 {
					return nil
				}
				return !truthy(applyLogic(args[0], data))
			default:
				return nil
			}
		}
	}
	return rule
}

func lookup(data map[string]any, path string) any {
	var cur any = data
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	}
	return true
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func looseEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		fb, ok := toFloat(b)
		return ok && fa == fb
	}
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	return reflect.DeepEqual(a, b)
}
