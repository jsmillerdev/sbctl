package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
)

// The authorization server end to end: the real oauth.Service over its memory store behind the real
// endpoints. These tests prove the rules of design section 4 that the endpoint tests (oauth_http_test.go,
// oauth_consent_test.go) leave to the service, as a client sees them. They skip while the service is
// the stub W0 left; they need no database.

// RFC 7636 appendix B.
const (
	flowVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	flowChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

func flowS256(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// flowClock is the clock of a test; the service reads it through Service.Now.
type flowClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *flowClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *flowClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// flowFixture is a fixture whose OAuth service is the real one, on a clock the test moves, with
// the audit events and alerts it raises recorded.
type flowFixture struct {
	*fixture
	svc    *oauth.Service
	clock  *flowClock
	mu     sync.Mutex
	events []map[string]any
	alerts []oauth.AlertEvent
}

func newFlowFixture(t *testing.T) *flowFixture {
	t.Helper()
	f := newFixture(t)
	svc, ok := f.srv.oauth.(*oauth.Service)
	if !ok {
		t.Fatalf("Server.oauth is %T", f.srv.oauth)
	}
	ff := &flowFixture{fixture: f, svc: svc, clock: &flowClock{t: time.Now().Truncate(time.Second)}}
	svc.Now = ff.clock.now
	svc.Audit = func(_ context.Context, kind string, payload map[string]any) {
		ff.mu.Lock()
		defer ff.mu.Unlock()
		ff.events = append(ff.events, map[string]any{"kind": kind, "payload": payload})
	}
	svc.Alert = func(_ context.Context, e oauth.AlertEvent) {
		ff.mu.Lock()
		defer ff.mu.Unlock()
		ff.alerts = append(ff.alerts, e)
	}
	return ff
}

type flowApp struct {
	id, secret, redirect string
}

// register makes a dynamic app that redirects to a loopback address.
func (f *flowFixture) register(t *testing.T, authMethod string) flowApp {
	t.Helper()
	body := map[string]any{"client_name": "Flow Client", "redirect_uris": []string{"http://127.0.0.1:8123/callback"}}
	if authMethod != "" {
		body["token_endpoint_auth_method"] = authMethod
	}
	b, _ := json.Marshal(body)
	rec := f.epDo(epAddr, http.MethodPost, "/platform/oauth/apps/register", string(b), "Content-Type", "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	m := jsonMap(t, rec)
	return flowApp{id: m["client_id"].(string), secret: m["client_secret"].(string), redirect: "http://127.0.0.1:8123/callback"}
}

// authorize sends an authorization request with PKCE and returns the recorder.
func (f *flowFixture) authorize(app flowApp, kv ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	q := url.Values{"client_id": {app.id}, "response_type": {"code"}, "redirect_uri": {app.redirect}, "state": {"flow-state-1"},
		"code_challenge": {flowChallenge}, "code_challenge_method": {"S256"}}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			q.Del(kv[i])
			continue
		}
		q.Set(kv[i], kv[i+1])
	}
	return f.epGet(epAddr, "/v1/oauth/authorize?"+q.Encode())
}

// start authorizes and returns the auth_id Studio would be given.
func (f *flowFixture) start(t *testing.T, app flowApp, kv ...string) string {
	t.Helper()
	rec := f.authorize(app, kv...)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Scheme+"://"+u.Host != "https://studio.example.test" || u.Path != "/authorize" || u.Query().Get("auth_id") == "" {
		t.Fatalf("authorize sent the browser to %q", rec.Header().Get("Location"))
	}
	return u.Query().Get("auth_id")
}

// approve approves as the fixture's owner and returns the code and the state of the redirect.
func (f *flowFixture) approve(t *testing.T, authID string) (code, state string) {
	t.Helper()
	rec := f.epDo(epAddr, http.MethodPost, "/platform/organizations/default/oauth/authorizations/"+authID+"?skip_browser_redirect=true", "", f.epJWT()...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(jsonMap(t, rec)["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "http://127.0.0.1:8123/callback" || q.Get("iss") != "https://api.example.test" {
		t.Fatalf("approve sent the client to %s", u)
	}
	return q.Get("code"), q.Get("state")
}

func (f *flowFixture) token(app flowApp, kv ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{"client_id": {app.id}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	return f.postToken(form.Encode(), "Authorization", basicHeader(app.id, app.secret))
}

// redeem runs a whole flow up to the token response.
func (f *flowFixture) redeem(t *testing.T, app flowApp) map[string]any {
	t.Helper()
	code, _ := f.approve(t, f.start(t, app))
	rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect)
	if rec.Code != 200 {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	return jsonMap(t, rec)
}

func (f *flowFixture) errorRedirect(t *testing.T, rec *httptest.ResponseRecorder) url.Values {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want a redirect: %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(u.String(), "http://127.0.0.1:8123/callback?") {
		t.Fatalf("redirect to %q", rec.Header().Get("Location"))
	}
	return u.Query()
}

// C1: PKCE with S256 is required of a dynamic app; every other method, a challenge without a
// method, a malformed challenge and no challenge at all go back to the client as invalid_request
// with its state and the issuer.
func TestAuthorizeRequiresS256(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	good := f.authorize(app)
	if good.Code != http.StatusFound || !strings.HasPrefix(good.Header().Get("Location"), "https://studio.example.test/authorize?auth_id=") {
		t.Fatalf("a valid request: %d %s", good.Code, good.Header().Get("Location"))
	}
	for name, kv := range map[string][]string{
		"plain":                   {"code_challenge_method", "plain"},
		"sha256":                  {"code_challenge_method", "sha256"},
		"lower case":              {"code_challenge_method", "s256"},
		"challenge, no method":    {"code_challenge_method", ""},
		"method, no challenge":    {"code_challenge", ""},
		"no PKCE":                 {"code_challenge", "", "code_challenge_method", ""},
		"challenge too short":     {"code_challenge", strings.Repeat("a", 42)},
		"challenge too long":      {"code_challenge", strings.Repeat("a", 129)},
		"challenge not base64url": {"code_challenge", strings.Repeat("a", 42) + "+"},
	} {
		q := f.errorRedirect(t, f.authorize(app, kv...))
		if q.Get("error") != "invalid_request" || q.Get("state") != "flow-state-1" || q.Get("iss") != "https://api.example.test" || q.Get("code") != "" {
			t.Errorf("%s: %v", name, q)
		}
	}
	// Other refusals of the same stage.
	if q := f.errorRedirect(t, f.authorize(app, "response_type", "token")); q.Get("error") != "unsupported_response_type" || q.Get("iss") == "" {
		t.Errorf("response_type=token: %v", q)
	}
	if q := f.errorRedirect(t, f.authorize(app, "response_mode", "fragment")); q.Get("error") == "" || q.Get("iss") == "" {
		t.Errorf("response_mode=fragment: %v", q)
	}
	if q := f.errorRedirect(t, f.authorize(app, "scope", "analytics:write")); q.Get("error") != "invalid_scope" {
		t.Errorf("a scope the app does not hold: %v", q)
	}
	// A client and a redirect URI that do not match are never redirected to.
	unknown := f.authorize(flowApp{id: "11111111-1111-4111-8111-111111111111", redirect: app.redirect})
	if unknown.Code != http.StatusUnprocessableEntity || unknown.Header().Get("Location") != "" {
		t.Errorf("unknown client: %d %v", unknown.Code, unknown.Header())
	}
	for _, uri := range []string{"http://localhost:8123/callback", "http://127.0.0.1:8123/other", "https://127.0.0.1:8123/callback", "http://127.0.0.1.evil.example/callback", "http://127.0.0.1@evil.example/callback"} {
		rec := f.authorize(flowApp{id: app.id, redirect: uri})
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("redirect_uri %q: %d %v", uri, rec.Code, rec.Header().Get("Location"))
		}
	}
	// A loopback client may use another port (RFC 8252 section 7.3).
	if rec := f.authorize(flowApp{id: app.id, redirect: "http://127.0.0.1:54321/callback"}); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://studio.example.test/") {
		t.Errorf("another loopback port: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// redirect_uri plus state may be 4096 characters at most.
	q := f.errorRedirect(t, f.authorize(app, "state", strings.Repeat("s", 4096)))
	if q.Get("error") != "invalid_request" {
		t.Errorf("a state that makes the request too long: %v", q)
	}
}

// C8: the resource is canonicalized and must name this server's MCP endpoint or its origin.
func TestAuthorizeResource(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	for _, ok := range []string{"https://api.example.test/mcp", "https://api.example.test/mcp/", "https://api.example.test/mcp?x=1#frag", "https://api.example.test", "https://api.example.test/"} {
		if rec := f.authorize(app, "resource", ok); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://studio.example.test/") {
			t.Errorf("resource %q: %d %s", ok, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, bad := range []string{"https://evil.example/mcp", "https://api.example.test/other", "http://api.example.test/mcp", "https://api.example.test.evil.example/mcp", "mcp", "https://studio.example.test/mcp"} {
		if q := f.errorRedirect(t, f.authorize(app, "resource", bad)); q.Get("error") != "invalid_target" || q.Get("state") != "flow-state-1" {
			t.Errorf("resource %q: %v", bad, q)
		}
	}
	// The resource a grant is bound to must come back on the exchange.
	code, _ := f.approve(t, f.start(t, app, "resource", "https://api.example.test/mcp/"))
	rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect, "resource", "https://api.example.test/other")
	if m := jsonMap(t, rec); rec.Code != 400 || m["error"] != "invalid_target" {
		t.Errorf("a different resource at the exchange: %d %s", rec.Code, rec.Body)
	}
}

// K3 and C7 over the service: one decision per request, the code is for 60 seconds, the redirect
// carries the state and the issuer, and the consent page can see what it is asked to approve.
func TestApproveStates(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	path := func(slug, id string) string { return "/platform/organizations/" + slug + "/oauth/authorizations/" + id }

	id := f.start(t, app, "scope", "projects:read database:read", "organization_slug", "default")
	rec := f.epGet(epAddr, "/platform/oauth/authorizations/"+id, f.epJWT()...)
	if rec.Code != 200 {
		t.Fatalf("describe: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/oauth/authorizations/{id}", rec.Body.Bytes())
	m := jsonMap(t, rec)
	if m["name"] != "Flow Client" || m["domain"] != "127.0.0.1" || m["registration_type"] != "dynamic" || m["redirect_uri"] != app.redirect {
		t.Errorf("describe: %s", rec.Body)
	}
	if _, ok := m["icon"]; ok {
		t.Errorf("a dynamic app shows no icon: %s", rec.Body)
	}
	if _, ok := m["approved_at"]; ok {
		t.Errorf("approved_at before approval: %s", rec.Body)
	}
	if sc := m["scopes"].([]any); len(sc) != 2 {
		t.Errorf("scopes %v", sc)
	}

	// A hint for another organization is refused; the request stays open.
	if rec := f.epDo(epAddr, http.MethodPost, path("other-org", id), "", f.epJWT()...); rec.Code != 404 && rec.Code != 403 {
		t.Errorf("approving in an organization that is not the hint: %d", rec.Code)
	}
	code, state := f.approve(t, id)
	if !strings.HasPrefix(code, oauth.AuthCodePrefix) || len(code) != len(oauth.AuthCodePrefix)+oauth.AuthCodeHexLen || state != "flow-state-1" {
		t.Errorf("the redirect carries code %q state %q", code, state)
	}
	// Decided once: a second approval and a decline are 409.
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := f.epDo(epAddr, method, path("default", id), "", f.epJWT()...); rec.Code != http.StatusConflict {
			t.Errorf("%s of an approved request: %d %s", method, rec.Code, rec.Body)
		}
	}
	// The page then reads the approval.
	m = jsonMap(t, f.epGet(epAddr, "/platform/oauth/authorizations/"+id, f.epJWT()...))
	if m["approved_organization_slug"] != "default" || m["approved_at"] == nil {
		t.Errorf("describe after approval: %v", m)
	}

	// Declined requests stay declined.
	id2 := f.start(t, app)
	if rec := f.epDo(epAddr, http.MethodDelete, path("default", id2), "", f.epJWT()...); rec.Code != 200 || jsonMap(t, rec)["id"] != id2 {
		t.Errorf("decline: %d %s", rec.Code, rec.Body)
	}
	if rec := f.epDo(epAddr, http.MethodPost, path("default", id2), "", f.epJWT()...); rec.Code != http.StatusConflict {
		t.Errorf("approving a declined request: %d", rec.Code)
	}

	// A request that waited ten minutes is expired (410) and is still described.
	id3 := f.start(t, app)
	f.clock.advance(oauth.AuthorizationTTL + time.Second)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := f.epDo(epAddr, method, path("default", id3), "", f.epJWT()...); rec.Code != http.StatusGone {
			t.Errorf("%s of an expired request: %d %s", method, rec.Code, rec.Body)
		}
	}
	if rec := f.epGet(epAddr, "/platform/oauth/authorizations/"+id3, f.epJWT()...); rec.Code != 200 {
		t.Errorf("describe of an expired request: %d", rec.Code)
	}
	// Unknown requests.
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := f.epDo(epAddr, method, path("default", "3f1c1b3e-0000-4000-8000-000000000000"), "", f.epJWT()...); rec.Code != 404 {
			t.Errorf("%s of an unknown request: %d", method, rec.Code)
		}
	}
	// A code lives for 60 seconds: after that the exchange fails.
	id4 := f.start(t, app)
	code4, _ := f.approve(t, id4)
	f.clock.advance(oauth.AuthCodeTTL + time.Second)
	rec = f.token(app, "grant_type", "authorization_code", "code", code4, "code_verifier", flowVerifier, "redirect_uri", app.redirect)
	if rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_grant" {
		t.Errorf("an expired code: %d %s", rec.Code, rec.Body)
	}
}

// The whole flow as a client lives it, with the refusals that matter at each step: wrong verifier
// burns the code, a replay revokes the grant and raises the alert, refresh rotates, revoke ends it.
func TestOAuthFlowEndToEnd(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")

	// A wrong verifier is invalid_grant, and the code is burned.
	code, _ := f.approve(t, f.start(t, app))
	rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowS256("other-verifier")+"x", "redirect_uri", app.redirect)
	if rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %s", rec.Code, rec.Body)
	}
	if rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect); rec.Code != 400 {
		t.Errorf("the code that met a wrong verifier still works: %d", rec.Code)
	}
	// A wrong redirect URI burns it too.
	code, _ = f.approve(t, f.start(t, app))
	if rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", "http://127.0.0.1:9/callback"); rec.Code != 400 {
		t.Errorf("a redirect_uri with another port: %d", rec.Code)
	}
	if rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect); rec.Code != 400 {
		t.Errorf("the code that met a wrong redirect_uri still works: %d", rec.Code)
	}

	// The real exchange.
	tok := f.redeem(t, app)
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if !strings.HasPrefix(access, "sbp_oauth_") || len(access) != len("sbp_oauth_")+40 || !strings.HasPrefix(refresh, "sbr_") || len(refresh) != 4+64 ||
		tok["token_type"] != "Bearer" || tok["expires_in"] != float64(3600) || tok["scope"] == "" {
		t.Fatalf("token response %v", tok)
	}

	// A code that is redeemed twice revokes the grant that the first redemption made. The replayed
	// code belongs to an app of its own: a new grant of the same app, user and organization would
	// supersede the grant that the refresh steps below use.
	replayApp := f.register(t, "")
	code, _ = f.approve(t, f.start(t, replayApp))
	first := f.token(replayApp, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", replayApp.redirect)
	if first.Code != 200 {
		t.Fatalf("first redemption: %d %s", first.Code, first.Body)
	}
	if rec := f.token(replayApp, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", replayApp.redirect); rec.Code != 400 {
		t.Errorf("replay: %d", rec.Code)
	}
	firstRefresh := jsonMap(t, first)["refresh_token"].(string)
	if rec := f.token(replayApp, "grant_type", "refresh_token", "refresh_token", firstRefresh); rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_grant" {
		t.Errorf("the grant of a replayed code still refreshes: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	reuseAlerts := 0
	for _, a := range f.alerts {
		if a.Kind == oauth.AlertKindTokenReuse {
			reuseAlerts++
		}
	}
	f.mu.Unlock()
	if reuseAlerts != 1 {
		t.Errorf("%d reuse alerts after one replay, want 1", reuseAlerts)
	}

	// Refresh rotates: a new pair; the old refresh token works again only inside the grace window.
	rec = f.token(app, "grant_type", "refresh_token", "refresh_token", refresh)
	if rec.Code != 200 {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	next := jsonMap(t, rec)
	if next["refresh_token"] == refresh || next["access_token"] == access {
		t.Errorf("refresh did not rotate: %v", next)
	}
	f.clock.advance(5 * time.Second)
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", refresh); rec.Code != 200 {
		t.Errorf("reuse inside the grace window: %d %s", rec.Code, rec.Body)
	}
	// Scope only narrows.
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", next["refresh_token"].(string), "scope", "analytics_config:write"); rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_scope" {
		t.Errorf("a wider scope on refresh: %d %s", rec.Code, rec.Body)
	}
	// Reuse after the window revokes the grant, the newest token with it.
	f.clock.advance(oauth.RefreshGrace + time.Second)
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", refresh); rec.Code != 400 || jsonMap(t, rec)["error"] != "invalid_grant" {
		t.Errorf("reuse after the grace window: %d %s", rec.Code, rec.Body)
	}
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", next["refresh_token"].(string)); rec.Code != 400 {
		t.Errorf("the grant survived a reused refresh token: %d", rec.Code)
	}

	// Revoke: bad credentials are 401, an unknown token is 204, the right token ends the grant.
	tok = f.redeem(t, app)
	revoke := func(secret, token string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"client_id": app.id, "client_secret": secret, "refresh_token": token})
		return f.epDo(epAddr, http.MethodPost, "/v1/oauth/revoke", string(b), "Content-Type", "application/json")
	}
	if rec := revoke("sba_"+strings.Repeat("0", 64), tok["refresh_token"].(string)); rec.Code != 401 {
		t.Errorf("revoke with a wrong secret: %d", rec.Code)
	}
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", tok["refresh_token"].(string)); rec.Code != 200 {
		t.Fatalf("a refused revoke ended the grant: %d", rec.Code)
	}
	if rec := revoke(app.secret, "sbr_"+strings.Repeat("0", 64)); rec.Code != 204 {
		t.Errorf("revoke of an unknown token: %d", rec.Code)
	}
	other := f.register(t, "")
	otherTok := f.redeem(t, other)
	if rec := revoke(app.secret, otherTok["refresh_token"].(string)); rec.Code != 204 {
		t.Errorf("revoke of another app's token: %d", rec.Code)
	}
	if rec := f.token(other, "grant_type", "refresh_token", "refresh_token", otherTok["refresh_token"].(string)); rec.Code != 200 {
		t.Errorf("an app revoked another app's grant: %d", rec.Code)
	}
	if rec := revoke(app.secret, tok["refresh_token"].(string)); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := f.token(app, "grant_type", "refresh_token", "refresh_token", tok["refresh_token"].(string)); rec.Code != 400 {
		t.Errorf("a revoked grant refreshed: %d", rec.Code)
	}
}

