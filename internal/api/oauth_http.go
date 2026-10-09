package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/supavise/supavise/internal/oauth"
)

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

// oauthBodyMax bounds the bodies of the endpoints that take no credential: none of their requests
// is larger than a few kilobytes.
const oauthBodyMax = 64 << 10

// errOAuthEndpointOff is what an OAuth route answers while [api] disable_oauth is set.
var errOAuthEndpointOff = errf(http.StatusNotFound, "Not Found")

// routesOAuth registers the OAuth routes of the specs: oauthPublicRoutes, and the consent routes
// through routesOAuthConsent. It is called whether or not [api] disable_oauth is set; the handlers
// answer 404 while it is (oauthDisabled).
func (s *Server) routesOAuth(add func(string, handlerFunc)) {
	lim := newOAuthLimits()
	add("POST /platform/oauth/apps/register", func(w http.ResponseWriter, r *http.Request) error {
		return s.oauthRegister(w, r, lim)
	})
	add("GET /v1/oauth/authorize", func(w http.ResponseWriter, r *http.Request) error {
		return s.oauthAuthorize(w, r, lim)
	})
	add("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) error {
		return s.oauthToken(w, r, lim)
	})
	add("POST /v1/oauth/revoke", func(w http.ResponseWriter, r *http.Request) error {
		return s.oauthRevoke(w, r, lim)
	})
	s.routesOAuthConsent(add)
}

// oauthClient names the caller for rate limiting (see claimClient).
func (s *Server) oauthClient(r *http.Request) string { return claimClient(r, s.trustForwarded(r)) }

// ---- answers ---------------------------------------------------------------------------------

