package api

// The consent routes Studio's "Authorize API access" page calls: describe an authorization request,
// approve it and decline it.

// routesOAuthConsent registers GET /platform/oauth/authorizations/{id} and POST and DELETE
// /platform/organizations/{slug}/oauth/authorizations/{id}. routesOAuth calls it.
func (s *Server) routesOAuthConsent(add func(string, handlerFunc)) {}
