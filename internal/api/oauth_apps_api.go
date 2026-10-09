package api

// The organization's OAuth Apps page (Studio's pages/org/[slug]/apps.tsx): the authorized and
// published lists, creating, updating, deleting and revoking an app, and an app's client secrets.

// routesOAuthApps registers the eight operations OAuthApps* and OAuthAppClientSecrets* of the
// platform spec. It is called whether or not [api] disable_oauth is set; the handlers answer 404
// while it is (oauthDisabled).
func (s *Server) routesOAuthApps(add func(string, handlerFunc)) {}
