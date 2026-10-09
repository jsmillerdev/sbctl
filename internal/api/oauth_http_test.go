package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
)

// The tests of the OAuth endpoints. The handlers here parse requests, apply the limits and write
// answers; the rules behind them are internal/oauth's and are tested there. So most tests put an
// endpointOAuth in Server.oauth, which answers what the test says and records what it was asked.
// oauth_flow_test.go drives the same endpoints over the real service once it is written.

const (
	epClient = "66666666-6666-4666-8666-666666666666"
	epAuthID = "0b9d1a58-7c2e-4b8a-9d41-2f6f6f0a7c11"
	epAddr   = "198.51.100.7:40001"
	epForm   = "application/x-www-form-urlencoded"
)

// endpointOAuth is an oauth.Authority for the tests of the endpoints. A method whose function the
// test has not set fails the test.
type endpointOAuth struct {
	oauth.Authority
	t *testing.T

	register func(oauth.RegisterRequest) (*oauth.RegisteredApp, error)
	start    func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error)
	describe func(string) (*oauth.AuthorizationView, error)
	approve  func(oauth.ApproveRequest) (*oauth.ApproveResult, error)
	decline  func(oauth.DeclineRequest) error
	exchange func(oauth.TokenRequest) (*oauth.TokenResponse, error)
	revoke   func(oauth.RevokeRequest) error

	calls int
}

var errUnexpectedCall = errors.New("unexpected call of the OAuth service")

func (e *endpointOAuth) Register(_ context.Context, r oauth.RegisterRequest) (*oauth.RegisteredApp, error) {
	e.calls++
	if e.register == nil {
		e.t.Errorf("unexpected call: Register")
		return nil, errUnexpectedCall
	}
	return e.register(r)
}

func (e *endpointOAuth) StartAuthorization(_ context.Context, r oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
	e.calls++
	if e.start == nil {
		e.t.Errorf("unexpected call: StartAuthorization")
		return nil, errUnexpectedCall
	}
	return e.start(r)
}

func (e *endpointOAuth) Describe(_ context.Context, id string) (*oauth.AuthorizationView, error) {
	e.calls++
	if e.describe == nil {
		e.t.Errorf("unexpected call: Describe")
		return nil, errUnexpectedCall
	}
	return e.describe(id)
}

func (e *endpointOAuth) Approve(_ context.Context, r oauth.ApproveRequest) (*oauth.ApproveResult, error) {
	e.calls++
	if e.approve == nil {
		e.t.Errorf("unexpected call: Approve")
		return nil, errUnexpectedCall
	}
	return e.approve(r)
}

func (e *endpointOAuth) Decline(_ context.Context, r oauth.DeclineRequest) error {
	e.calls++
	if e.decline == nil {
		e.t.Errorf("unexpected call: Decline")
		return errUnexpectedCall
	}
	return e.decline(r)
}

func (e *endpointOAuth) Exchange(_ context.Context, r oauth.TokenRequest) (*oauth.TokenResponse, error) {
	e.calls++
	if e.exchange == nil {
		e.t.Errorf("unexpected call: Exchange")
		return nil, errUnexpectedCall
	}
	return e.exchange(r)
}

func (e *endpointOAuth) Revoke(_ context.Context, r oauth.RevokeRequest) error {
	e.calls++
	if e.revoke == nil {
		e.t.Errorf("unexpected call: Revoke")
		return errUnexpectedCall
	}
	return e.revoke(r)
}

// oauthFake puts a new endpointOAuth into the server.
func (f *fixture) oauthFake() *endpointOAuth {
	f.t.Helper()
	t, ok := f.t.(*testing.T)
	if !ok {
		f.t.Fatal("oauthFake needs a *testing.T")
	}
	e := &endpointOAuth{t: t}
	f.srv.oauth = e
	return e
}

