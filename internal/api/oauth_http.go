package api

// The OAuth authorization server's endpoints: dynamic client registration, authorize, token and
// revoke (internal/oauth holds the rules; these handlers parse requests, apply the per-address
// limits and write the answers).

// oauthPublicRoutes are the operations of the pinned specs that a client calls before it has a
// credential, so that implemented() makes them authNone whatever their path (authFor would ask for
// a session on /platform and a token on /v1). A key that is not registered is skipped.
var oauthPublicRoutes = []string{
	"POST /platform/oauth/apps/register",
	"GET /v1/oauth/authorize",
	"POST /v1/oauth/token",
	"POST /v1/oauth/revoke",
}

// routesOAuth registers the OAuth routes of the specs: oauthPublicRoutes, and the consent routes
// through routesOAuthConsent. It is called whether or not [api] disable_oauth is set; the handlers
// answer 404 while it is (oauthDisabled).
func (s *Server) routesOAuth(add func(string, handlerFunc)) {
	s.routesOAuthConsent(add)
}
