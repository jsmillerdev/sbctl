package api

import (
	"net/http"

	"github.com/supavise/supavise/internal/oauth"
)

// The two discovery documents of the OAuth authorization server (RFC 8414) and of the MCP resource
// (RFC 9728). Their shapes follow what hosted serves (api.supabase.com and mcp.supabase.com), with the
// differences of design 2.3 and 2.17.

// oauthMetadataCache is the Cache-Control of a discovery document: a client may reuse it for an hour, as hosted allows.
const oauthMetadataCache = "public, max-age=3600"

// oauthProtectedResource is the document at /.well-known/oauth-protected-resource/mcp.
type oauthProtectedResource struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
	ScopesSupported        []string `json:"scopes_supported"`
}

// oauthServerMetadata is the document at /.well-known/oauth-authorization-server.
type oauthServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint"`
	RevocationEndpoint                         string   `json:"revocation_endpoint"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	ResponseModesSupported                     []string `json:"response_modes_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported     []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	ScopesSupported                            []string `json:"scopes_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// protectedResourceMetadata builds the document for issuer. It takes the issuer from configuration
// and never from the request, so a request with a forged Host header cannot make a client trust
// another authorization server.
func protectedResourceMetadata(issuer string) oauthProtectedResource {
	return oauthProtectedResource{
		Resource:               oauth.ResourceURL(issuer),
		AuthorizationServers:   []string{issuer},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Supavise MCP",
		ScopesSupported:        append([]string(nil), oauth.AdvertisedScopes...),
	}
}

// authorizationServerMetadata builds the document for issuer.
func authorizationServerMetadata(issuer string) oauthServerMetadata {
	authMethods := []string{oauth.AuthMethodBasic, oauth.AuthMethodPost}
	return oauthServerMetadata{
		Issuer:                                     issuer,
		AuthorizationEndpoint:                      oauth.AuthorizeURL(issuer),
		TokenEndpoint:                              oauth.TokenURL(issuer),
		RegistrationEndpoint:                       oauth.RegisterURL(issuer),
		RevocationEndpoint:                         oauth.RevokeURL(issuer),
		ResponseTypesSupported:                     []string{oauth.ResponseTypeCode},
		ResponseModesSupported:                     []string{oauth.ResponseModeQuery},
		GrantTypesSupported:                        []string{oauth.GrantTypeAuthorizationCode, oauth.GrantTypeRefreshToken},
		TokenEndpointAuthMethodsSupported:          authMethods,
		RevocationEndpointAuthMethodsSupported:     authMethods,
		CodeChallengeMethodsSupported:              []string{oauth.PKCEMethodS256},
		ScopesSupported:                            append([]string(nil), oauth.AdvertisedScopes...),
		AuthorizationResponseIssParameterSupported: true,
	}
}

// oauthWellKnown registers GET /.well-known/oauth-protected-resource/mcp and GET
// /.well-known/oauth-authorization-server on the mux, without credentials (authNone) and with the
// open CORS policy of oauth_cors.go. They are not operations of the specs, so they are not in the
// route table of implemented(). build calls it whether or not [api] disable_oauth is set; the
// handlers answer 404 while it is (oauthGuard).
func (s *Server) oauthWellKnown(mux *muxSet) {
	mux.handle("GET "+oauth.ProtectedResourceMetadataPath, s.wrap("", authNone, s.oauthGuard(func(w http.ResponseWriter, r *http.Request) error {
		return writeMetadata(w, protectedResourceMetadata(s.cfg.APIURL()))
	})))
	mux.handle("GET "+oauth.AuthorizationServerMetadataPath, s.wrap("", authNone, s.oauthGuard(func(w http.ResponseWriter, r *http.Request) error {
		return writeMetadata(w, authorizationServerMetadata(s.cfg.APIURL()))
	})))
}

// writeMetadata answers 200 with a discovery document that clients may cache for an hour.
func writeMetadata(w http.ResponseWriter, doc any) error {
	w.Header().Set("Cache-Control", oauthMetadataCache)
	writeJSON(w, http.StatusOK, doc)
	return nil
}