// T1 over the service: a wrong secret is invalid_client (with the Basic challenge when Basic was
// used), the secret may be sent in the body, and a dynamic app that registered as public needs no
// secret when its PKCE verifier is right.
func TestTokenClientAuthThroughService(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	exchange := func(creds []string, kv ...string) *httptest.ResponseRecorder {
		code, _ := f.approve(t, f.start(t, app))
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {flowVerifier}, "redirect_uri": {app.redirect}}
		for i := 0; i+1 < len(kv); i += 2 {
			form.Set(kv[i], kv[i+1])
		}
		return f.postToken(form.Encode(), creds...)
	}
	wrong := "sba_" + strings.Repeat("f", 64)
	rec := exchange([]string{"Authorization", basicHeader(app.id, wrong)})
	if rec.Code != 401 || jsonMap(t, rec)["error"] != "invalid_client" || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("wrong secret in Basic: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec = exchange(nil, "client_id", app.id, "client_secret", wrong)
	if rec.Code != 401 || jsonMap(t, rec)["error"] != "invalid_client" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("wrong secret in the body: %d %v", rec.Code, rec.Header())
	}
	if rec := exchange(nil, "client_id", app.id, "client_secret", app.secret); rec.Code != 200 {
		t.Errorf("secret in the body: %d %s", rec.Code, rec.Body)
	}
	if rec := exchange([]string{"Authorization", basicHeader(app.id, app.secret)}); rec.Code != 200 {
		t.Errorf("secret in Basic: %d %s", rec.Code, rec.Body)
	}
	// No secret: a dynamic app is accepted on its PKCE verifier alone, and a secret that is presented must be right.
	if rec := exchange(nil, "client_id", app.id); rec.Code != 200 {
		t.Errorf("dynamic app, no secret, right verifier: %d %s", rec.Code, rec.Body)
	}
	if rec := exchange(nil, "client_id", app.id, "code_verifier", flowVerifier+"x"); rec.Code != 400 {
		t.Errorf("dynamic app, no secret, wrong verifier: %d %s", rec.Code, rec.Body)
	}
	// An unknown client is invalid_client as well.
	if rec := f.postToken(url.Values{"grant_type": {"authorization_code"}, "client_id": {"22222222-2222-4222-8222-222222222222"}, "code": {"sbc_x"}}.Encode()); rec.Code != 401 {
		t.Errorf("unknown client: %d", rec.Code)
	}
	// The refused grant type of the spec.
	rec = f.token(app, "grant_type", oauth.GrantTypeJWTBearer, "assertion", "x")
	if rec.Code != 400 || jsonMap(t, rec)["error"] != "unsupported_grant_type" {
		t.Errorf("jwt-bearer: %d %s", rec.Code, rec.Body)
	}
	rec = f.token(app, "grant_type", "password", "username", "a", "password", "b")
	if rec.Code != 400 || jsonMap(t, rec)["error"] != "unsupported_grant_type" {
		t.Errorf("password grant: %d %s", rec.Code, rec.Body)
	}
}