// epDo sends a request from a client address with no credentials unless headers carry them. A body
// is sent as it is. A Host header sets the request's host.
func (f *fixture) epDo(remote, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	calledRoutes.Store(method+" "+strings.SplitN(path, "?", 2)[0], true)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if remote != "" {
		req.RemoteAddr = remote
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if strings.EqualFold(headers[i], "Host") {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// epGet is epDo for a GET with no body.
func (f *fixture) epGet(remote, path string, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.epDo(remote, http.MethodGet, path, "", headers...)
}

// epJWT is the header pair that signs a request in as the fixture's user.
func (f *fixture) epJWT() []string { return []string{"Authorization", "Bearer " + f.jwt} }

func hexOf(n int, c byte) string { return strings.Repeat(string(c), n) }

func epApp() *oauth.RegisteredApp {
	return &oauth.RegisteredApp{
		App: oauth.App{ID: epClient, RegistrationType: oauth.RegistrationDynamic, Name: "Claude Code",
			RedirectURIs: []string{"http://127.0.0.1:8123/callback"}, Scopes: oauth.AdvertisedScopes,
			TokenEndpointAuthMethod: oauth.AuthMethodBasic},
		ClientSecret:  oauth.ClientSecretPrefix + hexOf(oauth.ClientSecretHexLen, 'a'),
		IssuedAt:      time.Unix(1790000000, 0),
		GrantTypes:    []string{oauth.GrantTypeAuthorizationCode, oauth.GrantTypeRefreshToken},
		ResponseTypes: []string{oauth.ResponseTypeCode},
	}
}

func jsonMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %q", err, rec.Body.String())
	}
	return m
}

func basicHeader(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
}

// ---- registration ----------------------------------------------------------------------------

func TestRegisterEndpoint(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.RegisterRequest
	fake.register = func(r oauth.RegisterRequest) (*oauth.RegisteredApp, error) {
		got = r
		return epApp(), nil
	}
	body := `{"client_name":"Claude Code","client_uri":"https://claude.ai","logo_uri":"https://claude.ai/logo.png",
		"redirect_uris":["http://127.0.0.1:8123/callback"],"scope":"projects:read","token_endpoint_auth_method":"none",
		"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"software_id":"ignored"}`
	rec := f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", body, "Content-Type", "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := oauth.RegisterRequest{
		ClientName: "Claude Code", ClientURI: "https://claude.ai", LogoURI: "https://claude.ai/logo.png",
		RedirectURIs: []string{"http://127.0.0.1:8123/callback"}, Scope: "projects:read", TokenEndpointAuthMethod: "none",
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the service was asked %+v, want %+v", got, want)
	}
	validateAgainstSpec(t, "POST /platform/oauth/apps/register", rec.Body.Bytes())
	m := jsonMap(t, rec)
	app := epApp()
	for k, v := range map[string]any{
		"id": epClient, "client_id": epClient, "client_secret": app.ClientSecret, "client_secret_expires_at": float64(0),
		"client_name": "Claude Code", "client_id_issued_at": float64(1790000000), "token_endpoint_auth_method": "client_secret_basic",
		"scope": oauth.JoinScopes(oauth.AdvertisedScopes),
	} {
		if !reflect.DeepEqual(m[k], v) {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if !reflect.DeepEqual(m["redirect_uris"], []any{"http://127.0.0.1:8123/callback"}) ||
		!reflect.DeepEqual(m["grant_types"], []any{"authorization_code", "refresh_token"}) ||
		!reflect.DeepEqual(m["response_types"], []any{"code"}) {
		t.Errorf("echoed lists: %v", m)
	}
	if _, ok := m["logo_uri"]; ok || strings.Contains(rec.Body.String(), "logo") {
		t.Errorf("the logo is stored and never returned: %s", rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q: the answer holds a secret", cc)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("registration is open to browser clients")
	}
	// No credential is asked for or read.
	if rec := f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", body, "Authorization", "Bearer not-a-token"); rec.Code != http.StatusCreated {
		t.Errorf("a stray Authorization header changed the answer: %d", rec.Code)
	}
}

func TestRegisterErrors(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	post := func(body string) *httptest.ResponseRecorder {
		return f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", body, "Content-Type", "application/json")
	}
	// A body that is not a JSON object of the right shape never reaches the service.
	for name, body := range map[string]string{
		"empty": "", "text": "client_name=x", "array": `[]`, "wrong type": `{"redirect_uris":"http://localhost/cb"}`,
		"too large": `{"client_name":"` + strings.Repeat("x", oauthBodyMax) + `"}`,
	} {
		rec := post(body)
		m := jsonMap(t, rec)
		if rec.Code != http.StatusBadRequest || m["error"] != oauth.CodeInvalidClientMetadata || m["message"] == "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "redirect_uris") && name == "wrong type" {
			t.Errorf("the error repeats the input: %s", rec.Body)
		}
	}
	if fake.calls != 0 {
		t.Fatalf("the service was called %d times for bodies it should never see", fake.calls)
	}
	// The service's refusals keep their code.
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) {
		return nil, oauth.NewError(oauth.CodeInvalidRedirectURI, "redirect_uris must be https or loopback http")
	}
	rec := post(`{"client_name":"x","redirect_uris":["javascript:alert(1)"]}`)
	m := jsonMap(t, rec)
	if rec.Code != 400 || m["error"] != "invalid_redirect_uri" || m["error_description"] != "redirect_uris must be https or loopback http" || m["message"] != m["error_description"] {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	// Anything else is a server error that says nothing of its cause.
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) {
		return nil, errors.New("pq: connection to 10.0.0.9 refused")
	}
	rec = post(`{"client_name":"x","redirect_uris":["https://a.example/cb"]}`)
	if rec.Code != 500 || jsonMap(t, rec)["error"] != "server_error" || strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// R2: a client address registers 100 times in 10 minutes; the cap on stored apps is a 429 too.
func TestRegisterLimits(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) { return epApp(), nil }
	base := time.Now()
	now := base
	f.srv.now = func() time.Time { return now }
	post := func(addr string) *httptest.ResponseRecorder {
		return f.epDo(addr, http.MethodPost, "/platform/oauth/apps/register", `{"client_name":"c","redirect_uris":["http://localhost/cb"]}`)
	}
	for i := 0; i < 100; i++ {
		if rec := post("198.51.100.1:1"); rec.Code != http.StatusCreated {
			t.Fatalf("registration %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := post("198.51.100.1:2") // the port is not part of the client
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the 101st registration: %d", rec.Code)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n < 1 || n > 600 {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	if fake.calls != 100 {
		t.Errorf("the service was called %d times", fake.calls)
	}
	if rec := post("198.51.100.2:1"); rec.Code != http.StatusCreated {
		t.Errorf("another address is limited with the first: %d", rec.Code)
	}
	// An IPv6 client is limited by its /64.
	for i := 0; i < 100; i++ {
		post(fmt.Sprintf("[2001:db8:1:2::%x]:1", i+1))
	}
	if rec := post("[2001:db8:1:2:ffff::9]:1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a host of the same /64: %d", rec.Code)
	}
	now = base.Add(10*time.Minute + time.Second)
	if rec := post("198.51.100.1:1"); rec.Code != http.StatusCreated {
		t.Errorf("after the window: %d", rec.Code)
	}
	// The cap on stored dynamic apps.
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) {
		return nil, fmt.Errorf("%w: 5000 dynamic apps", oauth.ErrLimit)
	}
	rec = post("198.51.100.3:1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("app cap: %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if strings.Contains(rec.Body.String(), "5000") {
		t.Errorf("the answer repeats the service's text: %s", rec.Body)
	}
}

// ---- authorize -------------------------------------------------------------------------------

func authorizeQuery(kv ...string) string {
	q := url.Values{"client_id": {epClient}, "response_type": {"code"}, "redirect_uri": {"http://127.0.0.1:8123/callback"}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return "/v1/oauth/authorize?" + q.Encode()
}

func TestAuthorizeRedirectsToConsent(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.AuthorizeRequest
	consent := oauth.ConsentURL(f.cfg.DashboardURL(), epAuthID, "default")
	fake.start = func(r oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
		got = r
		return &oauth.AuthorizeResult{AuthID: epAuthID, ConsentURL: consent}, nil
	}
	challenge := strings.Repeat("a", 43)
	rec := f.epGet(epAddr, authorizeQuery(
		"scope", "projects:read database:read", "state", "st 1&x=y", "response_mode", "query", "code_challenge", challenge,
		"code_challenge_method", "S256", "organization_slug", "default", "target_flow", "t", "resource", "https://api.example.test/mcp"))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Location") != consent {
		t.Errorf("Location %q, want %q", rec.Header().Get("Location"), consent)
	}
	want := oauth.AuthorizeRequest{ClientID: epClient, ResponseType: "code", RedirectURI: "http://127.0.0.1:8123/callback",
		Scope: "projects:read database:read", State: "st 1&x=y", ResponseMode: "query", CodeChallenge: challenge,
		CodeChallengeMethod: "S256", OrganizationSlug: "default", TargetFlow: "t", Resource: "https://api.example.test/mcp"}
	if got != want {
		t.Errorf("the service was asked %+v, want %+v", got, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	if rp := rec.Header().Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy %q", rp)
	}
	// Without the optional parameters, only the three required ones are passed.
	if rec := f.epGet(epAddr, authorizeQuery()); rec.Code != http.StatusFound {
		t.Errorf("minimal request: %d", rec.Code)
	}
	if want := (oauth.AuthorizeRequest{ClientID: epClient, ResponseType: "code", RedirectURI: "http://127.0.0.1:8123/callback"}); got != want {
		t.Errorf("the service was asked %+v, want %+v", got, want)
	}
}

// C6 and C7: no redirect before the client and the redirect URI are known; pages that escape and
// frame-proof; an error that redirects carries error, state and iss and keeps the URI's own query.
func TestAuthorizeErrors(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	const hostile = `"><script>alert(1)</script>`
	isPage := func(t *testing.T, rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("a page that must not redirect sent the browser to %q", loc)
		}
		h := rec.Header()
		if !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
			t.Errorf("Content-Type %q", h.Get("Content-Type"))
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("Content-Security-Policy %q", csp)
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("headers: %v", h)
		}
		if strings.Contains(rec.Body.String(), "<script") || strings.Contains(rec.Body.String(), "alert(1)") {
			t.Errorf("the page repeats hostile input: %s", rec.Body)
		}
	}

	t.Run("unknown client", func(t *testing.T) {
		fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) { return nil, oauth.ErrUnknownClient }
		rec := f.epGet(epAddr, authorizeQuery("client_id", hostile, "state", hostile))
		isPage(t, rec, http.StatusUnprocessableEntity)
		if strings.Contains(rec.Body.String(), "client ") {
			t.Errorf("a client id that is no UUID is shown: %s", rec.Body)
		}
		// A client id of the shape of a UUID is shown, for the person who has to report it.
		rec = f.epGet(epAddr, authorizeQuery("client_id", epClient))
		isPage(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), epClient) {
			t.Errorf("the client id is not shown: %s", rec.Body)
		}
	})
	t.Run("redirect URI not registered", func(t *testing.T) {
		fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) { return nil, oauth.ErrInvalidRedirectURI }
		isPage(t, f.epGet(epAddr, authorizeQuery("redirect_uri", "javascript:"+hostile, "state", hostile)), http.StatusBadRequest)
	})
	t.Run("error back to the client", func(t *testing.T) {
		fake.start = func(r oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
			return nil, &oauth.RedirectError{RedirectURI: r.RedirectURI, State: r.State,
				Err: oauth.NewError(oauth.CodeInvalidRequest, "code_challenge_method must be S256")}
		}
		rec := f.epGet(epAddr, authorizeQuery("redirect_uri", "https://app.example/cb?keep=1", "state", "a b&c=d/é"))
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		u, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Scheme != "https" || u.Host != "app.example" || u.Path != "/cb" {
			t.Errorf("Location %s", u)
		}
		q := u.Query()
		for k, want := range map[string]string{"keep": "1", "error": "invalid_request", "error_description": "code_challenge_method must be S256",
			"state": "a b&c=d/é", "iss": "https://api.example.test"} {
			if got := q.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		if h := rec.Header(); h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("headers %v", h)
		}
		// A request with no state sends none back.
		rec = f.epGet(epAddr, authorizeQuery())
		u, _ = url.Parse(rec.Header().Get("Location"))
		if _, ok := u.Query()["state"]; ok || u.Query().Get("iss") == "" {
			t.Errorf("Location %s", u)
		}
		// A loopback redirect keeps its port.
		if !strings.HasPrefix(rec.Header().Get("Location"), "http://127.0.0.1:8123/callback?error=") {
			t.Errorf("Location %s", rec.Header().Get("Location"))
		}
	})
	t.Run("a redirect that is no web address is not followed", func(t *testing.T) {
		for _, uri := range []string{"javascript:alert(1)", "cursor://callback", "data:text/html,x", ""} {
			fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
				return nil, &oauth.RedirectError{RedirectURI: uri, Err: oauth.NewError(oauth.CodeInvalidScope, "")}
			}
			isPage(t, f.epGet(epAddr, authorizeQuery()), http.StatusInternalServerError)
		}
	})
	t.Run("a cap", func(t *testing.T) {
		fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
			return nil, fmt.Errorf("%w: 25 pending requests", oauth.ErrLimit)
		}
		rec := f.epGet(epAddr, authorizeQuery())
		isPage(t, rec, http.StatusTooManyRequests)
		if rec.Header().Get("Retry-After") == "" || strings.Contains(rec.Body.String(), "25") {
			t.Errorf("Retry-After %q body %s", rec.Header().Get("Retry-After"), rec.Body)
		}
	})
	t.Run("an internal error says nothing of its cause", func(t *testing.T) {
		fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
			return nil, errors.New("pq: relation oauth_apps does not exist")
		}
		rec := f.epGet(epAddr, authorizeQuery())
		isPage(t, rec, http.StatusInternalServerError)
		if strings.Contains(rec.Body.String(), "oauth_apps") {
			t.Errorf("the page shows the cause: %s", rec.Body)
		}
	})
	t.Run("a repeated or unreadable parameter never reaches the service", func(t *testing.T) {
		before := fake.calls
		for _, q := range []string{
			"/v1/oauth/authorize?client_id=" + epClient + "&client_id=" + epClient + "&response_type=code&redirect_uri=http://127.0.0.1/cb",
			authorizeQuery() + "&redirect_uri=https%3A%2F%2Fevil.example%2Fcb",
			authorizeQuery() + "&state=a&state=b",
			"/v1/oauth/authorize?client_id=%zz",
			"/v1/oauth/authorize?client_id=a;b=c",
		} {
			isPage(t, f.epGet(epAddr, q), http.StatusBadRequest)
		}
		if fake.calls != before {
			t.Errorf("the service was called for requests with repeated parameters")
		}
	})
}

