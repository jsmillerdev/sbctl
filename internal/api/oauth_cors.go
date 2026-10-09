package api

import (
	"net/http"
	"strings"
)

// The CORS policy of the OAuth endpoints (design 2.2). A browser-based MCP client (claude.ai) reads
// the discovery documents, registers itself and exchanges its code from another origin, and the
// pages cannot know which origin that is, so the policy is open: any origin, no credentials. None
// of these endpoints reads a cookie, so an open origin gives a page nothing it could not do with
// curl. Every other path keeps the dashboard policy of middleware.

const (
	oauthCORSMethods = "GET, POST, DELETE, OPTIONS"
	// oauthCORSHeaders are the request headers a page may send: the bearer, the bodies, and the
	// headers of the MCP transport that the gate of /mcp shares this policy for.
	oauthCORSHeaders = "authorization, content-type, mcp-protocol-version, mcp-session-id, last-event-id"
	// oauthCORSExpose are the response headers a page may read: the challenge of a 401 and the MCP session.
	oauthCORSExpose = "WWW-Authenticate, Mcp-Session-Id"
)

// setOAuthCORS writes the open policy into h. The remote MCP endpoint's gate (mcp.go) uses the same
// policy for /mcp.
func setOAuthCORS(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", oauthCORSMethods)
	h.Set("Access-Control-Allow-Headers", oauthCORSHeaders)
	h.Set("Access-Control-Expose-Headers", oauthCORSExpose)
}

// oauthCORSPath reports whether path is one the open policy covers: the discovery documents, the
// token and revocation endpoints and dynamic registration.
func oauthCORSPath(path string) bool {
	switch path {
	case "/v1/oauth/token", "/v1/oauth/revoke", "/platform/oauth/apps/register":
		return true
	}
	return strings.HasPrefix(path, "/.well-known/")
}

// oauthCORS applies the open CORS policy of the OAuth endpoints to the response when the request
// is for one of them: /.well-known/*, /v1/oauth/token, /v1/oauth/revoke and
// /platform/oauth/apps/register. Every other path is left to the dashboard policy of middleware.
//
// covered reports that the path belongs to the OAuth policy, so that middleware does not add the
// dashboard's. done reports that the request is finished (a preflight was answered with 204) and
// middleware returns. While [api] disable_oauth is set it covers nothing.
func (s *Server) oauthCORS(w http.ResponseWriter, r *http.Request) (covered, done bool) {
	if s.oauthDisabled() || !oauthCORSPath(r.URL.Path) {
		return false, false
	}
	h := w.Header()
	setOAuthCORS(h)
	if r.Method != http.MethodOptions {
		return true, false
	}
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true, true
}
