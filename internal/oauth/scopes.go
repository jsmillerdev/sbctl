package oauth

import (
	"slices"
	"strings"
)

// The scope vocabulary: the 24 values of the platform spec's CreateOAuthAppBody. The pinned specs
// annotate operations (x-oauth-scope) with 22 of them; analytics:write and storage:write annotate
// none. Matching is exact: a write scope does not imply its read scope.
const (
	ScopeAnalyticsRead        = "analytics:read"
	ScopeAnalyticsWrite       = "analytics:write"
	ScopeAnalyticsConfigRead  = "analytics_config:read"
	ScopeAnalyticsConfigWrite = "analytics_config:write"
	ScopeAuthRead             = "auth:read"
	ScopeAuthWrite            = "auth:write"
	ScopeDatabaseRead         = "database:read"
	ScopeDatabaseWrite        = "database:write"
	ScopeDomainsRead          = "domains:read"
	ScopeDomainsWrite         = "domains:write"
	ScopeEdgeFunctionsRead    = "edge_functions:read"
	ScopeEdgeFunctionsWrite   = "edge_functions:write"
	ScopeEnvironmentRead      = "environment:read"
	ScopeEnvironmentWrite     = "environment:write"
	ScopeOrganizationsRead    = "organizations:read"
	ScopeOrganizationsWrite   = "organizations:write"
	ScopeProjectsRead         = "projects:read"
	ScopeProjectsWrite        = "projects:write"
	ScopeRestRead             = "rest:read"
	ScopeRestWrite            = "rest:write"
	ScopeSecretsRead          = "secrets:read"
	ScopeSecretsWrite         = "secrets:write"
	ScopeStorageRead          = "storage:read"
	ScopeStorageWrite         = "storage:write"
)

// AllScopes are the 24 scopes, in the order of the spec's enum (alphabetical). A manual app may
// hold any of them. Treat the slice as read-only.
var AllScopes = []string{
	ScopeAnalyticsRead, ScopeAnalyticsWrite, ScopeAnalyticsConfigRead, ScopeAnalyticsConfigWrite,
	ScopeAuthRead, ScopeAuthWrite, ScopeDatabaseRead, ScopeDatabaseWrite, ScopeDomainsRead, ScopeDomainsWrite,
	ScopeEdgeFunctionsRead, ScopeEdgeFunctionsWrite, ScopeEnvironmentRead, ScopeEnvironmentWrite,
	ScopeOrganizationsRead, ScopeOrganizationsWrite, ScopeProjectsRead, ScopeProjectsWrite,
	ScopeRestRead, ScopeRestWrite, ScopeSecretsRead, ScopeSecretsWrite, ScopeStorageRead, ScopeStorageWrite,
}

// AdvertisedScopes are the 13 scopes that scopes_supported lists in both discovery documents and
// that a dynamic app may hold: the ones the Supabase MCP server's calls need, as hosted
// advertises them, in hosted's order. Treat the slice as read-only.
var AdvertisedScopes = []string{
	ScopeOrganizationsRead, ScopeProjectsRead, ScopeProjectsWrite, ScopeDatabaseRead, ScopeDatabaseWrite,
	ScopeAnalyticsRead, ScopeSecretsRead, ScopeEdgeFunctionsRead, ScopeEdgeFunctionsWrite,
	ScopeEnvironmentRead, ScopeEnvironmentWrite, ScopeStorageRead, ScopeStorageWrite,
}

// ValidScope reports whether s is one of the 24 scopes.
func ValidScope(s string) bool { return slices.Contains(AllScopes, s) }

// IsAdvertised reports whether s is one of the 13 advertised scopes.
func IsAdvertised(s string) bool { return slices.Contains(AdvertisedScopes, s) }

// ParseScopes splits a scope parameter (RFC 6749 section 3.3: scope tokens separated by spaces)
// into its tokens, dropping empty ones and repeats and keeping the first-seen order. It does not
// check that the tokens are known. The result is nil for an empty parameter.
func ParseScopes(s string) []string {
	var out []string
	for _, t := range strings.Fields(s) {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// JoinScopes is the scope parameter of a list: the tokens separated by single spaces.
func JoinScopes(scopes []string) string { return strings.Join(scopes, " ") }

// NormalizeScopes returns the scopes sorted, without repeats, in a new slice (nil when empty). It is
// the form in which scopes are stored and compared.
func NormalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	out := slices.Clone(scopes)
	slices.Sort(out)
	return slices.Compact(out)
}

// IntersectScopes returns the scopes of a that b also holds, in the order of a and without
// repeats (nil when none).
func IntersectScopes(a, b []string) []string {
	var out []string
	for _, s := range a {
		if slices.Contains(b, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// UnionScopes returns the scopes of a followed by those of b that a lacks, without repeats (nil
// when both are empty).
func UnionScopes(a, b []string) []string {
	var out []string
	for _, s := range slices.Concat(a, b) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// SubsetOf reports whether every scope of sub is in super. The empty list is a subset of
// everything.
func SubsetOf(sub, super []string) bool {
	for _, s := range sub {
		if !slices.Contains(super, s) {
			return false
		}
	}
	return true
}