// C1 and C8 at the endpoint: the parameters a PKCE or resource refusal is about reach the service
// untouched, and its refusal goes back to the client as a redirect. The rules themselves are
// proven over the real service in oauth_flow_test.go.
func TestAuthorizeForwardsPKCEAndResource(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.AuthorizeRequest
	fake.start = func(r oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
		got = r
		code := oauth.CodeInvalidRequest
		if r.Resource != "" {
			code = oauth.CodeInvalidTarget
		}
		return nil, &oauth.RedirectError{RedirectURI: r.RedirectURI, State: r.State, Err: oauth.NewError(code, "refused")}
	}
	for _, method := range []string{"plain", "sha256", "s256", ""} {
		rec := f.epGet(epAddr, authorizeQuery("state", "s1", "code_challenge", strings.Repeat("b", 43), "code_challenge_method", method))
		if got.CodeChallengeMethod != method || got.CodeChallenge != strings.Repeat("b", 43) {
			t.Errorf("method %q was changed to %+v", method, got)
		}
		u, _ := url.Parse(rec.Header().Get("Location"))
		if rec.Code != 302 || u.Query().Get("error") != "invalid_request" || u.Query().Get("state") != "s1" || u.Query().Get("iss") == "" {
			t.Errorf("method %q: %d %s", method, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, res := range []string{"https://evil.example/mcp", "https://api.example.test/mcp#frag", "not a url"} {
		rec := f.epGet(epAddr, authorizeQuery("resource", res))
		u, _ := url.Parse(rec.Header().Get("Location"))
		if got.Resource != res || u.Query().Get("error") != "invalid_target" {
			t.Errorf("resource %q: asked %q, answered %s", res, got.Resource, rec.Header().Get("Location"))
		}
	}
}

// 60 requests a minute per client address, and the page that says so.
func TestAuthorizeLimit(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
		return &oauth.AuthorizeResult{AuthID: epAuthID, ConsentURL: "https://studio.example.test/authorize?auth_id=" + epAuthID}, nil
	}
	base := time.Now()
	now := base
	f.srv.now = func() time.Time { return now }
	for i := 0; i < 60; i++ {
		if rec := f.epGet(epAddr, authorizeQuery()); rec.Code != http.StatusFound {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := f.epGet(epAddr, authorizeQuery())
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("the 61st request: %d %v", rec.Code, rec.Header())
	}
	if rec := f.epGet("198.51.100.99:1", authorizeQuery()); rec.Code != http.StatusFound {
		t.Errorf("another address: %d", rec.Code)
	}
	now = base.Add(time.Minute + time.Second)
	if rec := f.epGet(epAddr, authorizeQuery()); rec.Code != http.StatusFound {
		t.Errorf("after the window: %d", rec.Code)
	}
}

// ---- token -----------------------------------------------------------------------------------

func tokenOK(oauth.TokenRequest) (*oauth.TokenResponse, error) {
	return &oauth.TokenResponse{
		AccessToken: oauth.AccessTokenPrefix + hexOf(oauth.AccessTokenHexLen, 'b'), TokenType: "Bearer", ExpiresIn: 3600,
		RefreshToken: oauth.RefreshTokenPrefix + hexOf(oauth.RefreshTokenHexLen, 'c'), Scope: "projects:read database:read",
	}, nil
}

func tokenForm(kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Add(kv[i], kv[i+1])
	}
	return q.Encode()
}

func (f *fixture) postToken(body string, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.epDo(epAddr, http.MethodPost, "/v1/oauth/token", body, append([]string{"Content-Type", epForm}, headers...)...)
}

// T1: where the client's credentials may come from, and what the service is given.
func TestTokenClientAuth(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.TokenRequest
	fake.exchange = func(r oauth.TokenRequest) (*oauth.TokenResponse, error) {
		got = r
		return tokenOK(r)
	}
	const secret = "sba_" + "0123456789abcdef"
	codeForm := func(kv ...string) string {
		return tokenForm(append([]string{"grant_type", "authorization_code", "code", "sbc_x", "code_verifier", strings.Repeat("v", 43), "redirect_uri", "http://127.0.0.1:8123/callback"}, kv...)...)
	}
	cases := []struct {
		name     string
		body     string
		auth     string
		query    string
		wantCode int
		wantErr  string
		id, sec  string
		basic    bool
		called   bool
	}{
		{name: "Basic", body: codeForm(), auth: basicHeader(epClient, secret), wantCode: 200, id: epClient, sec: secret, called: true},
		{name: "Basic decodes form encoding", body: codeForm(), auth: basicHeader("a/b c", "s p+/="), wantCode: 200, id: "a/b c", sec: "s p+/=", called: true},
		{name: "body", body: codeForm("client_id", epClient, "client_secret", secret), wantCode: 200, id: epClient, sec: secret, called: true},
		{name: "public client sends only its id", body: codeForm("client_id", epClient), wantCode: 200, id: epClient, called: true},
		{name: "same in both", body: codeForm("client_id", epClient, "client_secret", secret), auth: basicHeader(epClient, secret), wantCode: 200, id: epClient, sec: secret, called: true},
		{name: "id differs", body: codeForm("client_id", "other"), auth: basicHeader(epClient, secret), wantCode: 400, wantErr: "invalid_request"},
		{name: "secret differs", body: codeForm("client_secret", "other"), auth: basicHeader(epClient, secret), wantCode: 400, wantErr: "invalid_request"},
		{name: "Basic that is not base64", body: codeForm(), auth: "Basic !!!", wantCode: 401, wantErr: "invalid_client", basic: true},
		{name: "Basic with no colon", body: codeForm(), auth: "Basic " + base64.StdEncoding.EncodeToString([]byte("justanid")), wantCode: 401, wantErr: "invalid_client", basic: true},
		{name: "no credentials", body: codeForm(), wantCode: 401, wantErr: "invalid_client"},
		{name: "credentials in the URL are not read", body: codeForm(), query: "?client_id=" + epClient + "&client_secret=" + secret, wantCode: 401, wantErr: "invalid_client"},
		{name: "no grant_type", body: tokenForm("client_id", epClient), wantCode: 400, wantErr: "invalid_request"},
		{name: "repeated parameter", body: codeForm("client_id", epClient, "code", "sbc_y"), wantCode: 400, wantErr: "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got = oauth.TokenRequest{}
			before := fake.calls
			var h []string
			if c.auth != "" {
				h = []string{"Authorization", c.auth}
			}
			rec := f.epDo(epAddr, http.MethodPost, "/v1/oauth/token"+c.query, c.body, append([]string{"Content-Type", epForm}, h...)...)
			if rec.Code != c.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantErr != "" && jsonMap(t, rec)["error"] != c.wantErr {
				t.Errorf("error %v, want %s", jsonMap(t, rec)["error"], c.wantErr)
			}
			if (fake.calls != before) != c.called {
				t.Errorf("service called = %v, want %v", fake.calls != before, c.called)
			}
			if c.called && (got.ClientID != c.id || got.ClientSecret != c.sec) {
				t.Errorf("the service got id %q secret %q, want %q %q", got.ClientID, got.ClientSecret, c.id, c.sec)
			}
			ch := rec.Header().Get("WWW-Authenticate")
			if wantChallenge := c.basic || (c.wantCode == 401 && c.auth != ""); wantChallenge != strings.HasPrefix(ch, "Basic") {
				t.Errorf("WWW-Authenticate %q", ch)
			}
		})
	}
	t.Run("other content types are refused", func(t *testing.T) {
		for _, ct := range []string{"application/json", "text/plain", ""} {
			rec := f.epDo(epAddr, http.MethodPost, "/v1/oauth/token", `{"grant_type":"authorization_code"}`, "Content-Type", ct)
			if rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_request" {
				t.Errorf("%q: %d %s", ct, rec.Code, rec.Body)
			}
		}
		rec := f.epDo(epAddr, http.MethodPost, "/v1/oauth/token", tokenForm("grant_type", "x"), "Content-Type", epForm+"; charset=UTF-8")
		if rec.Code == 400 && jsonMap(t, rec)["error"] == "invalid_request" && strings.Contains(rec.Body.String(), "x-www-form-urlencoded") {
			t.Errorf("a charset parameter is refused: %s", rec.Body)
		}
	})
	t.Run("a large body is refused", func(t *testing.T) {
		rec := f.postToken(tokenForm("grant_type", "authorization_code", "code", strings.Repeat("x", oauthBodyMax)))
		if rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_request" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("every parameter of the form reaches the service", func(t *testing.T) {
		rec := f.postToken(tokenForm("grant_type", "refresh_token", "client_id", epClient, "client_secret", secret, "code", "c", "code_verifier", "v",
			"redirect_uri", "r", "refresh_token", "sbr_x", "scope", "projects:read", "resource", "https://api.example.test/mcp", "assertion", "a", "unknown", "ignored"))
		want := oauth.TokenRequest{GrantType: "refresh_token", ClientID: epClient, ClientSecret: secret, Code: "c", CodeVerifier: "v", RedirectURI: "r",
			RefreshToken: "sbr_x", Scope: "projects:read", Resource: "https://api.example.test/mcp", Assertion: "a"}
		if rec.Code != 200 || got != want {
			t.Errorf("%d: the service got %+v, want %+v", rec.Code, got, want)
		}
	})
}

