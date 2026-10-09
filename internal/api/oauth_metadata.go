package api

// The two discovery documents of the OAuth authorization server and of the MCP resource.

// oauthWellKnown registers GET /.well-known/oauth-protected-resource/mcp and GET
// /.well-known/oauth-authorization-server on the mux, without credentials (authNone) and with the
// open CORS policy of oauth_cors.go. They are not operations of the specs, so they are not in the
// route table of implemented(). build calls it whether or not [api] disable_oauth is set; the
// handlers answer 404 while it is.
func (s *Server) oauthWellKnown(mux *muxSet) {}