// oauthErrorBody is the error answer of the token, registration and revocation endpoints: RFC 6749
// section 5.2 with the Management API's "message" key added.
type oauthErrorBody struct {
	Message          string `json:"message"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// noStore marks a response that carries or concerns a secret.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// basicChallenge is the WWW-Authenticate of a 401 to a client that authenticated with Basic.
const basicChallenge = `Basic realm="Supavise OAuth", charset="UTF-8"`

// writeOAuthError answers with an OAuth error. basic says that the client used the Authorization
// header, which a 401 then challenges (RFC 6749 section 5.2).
func writeOAuthError(w http.ResponseWriter, e *oauth.Error, basic bool) {
	noStore(w)
	status := e.HTTPStatus()
	if status == http.StatusUnauthorized && basic {
		w.Header().Set("WWW-Authenticate", basicChallenge)
	}
	msg := e.Description
	if msg == "" {
		msg = e.Code
	}
	writeJSON(w, status, oauthErrorBody{Message: msg, Error: e.Code, ErrorDescription: e.Description})
}

// oauthFail answers for an error the Service returned to a JSON endpoint. A protocol error is
// written as it is; a cap is a 429; a cancelled request is left to fail; anything else is logged
// and answered as server_error without its detail.
func (s *Server) oauthFail(w http.ResponseWriter, r *http.Request, err error, basic bool) error {
	var oe *oauth.Error
	switch {
	case errors.As(err, &oe):
		writeOAuthError(w, oe, basic)
		return nil
	case errors.Is(err, oauth.ErrLimit):
		noStore(w)
		w.Header().Set("Retry-After", "600")
		return errf(http.StatusTooManyRequests, "Too many requests of this kind are open at the moment; try again later")
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		return err
	}
	s.log.Error("an OAuth request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeOAuthError(w, oauth.NewError(oauth.CodeServerError, "The server could not complete the request."), basic)
	return nil
}

// ---- bodies ----------------------------------------------------------------------------------

// readJSONBody decodes a JSON body of at most oauthBodyMax bytes into dst. The error names no part
// of the input.
func readJSONBody(w http.ResponseWriter, r *http.Request, dst any, code string) *oauth.Error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyMax))
	if err != nil {
		return oauth.NewError(code, "The request body could not be read or is too large.")
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return oauth.NewError(code, "The request body is not a JSON object of the expected shape.")
	}
	return nil
}

// mediaType is the media type of a Content-Type header, lower-cased and without parameters; "" for
// a header that does not parse.
func mediaType(r *http.Request) string {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return mt
}

// readFormBody parses an application/x-www-form-urlencoded body of at most oauthBodyMax bytes.
// Only the body is read: parameters in the URL are ignored, because a URL is logged and a token
// endpoint must not accept secrets there. A parameter that is named in single, and sent more than
// once, is refused (RFC 6749 section 3.2).
func readFormBody(w http.ResponseWriter, r *http.Request, single []string) (url.Values, *oauth.Error) {
	if mediaType(r) != "application/x-www-form-urlencoded" {
		return nil, oauth.NewError(oauth.CodeInvalidRequest, "The request must be application/x-www-form-urlencoded.")
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyMax))
	if err != nil {
		return nil, oauth.NewError(oauth.CodeInvalidRequest, "The request body could not be read or is too large.")
	}
	v, err := url.ParseQuery(string(b))
	if err != nil {
		return nil, oauth.NewError(oauth.CodeInvalidRequest, "The request body is not valid form data.")
	}
	for _, k := range single {
		if len(v[k]) > 1 {
			return nil, oauth.Errorf(oauth.CodeInvalidRequest, "The parameter %s was sent more than once.", k)
		}
	}
	return v, nil
}

// ---- client authentication -------------------------------------------------------------------

// basicCredentials reads an Authorization: Basic header. present says that there was one. The id
// and secret are form-urlencoded inside the base64 (RFC 6749 section 2.3.1) and are decoded here.
// A header that cannot be read is invalid_client.
func basicCredentials(r *http.Request) (id, secret string, present bool, err *oauth.Error) {
	scheme, cred, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", "", false, nil
	}
	bad := oauth.NewError(oauth.CodeInvalidClient, "The Authorization header does not hold client credentials.")
	raw, e := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if e != nil {
		return "", "", true, bad
	}
	rawID, rawSecret, found := strings.Cut(string(raw), ":")
	if !found {
		return "", "", true, bad
	}
	id, e1 := url.QueryUnescape(rawID)
	secret, e2 := url.QueryUnescape(rawSecret)
	if e1 != nil || e2 != nil {
		return "", "", true, bad
	}
	return id, secret, true, nil
}

// hasBasic reports whether the request carries an Authorization: Basic header.
func hasBasic(r *http.Request) bool {
	scheme, _, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Basic")
}

// clientCredentials picks the client's id and secret from the Authorization header or the body. A
// request that sends them in both places with different values is invalid_request; one with no
// client_id at all is invalid_client. The secret is checked by the Service, in constant time.
func clientCredentials(r *http.Request, bodyID, bodySecret string) (id, secret string, err *oauth.Error) {
	hid, hsecret, basic, err := basicCredentials(r)
	if err != nil {
		return "", "", err
	}
	id, secret = bodyID, bodySecret
	if basic {
		if (bodyID != "" && bodyID != hid) || (bodySecret != "" && bodySecret != hsecret) {
			return "", "", oauth.NewError(oauth.CodeInvalidRequest, "The client credentials in the Authorization header and in the body differ.")
		}
		id, secret = hid, hsecret
	}
	if id == "" {
		return "", "", oauth.NewError(oauth.CodeInvalidClient, "Client authentication is required.")
	}
	return id, secret, nil
}

// ---- POST /platform/oauth/apps/register ------------------------------------------------------

// oauthRegisterBody is the request of dynamic client registration (RFC 7591; the spec's
// DynamicRegisterOAuthAppBody). Metadata the server does not know is ignored.
type oauthRegisterBody struct {
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// oauthRegisterResponse is the 201 of dynamic registration: the spec's required fields and the RFC 7591
// echo of what was registered. It never carries the logo.
type oauthRegisterResponse struct {
	ID                      string   `json:"id"`
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret"`
	ClientSecretExpiresAt   int64    `json:"client_secret_expires_at"`
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request, lim *oauthLimits) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	if retry, ok := lim.register.allow(s.oauthClient(r), s.now()); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		return errf(http.StatusTooManyRequests, "Too many registrations from this address; try again later")
	}
	var in oauthRegisterBody
	if e := readJSONBody(w, r, &in, oauth.CodeInvalidClientMetadata); e != nil {
		writeOAuthError(w, e, false)
		return nil
	}
	res, err := s.oauth.Register(r.Context(), oauth.RegisterRequest{
		ClientName:              in.ClientName,
		ClientURI:               in.ClientURI,
		LogoURI:                 in.LogoURI,
		RedirectURIs:            in.RedirectURIs,
		Scope:                   in.Scope,
		TokenEndpointAuthMethod: in.TokenEndpointAuthMethod,
		GrantTypes:              in.GrantTypes,
		ResponseTypes:           in.ResponseTypes,
	})
	if err != nil {
		return s.oauthFail(w, r, err, false)
	}
	uris := res.App.RedirectURIs
	if uris == nil {
		uris = []string{}
	}
	noStore(w)
	writeJSON(w, http.StatusCreated, oauthRegisterResponse{
		ID:                      res.App.ID,
		ClientID:                res.App.ID,
		ClientSecret:            res.ClientSecret,
		ClientSecretExpiresAt:   0,
		RedirectURIs:            uris,
		ClientName:              res.App.Name,
		ClientIDIssuedAt:        res.IssuedAt.Unix(),
		GrantTypes:              nonNil(res.GrantTypes),
		ResponseTypes:           nonNil(res.ResponseTypes),
		Scope:                   oauth.JoinScopes(res.App.Scopes),
		TokenEndpointAuthMethod: res.App.TokenEndpointAuthMethod,
	})
	return nil
}