// T3: the success answer and every error of the table in design 2.7.
func TestTokenResponses(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.exchange = tokenOK
	body := tokenForm("grant_type", "authorization_code", "client_id", epClient, "code", "c")
	rec := f.postToken(body)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/oauth/token", rec.Body.Bytes())
	m := jsonMap(t, rec)
	want := map[string]any{
		"access_token": oauth.AccessTokenPrefix + hexOf(40, 'b'), "token_type": "Bearer", "expires_in": float64(3600),
		"refresh_token": oauth.RefreshTokenPrefix + hexOf(64, 'c'), "scope": "projects:read database:read",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("body %v, want %v", m, want)
	}
	if h := rec.Header(); h.Get("Cache-Control") != "no-store" || h.Get("Pragma") != "no-cache" || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		t.Errorf("headers %v", h)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("CORS headers %v", rec.Header())
	}

	for code, status := range map[string]int{
		oauth.CodeInvalidRequest: 400, oauth.CodeInvalidClient: 401, oauth.CodeInvalidGrant: 400, oauth.CodeUnsupportedGrantType: 400,
		oauth.CodeInvalidScope: 400, oauth.CodeInvalidTarget: 400,
	} {
		fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) { return nil, oauth.NewError(code, "because") }
		for _, basic := range []bool{false, true} {
			var h []string
			if basic {
				h = []string{"Authorization", basicHeader(epClient, "sba_x")}
			}
			rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient), h...)
			m := jsonMap(t, rec)
			if rec.Code != status || m["error"] != code || m["error_description"] != "because" || m["message"] != "because" {
				t.Errorf("%s: %d %s", code, rec.Code, rec.Body)
			}
			if h := rec.Header(); h.Get("Cache-Control") != "no-store" || h.Get("Pragma") != "no-cache" {
				t.Errorf("%s: headers %v", code, h)
			}
			if ch := rec.Header().Get("WWW-Authenticate"); (ch != "") != (basic && status == 401) {
				t.Errorf("%s basic=%v: WWW-Authenticate %q", code, basic, ch)
			}
		}
	}
	// An error with no description still has a message.
	fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) {
		return nil, oauth.NewError(oauth.CodeInvalidGrant, "")
	}
	if m := jsonMap(t, f.postToken(tokenForm("grant_type", "x", "client_id", epClient))); m["message"] != "invalid_grant" || m["error"] != "invalid_grant" {
		t.Errorf("%v", m)
	}
	// An internal error is a server_error that says nothing of its cause, and no failure of the client.
	fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) {
		return nil, errors.New("pq: deadlock at 10.0.0.9")
	}
	rec = f.postToken(tokenForm("grant_type", "x", "client_id", epClient))
	if m := jsonMap(t, rec); rec.Code != 500 || m["error"] != "server_error" || strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	// A refresh response with no refresh token (the service decides) leaves the field out.
	fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) {
		return &oauth.TokenResponse{AccessToken: "sbp_oauth_x", ExpiresIn: 3600}, nil
	}
	m = jsonMap(t, f.postToken(tokenForm("grant_type", "x", "client_id", epClient)))
	if _, ok := m["refresh_token"]; ok || m["token_type"] != "Bearer" {
		t.Errorf("%v", m)
	}
}

