package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// Paths of the OAuth surface. All are served on the API origin (the issuer) except ConsentPath,
// which is Studio's page on the dashboard origin.
const (
	// MCPPath is the protected resource: the remote MCP endpoint.
	MCPPath                         = "/mcp"
	ProtectedResourceMetadataPath   = "/.well-known/oauth-protected-resource/mcp"
	AuthorizationServerMetadataPath = "/.well-known/oauth-authorization-server"
	AuthorizePath                   = "/v1/oauth/authorize"
	TokenPath                       = "/v1/oauth/token"
	RevokePath                      = "/v1/oauth/revoke"
	RegisterPath                    = "/platform/oauth/apps/register"
	ConsentPath                     = "/authorize"
)

func join(base, path string) string { return strings.TrimRight(base, "/") + path }

// ResourceURL is the resource identifier of the MCP endpoint (RFC 9728, RFC 8707): the issuer plus
// "/mcp". The issuer is the API origin from configuration (cfg.APIURL()), never a request's Host.
func ResourceURL(issuer string) string { return join(issuer, MCPPath) }

// ProtectedResourceMetadataURL is where the MCP endpoint's metadata document is served. The 401
// challenge of /mcp and the insufficient_scope challenge name it as resource_metadata.
func ProtectedResourceMetadataURL(issuer string) string {
	return join(issuer, ProtectedResourceMetadataPath)
}

// AuthorizationServerMetadataURL is where the authorization server metadata (RFC 8414) is served.
func AuthorizationServerMetadataURL(issuer string) string {
	return join(issuer, AuthorizationServerMetadataPath)
}

// AuthorizeURL, TokenURL, RevokeURL and RegisterURL are the endpoints the metadata advertises.
func AuthorizeURL(issuer string) string { return join(issuer, AuthorizePath) }
func TokenURL(issuer string) string     { return join(issuer, TokenPath) }
func RevokeURL(issuer string) string    { return join(issuer, RevokePath) }
func RegisterURL(issuer string) string  { return join(issuer, RegisterPath) }

// ConsentURL is where the authorization endpoint sends the browser: Studio's "Authorize API access"
// page, <dashboard>/authorize?auth_id=<id>[&organization_slug=<slug>]. orgSlug may be empty.
func ConsentURL(dashboardURL, authID, orgSlug string) string {
	u := join(dashboardURL, ConsentPath) + "?auth_id=" + url.QueryEscape(authID)
	if orgSlug != "" {
		u += "&organization_slug=" + url.QueryEscape(orgSlug)
	}
	return u
}

// AppendQuery returns rawURL with the key/value pairs added to its query, in the order given and
// after any query it has already. Keys and values are query-escaped. Use it to build every
// redirect to a client (the code, an error), so that an existing query of the redirect URI is
// kept. kv must have an even length; pass only the pairs to send (leave out an empty state).
func AppendQuery(rawURL string, kv ...string) (string, error) {
	if len(kv)%2 != 0 {
		return "", fmt.Errorf("oauth: AppendQuery needs key/value pairs, got %d strings", len(kv))
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	var q strings.Builder
	q.WriteString(u.RawQuery)
	for i := 0; i < len(kv); i += 2 {
		if q.Len() > 0 {
			q.WriteByte('&')
		}
		q.WriteString(url.QueryEscape(kv[i]))
		q.WriteByte('=')
		q.WriteString(url.QueryEscape(kv[i+1]))
	}
	u.RawQuery = q.String()
	return u.String(), nil
}
