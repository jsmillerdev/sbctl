package api

import "net/http"

// oauthCORS applies the open CORS policy of the OAuth endpoints to the response when the request
// is for one of them: /.well-known/*, /v1/oauth/token, /v1/oauth/revoke and
// /platform/oauth/apps/register. Every other path is left to the dashboard policy of middleware.
//
// covered reports that the path belongs to the OAuth policy, so that middleware does not add the
// dashboard's. done reports that the request is finished (a preflight was answered with 204) and
// middleware returns. While [api] disable_oauth is set it covers nothing.
func (s *Server) oauthCORS(w http.ResponseWriter, r *http.Request) (covered, done bool) {
	return false, false
}