// T5: 30 failed requests a minute per client address, counted together for token and revoke.
func TestTokenFailureLimit(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) {
		return nil, oauth.NewError(oauth.CodeInvalidGrant, "no")
	}
	fake.revoke = func(oauth.RevokeRequest) error { return oauth.NewError(oauth.CodeInvalidClient, "no") }
	base := time.Now()
	now := base
	f.srv.now = func() time.Time { return now }
	for i := 0; i < 20; i++ {
		if rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient)); rec.Code != 400 {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	revoke := func(addr string) *httptest.ResponseRecorder {
		return f.epDo(addr, http.MethodPost, "/v1/oauth/revoke", `{"client_id":"`+epClient+`","client_secret":"x","refresh_token":"sbr_x"}`, "Content-Type", "application/json")
	}
	for i := 0; i < 10; i++ {
		if rec := revoke(epAddr); rec.Code != 401 {
			t.Fatalf("revoke failure %d: %d", i+1, rec.Code)
		}
	}
	calls := fake.calls
	rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 30 failures: %d %s", rec.Code, rec.Body)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	if m := jsonMap(t, rec); m["message"] == "" {
		t.Errorf("no message: %s", rec.Body)
	}
	if rec := revoke(epAddr); rec.Code != http.StatusTooManyRequests {
		t.Errorf("revoke after 30 failures: %d", rec.Code)
	}
	if fake.calls != calls {
		t.Errorf("the service was called while the address was blocked")
	}
	// A request that would succeed is held back as well: the address is blocked, not the request.
	fake.exchange = tokenOK
	if rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient)); rec.Code != 429 {
		t.Errorf("a valid request from a blocked address: %d", rec.Code)
	}
	// Other addresses are not affected, and the window ends.
	if rec := f.epDo("198.51.100.50:1", http.MethodPost, "/v1/oauth/token", tokenForm("grant_type", "authorization_code", "client_id", epClient), "Content-Type", epForm); rec.Code != 200 {
		t.Errorf("another address: %d", rec.Code)
	}
	now = base.Add(time.Minute + time.Second)
	if rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient)); rec.Code != 200 {
		t.Errorf("after the window: %d", rec.Code)
	}
	// Successes and server errors are not failures.
	for i := 0; i < 40; i++ {
		if rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient)); rec.Code != 200 {
			t.Fatalf("success %d: %d", i+1, rec.Code)
		}
	}
	fake.exchange = func(oauth.TokenRequest) (*oauth.TokenResponse, error) { return nil, errors.New("boom") }
	for i := 0; i < 40; i++ {
		if rec := f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient)); rec.Code != 500 {
			t.Fatalf("server error %d: %d", i+1, rec.Code)
		}
	}
}