// T4 over the service: the audit events and the alert of a whole flow, replay included, hold no
// secret and no state.
func TestAuditPayloadsHaveNoSecrets(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	var secrets []string
	secrets = append(secrets, flowVerifier, flowChallenge, "flow-state-1", app.secret)

	id := f.start(t, app)
	code, _ := f.approve(t, id)
	secrets = append(secrets, code)
	rec := f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect)
	if rec.Code != 200 {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	m := jsonMap(t, rec)
	secrets = append(secrets, m["access_token"].(string), m["refresh_token"].(string))
	f.token(app, "grant_type", "authorization_code", "code", code, "code_verifier", flowVerifier, "redirect_uri", app.redirect) // replay: revokes the grant, alerts
	rec = f.token(app, "grant_type", "refresh_token", "refresh_token", m["refresh_token"].(string))
	if rec.Code != 400 {
		t.Fatalf("the replay did not revoke the grant: %d", rec.Code)
	}
	// A reused refresh token on a second grant.
	tok := f.redeem(t, app)
	f.token(app, "grant_type", "refresh_token", "refresh_token", tok["refresh_token"].(string))
	f.clock.advance(time.Minute)
	f.token(app, "grant_type", "refresh_token", "refresh_token", tok["refresh_token"].(string))
	secrets = append(secrets, tok["access_token"].(string), tok["refresh_token"].(string))

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		t.Fatal("no audit event was recorded")
	}
	kinds := map[string]bool{}
	for _, e := range f.events {
		kinds[e["kind"].(string)] = true
		b, _ := json.Marshal(e)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("audit event %s holds %q", b, s)
			}
		}
	}
	for _, k := range []string{oauth.EventAuthorizationApproved, oauth.EventGrantCreated, oauth.EventGrantRevoked, oauth.EventCodeReuse, oauth.EventRefreshReuse} {
		if !kinds[k] {
			t.Errorf("no %s event; got %v", k, kinds)
		}
	}
	if len(f.alerts) < 2 {
		t.Errorf("%d alerts, want one for the replayed code and one for the reused refresh token", len(f.alerts))
	}
	for _, a := range f.alerts {
		b, _ := json.Marshal(a)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("alert %s holds %q", b, s)
			}
		}
	}
}
