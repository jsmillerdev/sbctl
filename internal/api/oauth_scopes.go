package api

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/supavise/supavise/internal/oauth"
)

// The scope gate. An OAuth access token carries scopes, and each operation of the pinned specs is
// annotated with the one scope it needs (x-oauth-scope, read into Operation.Scope). The gate is
// default deny: a route that is in no spec, that the spec does not annotate, or that is not in the
// map below is not available to OAuth tokens at all. Matching is exact; "write" does not imply
// "read".
//
// The gate runs in authorize (authz.go), after authenticate and before the permission rules, on every
// route including the ones open to any signed-in user. The permission rules still apply afterwards, so
// a token never exceeds the rights its user holds in the grant's organization.

// oauthScopeOverrides are the operations the specs leave unannotated that the Supabase MCP server
// calls. The storage tools read and write the project's storage config, and no operation anywhere is
// annotated storage:write, so without these the advertised scope could never be used. An override
// for an operation the spec has since annotated is a mistake (TestOAuthScopeOverrides).
var oauthScopeOverrides = map[string]string{
	"GET /v1/projects/{ref}/config/storage":   oauth.ScopeStorageRead,
	"PATCH /v1/projects/{ref}/config/storage": oauth.ScopeStorageWrite,
}

var (
	oauthScopesOnce sync.Once
	oauthScopes     map[string]string
)

// oauthScopeTable maps a route key ("METHOD /template") to the scope it needs: the specs'
// annotations plus oauthScopeOverrides. Built once.
func oauthScopeTable() map[string]string {
	oauthScopesOnce.Do(func() {
		m := map[string]string{}
		// If the specs cannot be loaded the table stays empty, which denies everything; build() fails
		// on the same error before the server serves a request.
		ops, _ := Operations()
		for _, op := range ops {
			if op.Scope != "" {
				m[op.Key()] = op.Scope
			}
		}
		for key, scope := range oauthScopeOverrides {
			m[key] = scope
		}
		oauthScopes = m
	})
	return oauthScopes
}

// oauthScopeOf returns the scope an OAuth token needs to call the route key ("METHOD /template",
// empty for a path no spec lists). ok is false when no scope allows the route.
func oauthScopeOf(key string) (scope string, ok bool) {
	if key == "" {
		return "", false
	}
	scope, ok = oauthScopeTable()[key]
	return scope, ok
}

// errOAuthNotAvailable refuses a route no scope opens: the profile, creating organizations, the node's
// health, every /v1/oauth operation, and any route that is in no spec.
var errOAuthNotAvailable = errf(http.StatusForbidden, "This operation is not available to OAuth tokens")

// oauthScopeGate holds an OAuth principal to the scopes of its grant for the route key. It returns nil
// for any other principal.
func (s *Server) oauthScopeGate(key string, p *Principal) error {
	if p == nil || p.OAuth == nil {
		return nil
	}
	need, ok := oauthScopeOf(key)
	if !ok {
		return errOAuthNotAvailable
	}
	for _, have := range p.OAuth.Scopes {
		if have == need {
			return nil
		}
	}
	// RFC 6750 section 3.1 with the resource metadata of RFC 9728: the client learns which scope to
	// ask for and where the authorization server is described.
	challenge := fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q, resource_metadata=%q`,
		need, oauth.ProtectedResourceMetadataURL(s.cfg.APIURL()))
	return &Error{
		Status:  http.StatusForbidden,
		Message: fmt.Sprintf("Insufficient scope: this operation needs the %s scope", need),
		Header:  http.Header{"Www-Authenticate": {challenge}},
	}
}