// ---- revoke ----------------------------------------------------------------------------------

// T6 at the endpoint: both encodings, Basic, 204 for what the service accepts, 401 for bad client
// credentials. That a token of another app is ignored is the service's rule (oauth_flow_test.go).
func TestRevoke(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.RevokeRequest
	var fail error
	fake.revoke = func(r oauth.RevokeRequest) error { got = r; return fail }
	const secret = "sba_aaaa"
	do := func(ct, body string, headers ...string) *httptest.ResponseRecorder {
		return f.epDo(epAddr, http.MethodPost, "/v1/oauth/revoke", body, append([]string{"Content-Type", ct}, headers...)...)
	}
	rec := do("application/json", `{"client_id":"`+epClient+`","client_secret":"`+secret+`","refresh_token":"sbr_x"}`)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("JSON form: %d %s", rec.Code, rec.Body)
	}
	if want := (oauth.RevokeRequest{ClientID: epClient, ClientSecret: secret, Token: "sbr_x"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers %v", rec.Header())
	}
	rec = do(epForm, tokenForm("token", "sbp_oauth_x", "token_type_hint", "access_token"), "Authorization", basicHeader(epClient, secret))
	if rec.Code != 204 {
		t.Fatalf("RFC 7009 form: %d %s", rec.Code, rec.Body)
	}
	if want := (oauth.RevokeRequest{ClientID: epClient, ClientSecret: secret, Token: "sbp_oauth_x", TokenTypeHint: "access_token"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if rec := do(epForm, tokenForm("client_id", epClient, "client_secret", secret, "token", "sbr_y")); rec.Code != 204 || got.Token != "sbr_y" || got.ClientSecret != secret {
		t.Errorf("credentials in the form: %d %+v", rec.Code, got)
	}

	for name, c := range map[string]struct {
		ct, body string
		h        []string
		code     int
		err      string
	}{
		"no token":        {"application/json", `{"client_id":"` + epClient + `","client_secret":"x"}`, nil, 400, "invalid_request"},
		"no client":       {"application/json", `{"refresh_token":"sbr_x"}`, nil, 401, "invalid_client"},
		"two tokens":      {"application/json", `{"client_id":"` + epClient + `","token":"a","refresh_token":"b"}`, nil, 400, "invalid_request"},
		"conflicting":     {epForm, tokenForm("client_id", "other", "token", "t"), []string{"Authorization", basicHeader(epClient, secret)}, 400, "invalid_request"},
		"bad content":     {"text/plain", `token=x`, nil, 400, "invalid_request"},
		"JSON not object": {"application/json", `"sbr_x"`, nil, 400, "invalid_request"},
		"repeated":        {epForm, tokenForm("client_id", epClient, "token", "a", "token", "b"), nil, 400, "invalid_request"},
	} {
		before := fake.calls
		rec := do(c.ct, c.body, c.h...)
		if rec.Code != c.code || jsonMap(t, rec)["error"] != c.err || fake.calls != before {
			t.Errorf("%s: %d %s (calls %d)", name, rec.Code, rec.Body, fake.calls-before)
		}
	}
	fail = oauth.NewError(oauth.CodeInvalidClient, "Client authentication failed.")
	rec = do("application/json", `{"client_id":"`+epClient+`","client_secret":"wrong","refresh_token":"sbr_x"}`)
	if rec.Code != 401 || jsonMap(t, rec)["error"] != "invalid_client" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("bad credentials in the body: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec = do(epForm, tokenForm("token", "sbr_x"), "Authorization", basicHeader(epClient, "wrong"))
	if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("bad credentials in the header: %d %v", rec.Code, rec.Header())
	}
	fail = errors.New("registry down")
	if rec := do("application/json", `{"client_id":"`+epClient+`","client_secret":"x","refresh_token":"sbr_x"}`); rec.Code != 500 || jsonMap(t, rec)["error"] != "server_error" {
		t.Errorf("internal error: %d %s", rec.Code, rec.Body)
	}
}

// ---- logging ---------------------------------------------------------------------------------

// T4: what a request carries is never written to the log, on success or on failure, even at debug
// level (the level that logs every request).
func TestOAuthLogsHaveNoSecrets(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fake := f.oauthFake()
	secrets := []string{
		"STATE-SECRET-1", "sbc_" + hexOf(64, '1'), "VERIFIER-" + hexOf(35, 'v'), "sba_" + hexOf(64, '2'),
		"sbr_" + hexOf(64, '3'), "sbp_oauth_" + hexOf(40, '4'), "CHALLENGE-" + hexOf(33, 'x'),
	}
	boom := errors.New("backend failed")
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) { return epApp(), nil }
	fake.start = func(r oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) {
		if r.State == secrets[0]+"-fail" {
			return nil, boom
		}
		return &oauth.AuthorizeResult{AuthID: epAuthID, ConsentURL: "https://studio.example.test/authorize?auth_id=" + epAuthID}, nil
	}
	fake.approve = func(oauth.ApproveRequest) (*oauth.ApproveResult, error) {
		return &oauth.ApproveResult{RedirectURL: "http://127.0.0.1:8123/cb?code=" + secrets[1] + "&state=" + secrets[0]}, nil
	}
	fake.exchange = func(r oauth.TokenRequest) (*oauth.TokenResponse, error) {
		if r.Code == "fail" {
			return nil, boom
		}
		if r.Code == "bad" {
			return nil, oauth.NewError(oauth.CodeInvalidGrant, "The code is not valid.")
		}
		return &oauth.TokenResponse{AccessToken: secrets[5], TokenType: "Bearer", ExpiresIn: 3600, RefreshToken: secrets[4]}, nil
	}
	fake.revoke = func(r oauth.RevokeRequest) error {
		if r.Token == "fail" {
			return boom
		}
		return nil
	}
	fake.describe = func(string) (*oauth.AuthorizationView, error) { return nil, boom }
	fake.decline = func(oauth.DeclineRequest) error { return boom }

	f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", `{"client_name":"c","redirect_uris":["http://localhost/cb"]}`)
	f.epGet(epAddr, authorizeQuery("state", secrets[0], "code_challenge", secrets[6]))
	f.epGet(epAddr, authorizeQuery("state", secrets[0]+"-fail", "code_challenge", secrets[6]))
	f.epDo(epAddr, http.MethodPost, "/platform/organizations/default/oauth/authorizations/"+epAuthID+"?skip_browser_redirect=true", "", f.epJWT()...)
	for _, code := range []string{"ok", "bad", "fail"} {
		f.postToken(tokenForm("grant_type", "authorization_code", "code", code+secrets[1], "code_verifier", secrets[2]), "Authorization", basicHeader(epClient, secrets[3]))
		f.postToken(tokenForm("grant_type", "authorization_code", "client_id", epClient, "client_secret", secrets[3], "code", code, "code_verifier", secrets[2], "refresh_token", secrets[4]))
	}
	f.epDo(epAddr, http.MethodPost, "/v1/oauth/token?code="+secrets[1]+"&client_secret="+secrets[3], tokenForm("grant_type", "refresh_token", "refresh_token", secrets[4]), "Content-Type", epForm)
	for _, tok := range []string{"ok", "fail"} {
		f.epDo(epAddr, http.MethodPost, "/v1/oauth/revoke", `{"client_id":"`+epClient+`","client_secret":"`+secrets[3]+`","refresh_token":"`+secrets[4]+tok+`"}`, "Content-Type", "application/json")
	}
	f.epDo(epAddr, http.MethodGet, "/platform/oauth/authorizations/"+epAuthID, "", f.epJWT()...)
	f.epDo(epAddr, http.MethodDelete, "/platform/organizations/default/oauth/authorizations/"+epAuthID, "", f.epJWT()...)

	out := logs.String()
	if !strings.Contains(out, "backend failed") {
		t.Fatalf("the test never reached the failure paths; log:\n%s", out)
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the log holds %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "code=") || strings.Contains(out, "client_secret=") {
		t.Errorf("the log holds a query string:\n%s", out)
	}
}

// ---- route table, switch and CORS ------------------------------------------------------------

// O6: the routes that take no credential are the device-login poll and the four OAuth endpoints a
// client calls before it has a token.
func TestOnlyExpectedRoutesAreAuthNone(t *testing.T) {
	want := map[string]bool{
		"GET /platform/cli/login/{session_id}": true,
		"POST /platform/oauth/apps/register":   true,
		"GET /v1/oauth/authorize":              true,
		"POST /v1/oauth/token":                 true,
		"POST /v1/oauth/revoke":                true,
	}
	f := newFixture(t)
	impl := f.srv.implemented()
	for key, r := range impl {
		if r.auth == authNone && !want[key] {
			t.Errorf("%s takes no credential", key)
		}
	}
	for key := range want {
		if r, ok := impl[key]; !ok || r.auth != authNone {
			t.Errorf("%s is not an implemented route without credentials", key)
		}
	}
	for _, key := range oauthPublicRoutes {
		if !want[key] {
			t.Errorf("oauthPublicRoutes names %s", key)
		}
	}
	// The consent routes need a dashboard session.
	for _, key := range []string{"GET /platform/oauth/authorizations/{id}", "POST /platform/organizations/{slug}/oauth/authorizations/{id}", "DELETE /platform/organizations/{slug}/oauth/authorizations/{id}"} {
		if r, ok := impl[key]; !ok || r.auth != authJWT {
			t.Errorf("%s: %+v, want a route that needs a session", key, r.auth)
		}
	}
}

// O1 for the authorization server's own routes: with [api] disable_oauth they answer 404 and reach
// nobody. The stub of GET /v1/oauth/authorize/project-claim is not one of them.
func TestDisableOAuthServerRoutes(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake() // any call fails the test
	f.cfg.API.DisableOAuth = true
	consent := "/platform/organizations/default/oauth/authorizations/" + epAuthID
	for _, c := range []struct {
		method, path, body, ct string
		jwt                    bool
	}{
		{http.MethodPost, "/platform/oauth/apps/register", `{"client_name":"c","redirect_uris":["http://localhost/cb"]}`, "application/json", false},
		{http.MethodGet, authorizeQuery(), "", "", false},
		{http.MethodPost, "/v1/oauth/token", tokenForm("grant_type", "authorization_code"), epForm, false},
		{http.MethodPost, "/v1/oauth/revoke", `{"client_id":"` + epClient + `","client_secret":"x","refresh_token":"y"}`, "application/json", false},
		{http.MethodGet, "/platform/oauth/authorizations/" + epAuthID, "", "", true},
		{http.MethodPost, consent, "", "", true},
		{http.MethodDelete, consent, "", "", true},
	} {
		h := []string{"Content-Type", c.ct}
		if c.jwt {
			h = append(h, f.epJWT()...)
		}
		rec := f.epDo(epAddr, c.method, c.path, c.body, h...)
		if rec.Code != http.StatusNotFound || jsonMap(t, rec)["message"] != "Not Found" {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s %s: CORS headers on a disabled endpoint", c.method, c.path)
		}
	}
	if fake.calls != 0 {
		t.Errorf("the service was called %d times", fake.calls)
	}
	rec := f.epDo(epAddr, http.MethodGet, "/v1/oauth/authorize/project-claim", "", f.epJWT()...)
	if rec.Header().Get("X-Supavise-Stub") == "" {
		t.Errorf("the project-claim stub changed: %d %v", rec.Code, rec.Header())
	}
	// Turning the switch off again brings the routes back, without a new server.
	f.cfg.API.DisableOAuth = false
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) { return epApp(), nil }
	if rec := f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", `{"client_name":"c","redirect_uris":["http://localhost/cb"]}`); rec.Code != 201 {
		t.Errorf("after the switch: %d", rec.Code)
	}
}