// nonNil returns s, or an empty slice for nil, so that JSON has [] and not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- GET /v1/oauth/authorize -----------------------------------------------------------------

// authorizeParams are the query parameters of an authorization request. A parameter that is sent
// twice is refused: two readers of one URL that pick different values are how a proxy and a server
// disagree.
var authorizeParams = []string{
	"client_id", "response_type", "redirect_uri", "scope", "state", "response_mode",
	"code_challenge", "code_challenge_method", "organization_slug", "target_flow", "resource",
}

// oauthPage is what the error page of the authorization endpoint shows. Everything in it is fixed
// text, except ClientID, which is shown only when it has the shape of a UUID.
type oauthPage struct {
	Title    string
	Detail   string
	Code     string
	ClientID string
}

// oauthErrorCSP lets the page load and run nothing and forbids framing it. The page has no style
// of its own: the color-scheme meta tag gives it a light and a dark rendering.
const oauthErrorCSP = "default-src 'none'; frame-ancestors 'none'"

var oauthErrorTemplate = template.Must(template.New("oauth-error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>{{.Title}}</title></head><body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Detail}}</p>
{{if .Code}}<p><small>Error: <code>{{.Code}}</code>{{if .ClientID}} (client {{.ClientID}}){{end}}</small></p>{{end}}
</main></body></html>
`))

// writeOAuthPage answers a request the browser cannot be sent back to the client for. The page is
// not cacheable, cannot be framed and shows no text of the request.
func writeOAuthPage(w http.ResponseWriter, status int, p oauthPage) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", oauthErrorCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = oauthErrorTemplate.Execute(w, p)
}

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request, lim *oauthLimits) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	if retry, ok := lim.authorize.allow(s.oauthClient(r), s.now()); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		writeOAuthPage(w, http.StatusTooManyRequests, oauthPage{
			Title:  "Too many requests",
			Detail: "This address has started too many sign-in requests. Wait a minute, then go back to the application and try again.",
		})
		return nil
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeOAuthPage(w, http.StatusBadRequest, oauthPage{
			Title:  "Invalid sign-in request",
			Detail: "The request could not be read. Go back to the application and start the sign-in again.",
			Code:   oauth.CodeInvalidRequest,
		})
		return nil
	}
	for _, k := range authorizeParams {
		if len(q[k]) > 1 {
			writeOAuthPage(w, http.StatusBadRequest, oauthPage{
				Title:  "Invalid sign-in request",
				Detail: "The request repeats a parameter. Go back to the application and start the sign-in again.",
				Code:   oauth.CodeInvalidRequest,
			})
			return nil
		}
	}
	res, err := s.oauth.StartAuthorization(r.Context(), oauth.AuthorizeRequest{
		ClientID:            q.Get("client_id"),
		ResponseType:        q.Get("response_type"),
		RedirectURI:         q.Get("redirect_uri"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
		ResponseMode:        q.Get("response_mode"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		OrganizationSlug:    q.Get("organization_slug"),
		TargetFlow:          q.Get("target_flow"),
		Resource:            q.Get("resource"),
	})
	var re *oauth.RedirectError
	switch {
	case err == nil:
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Location", res.ConsentURL)
		w.WriteHeader(http.StatusFound)
	case errors.Is(err, oauth.ErrUnknownClient):
		p := oauthPage{
			Title:  "Unknown application",
			Detail: "This sign-in request names an application that Supavise does not know. Go back to the application and start the sign-in again; it registers itself each time it connects.",
			Code:   oauth.CodeInvalidClient,
		}
		if id := q.Get("client_id"); uuidRe.MatchString(id) {
			p.ClientID = id
		}
		writeOAuthPage(w, http.StatusUnprocessableEntity, p)
	case errors.Is(err, oauth.ErrInvalidRedirectURI):
		writeOAuthPage(w, http.StatusBadRequest, oauthPage{
			Title:  "Invalid redirect address",
			Detail: "The application asked to be sent back to an address it did not register, so nothing was sent there. Ask the application's developer to register the address.",
			Code:   oauth.CodeInvalidRequest,
		})
	case errors.As(err, &re):
		s.oauthRedirectError(w, re)
	case errors.Is(err, oauth.ErrLimit):
		w.Header().Set("Retry-After", "60")
		writeOAuthPage(w, http.StatusTooManyRequests, oauthPage{
			Title:  "Too many requests",
			Detail: "Too many sign-in requests are waiting for a decision. Wait a minute, then go back to the application and try again.",
		})
	default:
		if r.Context().Err() == nil {
			s.log.Error("an OAuth authorization request failed", "err", err)
		}
		writeOAuthPage(w, http.StatusInternalServerError, oauthPage{
			Title:  "Something went wrong",
			Detail: "Supavise could not process the sign-in request. Try again in a moment.",
			Code:   oauth.CodeServerError,
		})
	}
	return nil
}

// oauthRedirectError sends the browser back to the client's redirect URI with the error, the
// client's state and the issuer (RFC 6749 section 4.1.2.1, RFC 9207). The Service returns a
// RedirectError only for a redirect URI that it matched against the app's registered ones; the
// scheme is checked here again, because a redirect is the one place a bug would hand an attacker
// the browser.
func (s *Server) oauthRedirectError(w http.ResponseWriter, re *oauth.RedirectError) {
	e := re.Err
	if e == nil {
		e = oauth.NewError(oauth.CodeServerError, "")
	}
	kv := []string{"error", e.Code}
	if e.Description != "" {
		kv = append(kv, "error_description", e.Description)
	}
	if re.State != "" {
		kv = append(kv, "state", re.State)
	}
	kv = append(kv, "iss", s.cfg.APIURL())
	loc, err := oauth.AppendQuery(re.RedirectURI, kv...)
	if u, perr := url.Parse(re.RedirectURI); err != nil || perr != nil || (u.Scheme != "https" && u.Scheme != "http") {
		s.log.Error("an OAuth authorization error has no usable redirect URI")
		writeOAuthPage(w, http.StatusInternalServerError, oauthPage{
			Title:  "Something went wrong",
			Detail: "Supavise could not send you back to the application. Go back to it and start the sign-in again.",
			Code:   oauth.CodeServerError,
		})
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Location", loc)
	w.WriteHeader(http.StatusFound)
}

// ---- POST /v1/oauth/token --------------------------------------------------------------------

// tokenParams are the parameters of the token request that may appear once.
var tokenParams = []string{
	"grant_type", "client_id", "client_secret", "code", "code_verifier", "redirect_uri",
	"refresh_token", "scope", "resource", "assertion",
}

// oauthTokenBody is the 200 of the token endpoint (RFC 6749 section 5.1) with the granted scope.
type oauthTokenBody struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request, lim *oauthLimits) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	client, now := s.oauthClient(r), s.now()
	if retry, blocked := lim.failures.blocked(client, now); blocked {
		noStore(w)
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		return errf(http.StatusTooManyRequests, "Too many failed requests from this address; try again later")
	}
	basic := hasBasic(r)
	// refuse answers an error the caller caused and counts it against the address.
	refuse := func(e *oauth.Error) error {
		if e.HTTPStatus() < http.StatusInternalServerError {
			lim.failures.fail(client, now)
		}
		writeOAuthError(w, e, basic)
		return nil
	}
	form, e := readFormBody(w, r, tokenParams)
	if e != nil {
		return refuse(e)
	}
	id, secret, e := clientCredentials(r, form.Get("client_id"), form.Get("client_secret"))
	if e != nil {
		return refuse(e)
	}
	if form.Get("grant_type") == "" {
		return refuse(oauth.NewError(oauth.CodeInvalidRequest, "The parameter grant_type is required."))
	}
	res, err := s.oauth.Exchange(r.Context(), oauth.TokenRequest{
		GrantType:    form.Get("grant_type"),
		ClientID:     id,
		ClientSecret: secret,
		Code:         form.Get("code"),
		CodeVerifier: form.Get("code_verifier"),
		RedirectURI:  form.Get("redirect_uri"),
		RefreshToken: form.Get("refresh_token"),
		Scope:        form.Get("scope"),
		Resource:     form.Get("resource"),
		Assertion:    form.Get("assertion"),
	})
	if err != nil {
		var oe *oauth.Error
		if errors.As(err, &oe) {
			return refuse(oe)
		}
		return s.oauthFail(w, r, err, basic)
	}
	noStore(w)
	writeJSON(w, http.StatusOK, oauthTokenBody{
		AccessToken:  res.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    res.ExpiresIn,
		RefreshToken: res.RefreshToken,
		Scope:        res.Scope,
	})
	return nil
}

// ---- POST /v1/oauth/revoke -------------------------------------------------------------------

// oauthRevokeBody is the JSON request of the spec; token and token_type_hint are RFC 7009's names,
// which the form encoding uses.
type oauthRevokeBody struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	RefreshToken  string `json:"refresh_token"`
	Token         string `json:"token"`
	TokenTypeHint string `json:"token_type_hint"`
}

func (s *Server) oauthRevoke(w http.ResponseWriter, r *http.Request, lim *oauthLimits) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	client, now := s.oauthClient(r), s.now()
	if retry, blocked := lim.failures.blocked(client, now); blocked {
		noStore(w)
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		return errf(http.StatusTooManyRequests, "Too many failed requests from this address; try again later")
	}
	basic := hasBasic(r)
	refuse := func(e *oauth.Error) error {
		if e.HTTPStatus() < http.StatusInternalServerError {
			lim.failures.fail(client, now)
		}
		writeOAuthError(w, e, basic)
		return nil
	}
	var in oauthRevokeBody
	switch mediaType(r) {
	case "application/json":
		if e := readJSONBody(w, r, &in, oauth.CodeInvalidRequest); e != nil {
			return refuse(e)
		}
	case "application/x-www-form-urlencoded":
		form, e := readFormBody(w, r, []string{"token", "token_type_hint", "client_id", "client_secret", "refresh_token"})
		if e != nil {
			return refuse(e)
		}
		in = oauthRevokeBody{
			ClientID: form.Get("client_id"), ClientSecret: form.Get("client_secret"),
			RefreshToken: form.Get("refresh_token"), Token: form.Get("token"), TokenTypeHint: form.Get("token_type_hint"),
		}
	default:
		return refuse(oauth.NewError(oauth.CodeInvalidRequest, "The request must be application/json or application/x-www-form-urlencoded."))
	}
	id, secret, e := clientCredentials(r, in.ClientID, in.ClientSecret)
	if e != nil {
		return refuse(e)
	}
	token := in.Token
	if in.RefreshToken != "" {
		if token != "" && token != in.RefreshToken {
			return refuse(oauth.NewError(oauth.CodeInvalidRequest, "The parameters token and refresh_token differ."))
		}
		token = in.RefreshToken
	}
	if token == "" {
		return refuse(oauth.NewError(oauth.CodeInvalidRequest, "The parameter token is required."))
	}
	err := s.oauth.Revoke(r.Context(), oauth.RevokeRequest{
		ClientID: id, ClientSecret: secret, Token: token, TokenTypeHint: in.TokenTypeHint,
	})
	if err != nil {
		var oe *oauth.Error
		if errors.As(err, &oe) {
			return refuse(oe)
		}
		return s.oauthFail(w, r, err, basic)
	}
	noStore(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