// O4: the open policy covers the listed paths and only them, carries no credentials, and leaves the
// dashboard's policy alone everywhere else.
func TestOAuthCORS(t *testing.T) {
	f := newFixture(t)
	// The requests below that reach a handler are refused by it; the test is about their headers.
	f.oauthFake().start = func(oauth.AuthorizeRequest) (*oauth.AuthorizeResult, error) { return nil, oauth.ErrUnknownClient }
	covered := []string{
		"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp", "/.well-known/openid-configuration",
		"/v1/oauth/token", "/v1/oauth/revoke", "/platform/oauth/apps/register",
	}
	const origin = "https://claude.ai"
	for _, path := range covered {
		// The preflight.
		rec := f.epDo(epAddr, http.MethodOptions, path, "", "Origin", origin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization, content-type")
		h := rec.Header()
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("OPTIONS %s: %d %s", path, rec.Code, rec.Body)
		}
		for k, want := range map[string]string{
			"Access-Control-Allow-Origin":   "*",
			"Access-Control-Allow-Methods":  "GET, POST, DELETE, OPTIONS",
			"Access-Control-Allow-Headers":  "authorization, content-type, mcp-protocol-version, mcp-session-id, last-event-id",
			"Access-Control-Expose-Headers": "WWW-Authenticate, Mcp-Session-Id",
		} {
			if got := h.Get(k); got != want {
				t.Errorf("OPTIONS %s: %s = %q, want %q", path, k, got, want)
			}
		}
		if h.Get("Access-Control-Allow-Credentials") != "" || h.Get("Vary") != "" {
			t.Errorf("OPTIONS %s: credentials or Vary: %v", path, h)
		}
		// A real request from a page carries the origin header on its answer, errors included.
		rec = f.epDo(epAddr, http.MethodGet, path, "", "Origin", origin)
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("GET %s: %v", path, rec.Header())
		}
	}
	// Elsewhere the dashboard policy applies: its own origin, with credentials, and no one else's.
	dash := f.cfg.DashboardURL()
	for _, path := range []string{"/v1/projects", "/platform/profile", "/v1/oauth/authorize", "/platform/oauth/authorizations/" + epAuthID, "/mcp", "/claim"} {
		rec := f.epDo(epAddr, http.MethodOptions, path, "", "Origin", origin)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("OPTIONS %s from another origin got CORS headers: %v", path, rec.Header())
		}
		rec = f.epDo(epAddr, http.MethodOptions, path, "", "Origin", dash)
		h := rec.Header()
		if rec.Code != 204 || h.Get("Access-Control-Allow-Origin") != dash || h.Get("Access-Control-Allow-Credentials") != "true" {
			t.Errorf("OPTIONS %s from the dashboard: %d %v", path, rec.Code, h)
		}
		rec = f.epDo(epAddr, http.MethodGet, path, "", "Origin", origin)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("GET %s from another origin got CORS headers: %v", path, rec.Header())
		}
	}
	// The dashboard origin on a covered path gets the open policy, not credentials.
	rec := f.epDo(epAddr, http.MethodOptions, "/v1/oauth/token", "", "Origin", dash)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("the dashboard on /v1/oauth/token: %v", rec.Header())
	}
	// Switched off, a covered path has no open policy; the dashboard policy applies again.
	f.cfg.API.DisableOAuth = true
	rec = f.epDo(epAddr, http.MethodOptions, "/v1/oauth/token", "", "Origin", origin)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disable_oauth: %v", rec.Header())
	}
	rec = f.epDo(epAddr, http.MethodOptions, "/v1/oauth/token", "", "Origin", dash)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != dash {
		t.Errorf("disable_oauth, dashboard: %d %v", rec.Code, rec.Header())
	}
}
