package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
)

// The tests of the consent routes, over an endpointOAuth (oauth_http_test.go). The Service's own
// rules (who can still be approved, the states of a request) are tested over the real service in
// oauth_flow_test.go.

const consentCode = "sbc_" + "0123456789012345678901234567890123456789012345678901234567890123"

func consentPath(slug string) string {
	return "/platform/organizations/" + slug + "/oauth/authorizations/" + epAuthID
}

// consentUser adds a member of the fixture's organization with a role and returns a session for
// them. role 0 makes a user who belongs to no organization.
func (f *fixture) consentUser(id string, role int) string {
	f.t.Helper()
	if role != 0 {
		f.addMember(id, role)
	}
	return f.signJWT(map[string]any{"sub": id, "email": id[:8] + "@example.test"})
}

func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

func okApproval(oauth.ApproveRequest) (*oauth.ApproveResult, error) {
	return &oauth.ApproveResult{RedirectURL: "http://127.0.0.1:8123/callback?code=" + consentCode + "&state=s&iss=https%3A%2F%2Fapi.example.test"}, nil
}

// K4: what the consent page is told.
func TestDescribeAuthorization(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	expires := time.Date(2026, 10, 8, 12, 10, 0, 0, time.UTC)
	approved := time.Date(2026, 10, 8, 12, 5, 30, 0, time.UTC)
	view := &oauth.AuthorizationView{
		Name: `<img src=x onerror=alert(1)> Claude Code`, Website: "", Icon: "https://evil.example/logo.png", Domain: "127.0.0.1",
		RedirectURI: "http://127.0.0.1:8123/callback", ExpiresAt: expires, Scopes: []string{"projects:read", "database:write"},
		RegistrationType: oauth.RegistrationDynamic,
	}
	var asked string
	fake.describe = func(id string) (*oauth.AuthorizationView, error) { asked = id; return view, nil }
	path := "/platform/oauth/authorizations/" + epAuthID

	rec := f.epGet(epAddr, path, f.epJWT()...)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/oauth/authorizations/{id}", rec.Body.Bytes())
	m := jsonMap(t, rec)
	if asked != epAuthID {
		t.Errorf("the service was asked for %q", asked)
	}
	if _, ok := m["icon"]; ok {
		t.Errorf("a dynamic app has no icon, whatever the service says: %s", rec.Body)
	}
	if v, ok := m["website"]; !ok || v != "" {
		t.Errorf("website must be present and empty: %v", m)
	}
	for k, v := range map[string]any{"name": view.Name, "domain": "127.0.0.1", "redirect_uri": "http://127.0.0.1:8123/callback",
		"expires_at": "2026-10-08T12:10:00.000Z", "registration_type": "dynamic"} {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if s, _ := m["scopes"].([]any); len(s) != 2 || s[0] != "projects:read" || s[1] != "database:write" {
		t.Errorf("scopes %v", m["scopes"])
	}
	for _, k := range []string{"approved_at", "approved_organization_slug"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s is left out until the request is approved", k)
		}
	}
	if strings.Contains(rec.Body.String(), "<img") {
		t.Errorf("the name is not escaped in the JSON: %s", rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}

	// A manual app shows its own icon and website; an approved request says when and where.
	view = &oauth.AuthorizationView{Name: "Acme", Website: "https://acme.example", Icon: "https://acme.example/i.png", Domain: "app.acme.example",
		RedirectURI: "https://app.acme.example/cb", ExpiresAt: expires, ApprovedAt: &approved, ApprovedOrgSlug: "default",
		Scopes: []string{"projects:read"}, RegistrationType: oauth.RegistrationManual}
	rec = f.epGet(epAddr, path, f.epJWT()...)
	m = jsonMap(t, rec)
	validateAgainstSpec(t, "GET /platform/oauth/authorizations/{id}", rec.Body.Bytes())
	if m["icon"] != "https://acme.example/i.png" || m["website"] != "https://acme.example" || m["approved_at"] != "2026-10-08T12:05:30.000Z" ||
		m["approved_organization_slug"] != "default" || m["registration_type"] != "manual" {
		t.Errorf("%s", rec.Body)
	}

	// An expired request is described as well; Studio works out "expired" from expires_at.
	view.ExpiresAt = time.Now().Add(-time.Hour)
	if rec := f.epGet(epAddr, path, f.epJWT()...); rec.Code != 200 {
		t.Errorf("expired: %d", rec.Code)
	}
	// Scopes are an array even when the service has none.
	view.Scopes = nil
	if s, ok := jsonMap(t, f.epGet(epAddr, path, f.epJWT()...))["scopes"].([]any); !ok || len(s) != 0 {
		t.Errorf("scopes must be []")
	}

	// Unknown or malformed ids are 404; a malformed one never reaches the service.
	fake.describe = func(string) (*oauth.AuthorizationView, error) { return nil, oauth.ErrNotFound }
	if rec := f.epGet(epAddr, path, f.epJWT()...); rec.Code != 404 {
		t.Errorf("unknown id: %d", rec.Code)
	}
	calls := fake.calls
	for _, id := range []string{"x", "1", "00000000-0000-0000-0000-00000000000g", epAuthID + "0", "%27%3Bdrop"} {
		if rec := f.epGet(epAddr, "/platform/oauth/authorizations/"+id, f.epJWT()...); rec.Code != 404 {
			t.Errorf("id %q: %d", id, rec.Code)
		}
	}
	if fake.calls != calls {
		t.Errorf("malformed ids reached the service")
	}

	// Only an Owner or Administrator may ask (design decision 1); nobody without a session.
	fake.describe = func(string) (*oauth.AuthorizationView, error) { return view, nil }
	for name, c := range map[string]struct {
		jwt  string
		code int
	}{
		"administrator": {f.consentUser("a0000000-1111-4222-8333-444444444444", members.RoleAdministrator), 200},
		"developer":     {f.consentUser("d0000000-1111-4222-8333-444444444444", members.RoleDeveloper), 403},
		"read-only":     {f.consentUser("e0000000-1111-4222-8333-444444444444", members.RoleReadOnly), 403},
		"no membership": {f.consentUser("f0000000-1111-4222-8333-444444444444", 0), 403},
		"no session":    {"", 401},
	} {
		before := fake.calls
		var h []string
		if c.jwt != "" {
			h = bearer(c.jwt)
		}
		rec := f.epGet(epAddr, path, h...)
		if rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if c.code != 200 && fake.calls != before {
			t.Errorf("%s: the service was asked", name)
		}
	}
}

// K2: who may approve and decline. The route rule makes it the Owner or Administrator of the
// organization in the path; the service is not asked for anybody else.
func TestApproveRoles(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.approve = okApproval
	fake.decline = func(oauth.DeclineRequest) error { return nil }
	other, err := f.reg.CreateOrganization(context.Background(), "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	const dual = "c0000000-1111-4222-8333-444444444444"
	f.addMember(dual, members.RoleOwner) // Owner of default ...
	if err := f.srv.members.Store.Update(context.Background(), other.ID, func(ops members.Ops) error {
		return ops.PutMember(context.Background(), members.Member{OrgID: other.ID, UserID: dual, RoleID: members.RoleDeveloper}) // ... Developer of other
	}); err != nil {
		t.Fatal(err)
	}
	const elsewhere = "b0000000-1111-4222-8333-444444444444"
	if err := f.srv.members.EnsureOwner(context.Background(), members.OrgRef{ID: other.ID, Slug: other.Slug}, elsewhere); err != nil {
		t.Fatal(err)
	}
	ownerOfOther := f.signJWT(map[string]any{"sub": elsewhere, "email": "else@example.test"})
	users := []struct {
		name, jwt, slug string
		code            int
	}{
		{"owner", f.jwt, "default", 201},
		{"administrator", f.consentUser("a0000000-1111-4222-8333-444444444444", members.RoleAdministrator), "default", 201},
		{"developer", f.consentUser("d0000000-1111-4222-8333-444444444444", members.RoleDeveloper), "default", 403},
		{"read-only", f.consentUser("e0000000-1111-4222-8333-444444444444", members.RoleReadOnly), "default", 403},
		{"not a member", f.consentUser("f0000000-1111-4222-8333-444444444444", 0), "default", 403},
		{"owner of another organization", ownerOfOther, "default", 403},
		{"developer in the chosen organization", f.signJWT(map[string]any{"sub": dual, "email": "dual@example.test"}), "other", 403},
		{"owner of the chosen organization", f.signJWT(map[string]any{"sub": dual, "email": "dual@example.test"}), "default", 201},
		{"unknown organization", f.jwt, "nowhere", 404},
	}
	for _, u := range users {
		for _, decline := range []bool{false, true} {
			method, want := http.MethodPost, u.code
			if decline {
				method = http.MethodDelete
				if want == 201 {
					want = 200
				}
			}
			before := fake.calls
			rec := f.epDo(epAddr, method, consentPath(u.slug), "", bearer(u.jwt)...)
			if rec.Code != want {
				t.Errorf("%s %s: %d, want %d: %s", method, u.name, rec.Code, want, rec.Body)
			}
			if asked := fake.calls != before; asked != (want < 300) {
				t.Errorf("%s %s: service asked = %v", method, u.name, asked)
			}
		}
	}
	// Nobody without a session, whatever the id.
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := f.epDo(epAddr, method, consentPath("default"), ""); rec.Code != 401 {
			t.Errorf("%s without a session: %d", method, rec.Code)
		}
	}
	// The organization hint of the request is the service's to compare; its refusal is a 403.
	fake.approve = func(oauth.ApproveRequest) (*oauth.ApproveResult, error) { return nil, oauth.ErrOrgMismatch }
	rec := f.epDo(epAddr, http.MethodPost, consentPath("default"), "", f.epJWT()...)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "another organization") {
		t.Errorf("hint mismatch: %d %s", rec.Code, rec.Body)
	}
}

// K1: an approval mints tokens that are exempt from MFA, so a session without a second factor is
// refused when the caller belongs to any organization that requires it.
func TestApproveRequiresAAL(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.approve = okApproval
	fake.decline = func(oauth.DeclineRequest) error { return nil }
	ctx := context.Background()
	other, err := f.reg.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.members.EnsureOwner(ctx, members.OrgRef{ID: other.ID, Slug: other.Slug}, f.userID); err != nil {
		t.Fatal(err)
	}
	aal1 := f.jwt
	aal2 := f.signJWT(map[string]any{"sub": f.userID, "email": "dev@example.test", "aal": "aal2"})
	approve := func(token, slug string) *httptest.ResponseRecorder {
		return f.epDo(epAddr, http.MethodPost, consentPath(slug), "", bearer(token)...)
	}

	// No organization requires MFA: aal1 will do.
	if rec := approve(aal1, "default"); rec.Code != 201 {
		t.Fatalf("no MFA anywhere: %d %s", rec.Code, rec.Body)
	}
	// "other" requires it. The approval is in "default", which does not, and is still refused: the
	// token would work in "other" as well as a personal access token does.
	if err := f.srv.members.SetMFAEnforced(ctx, other.ID, true); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls
	rec := approve(aal1, "default")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "MFA required") {
		t.Fatalf("aal1 with another organization that requires MFA: %d %s", rec.Code, rec.Body)
	}
	if fake.calls != calls {
		t.Error("the service was asked for a session below aal2")
	}
	// The organization of the approval requires it too: the route's own gate agrees.
	if rec := approve(aal1, "other"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "MFA required") {
		t.Errorf("aal1 in the organization that requires MFA: %d %s", rec.Code, rec.Body)
	}
	// With the second factor both go through.
	for _, slug := range []string{"default", "other"} {
		if rec := approve(aal2, slug); rec.Code != 201 {
			t.Errorf("aal2 in %s: %d %s", slug, rec.Code, rec.Body)
		}
	}
	// Declining mints nothing, so the requirement is the route's alone: "default" does not have it.
	if rec := f.epDo(epAddr, http.MethodDelete, consentPath("default"), "", bearer(aal1)...); rec.Code != 200 {
		t.Errorf("decline at aal1 in an organization without MFA: %d %s", rec.Code, rec.Body)
	}
}

// K3 at the endpoint: the status of each refusal of the service. The states themselves are the
// service's (oauth_flow_test.go).
func TestApproveStatusMapping(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var fail error
	fake.approve = func(oauth.ApproveRequest) (*oauth.ApproveResult, error) { return nil, fail }
	fake.decline = func(oauth.DeclineRequest) error { return fail }
	for _, c := range []struct {
		err  error
		code int
	}{
		{oauth.ErrNotFound, 404},
		{oauth.ErrAlreadyDecided, 409},
		{oauth.ErrExpired, 410},
		{oauth.ErrOrgMismatch, 403},
		{errors.New("pq: no connection to 10.0.0.9"), 500},
	} {
		fail = c.err
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			rec := f.epDo(epAddr, method, consentPath("default"), "", f.epJWT()...)
			if rec.Code != c.code {
				t.Errorf("%s with %v: %d %s", method, c.err, rec.Code, rec.Body)
			}
			if m := jsonMap(t, rec); m["message"] == "" || strings.Contains(rec.Body.String(), "10.0.0.9") {
				t.Errorf("%s with %v: %s", method, c.err, rec.Body)
			}
			if rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), "code=") {
				t.Errorf("a refusal carries a redirect: %s", rec.Body)
			}
		}
	}
	// A malformed id is not an authorization request and never reaches the service.
	calls := fake.calls
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := f.epDo(epAddr, method, "/platform/organizations/default/oauth/authorizations/not-a-uuid", "", f.epJWT()...); rec.Code != 404 {
			t.Errorf("%s with a malformed id: %d", method, rec.Code)
		}
	}
	if fake.calls != calls {
		t.Error("a malformed id reached the service")
	}
}

// C7 at the endpoint, and the contract with Studio: what the service is asked, and the JSON that
// carries the URL; skip_browser_redirect changes nothing.
func TestApproveURL(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	var got oauth.ApproveRequest
	fake.approve = func(r oauth.ApproveRequest) (*oauth.ApproveResult, error) { got = r; return okApproval(r) }
	var declined oauth.DeclineRequest
	fake.decline = func(r oauth.DeclineRequest) error { declined = r; return nil }

	want := oauth.ApproveRequest{AuthID: epAuthID, UserID: f.userID, OrgID: f.org.ID, OrgSlug: "default"}
	var first string
	for i, q := range []string{"", "?skip_browser_redirect=true", "?skip_browser_redirect=false&x=1"} {
		rec := f.epDo(epAddr, http.MethodPost, consentPath("default")+q, "", f.epJWT()...)
		if rec.Code != 201 {
			t.Fatalf("%q: %d %s", q, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, "POST /platform/organizations/{slug}/oauth/authorizations/{id}", rec.Body.Bytes())
		if got != want {
			t.Errorf("%q: the service was asked %+v, want %+v", q, got, want)
		}
		m := jsonMap(t, rec)
		if len(m) != 1 || !strings.HasPrefix(m["url"].(string), "http://127.0.0.1:8123/callback?code=sbc_") || !strings.Contains(m["url"].(string), "&iss=") {
			t.Errorf("%q: body %s", q, rec.Body)
		}
		if i == 0 {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%q changed the answer", q)
		}
		if h := rec.Header(); h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("headers %v", h)
		}
	}
	// The numeric id of an organization is accepted as the slug by older clients; the service is
	// given the canonical slug, which is what it compares with the request's hint.
	rec := f.epDo(epAddr, http.MethodPost, consentPath(strconv.FormatInt(f.org.ID, 10)), "", f.epJWT()...)
	if rec.Code != 201 || got.OrgSlug != "default" || got.OrgID != f.org.ID {
		t.Errorf("numeric slug: %d %+v", rec.Code, got)
	}

	rec = f.epDo(epAddr, http.MethodDelete, consentPath("default"), "", f.epJWT()...)
	if rec.Code != 200 {
		t.Fatalf("decline: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "DELETE /platform/organizations/{slug}/oauth/authorizations/{id}", rec.Body.Bytes())
	if m := jsonMap(t, rec); m["id"] != epAuthID || len(m) != 1 {
		t.Errorf("decline body %s", rec.Body)
	}
	if declined != (oauth.DeclineRequest{AuthID: epAuthID, UserID: f.userID}) {
		t.Errorf("decline asked %+v", declined)
	}
}

// K5: the consent routes take a dashboard session and nothing else. A personal access token and an
// OAuth access token are refused, and so is a cookie: there is no ambient credential for another
// site to ride on.
func TestOAuthTokenRefusedOnPlatform(t *testing.T) {
	f := newFixture(t)
	fake := f.oauthFake()
	fake.describe = func(string) (*oauth.AuthorizationView, error) { return &oauth.AuthorizationView{}, nil }
	fake.approve = okApproval
	fake.decline = func(oauth.DeclineRequest) error { return nil }

	rec := f.do(http.MethodPost, "/platform/profile/access-tokens", map[string]any{"name": "ci"})
	if rec.Code != 201 {
		t.Fatalf("creating a personal access token: %d %s", rec.Code, rec.Body)
	}
	pat, _ := jsonMap(t, rec)["token"].(string)
	if !strings.HasPrefix(pat, "sbp_") {
		t.Fatalf("token %q", pat)
	}
	tokens := map[string]string{
		"personal access token": pat,
		"OAuth access token":    oauth.AccessTokenPrefix + hexOf(oauth.AccessTokenHexLen, 'a'),
		"unknown sbp_ token":    "sbp_" + hexOf(40, 'b'),
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/platform/oauth/authorizations/" + epAuthID},
		{http.MethodPost, consentPath("default")},
		{http.MethodDelete, consentPath("default")},
	}
	for name, token := range tokens {
		for _, r := range routes {
			if rec := f.epDo(epAddr, r.method, r.path, "", bearer(token)...); rec.Code != 401 {
				t.Errorf("%s on %s %s: %d %s", name, r.method, r.path, rec.Code, rec.Body)
			}
		}
	}
	for _, r := range routes {
		// A cookie carries nothing the API reads.
		rec := f.epDo(epAddr, r.method, r.path, "", "Cookie", "sb-access-token="+f.jwt+"; access_token="+f.jwt, "Origin", f.cfg.DashboardURL())
		if rec.Code != 401 {
			t.Errorf("a cookie on %s %s: %d", r.method, r.path, rec.Code)
		}
		// The same session in the header works.
		if rec := f.epDo(epAddr, r.method, r.path, "", f.epJWT()...); rec.Code >= 400 {
			t.Errorf("%s %s with a session: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if fake.calls != 3 {
		t.Errorf("the service was asked %d times, want 3 (the three requests with a session)", fake.calls)
	}
}

// oauthConformanceSteps are the OAuth operations whose answers TestImplementedRoutesMatchSpec holds to
// their schemas, over an endpointOAuth so that the check is of the handlers' output. The other two
// implemented operations are not driven there: GET /v1/oauth/authorize answers a redirect where the
// spec lists 204, and POST /v1/oauth/token takes a form that f.run cannot send. oauth_http_test.go
// checks the token answer against its schema.
func oauthConformanceSteps(f *fixture) []step {
	fake := f.oauthFake()
	fake.register = func(oauth.RegisterRequest) (*oauth.RegisteredApp, error) { return epApp(), nil }
	fake.describe = func(string) (*oauth.AuthorizationView, error) {
		return &oauth.AuthorizationView{Name: "Claude Code", Domain: "127.0.0.1", RedirectURI: "http://127.0.0.1:8123/callback",
			ExpiresAt: time.Now().Add(time.Minute), Scopes: []string{"projects:read"}, RegistrationType: oauth.RegistrationDynamic}, nil
	}
	fake.approve = okApproval
	fake.decline = func(oauth.DeclineRequest) error { return nil }
	return []step{
		{key: "POST /platform/oauth/apps/register", body: map[string]any{"client_name": "Claude Code", "redirect_uris": []string{"http://127.0.0.1:8123/callback"}}, check: want("client_id", epClient)},
		{key: "GET /platform/oauth/authorizations/{id}", path: "/platform/oauth/authorizations/" + epAuthID, check: want("registration_type", "dynamic")},
		{key: "POST /platform/organizations/{slug}/oauth/authorizations/{id}", path: consentPath("default")},
		{key: "DELETE /platform/organizations/{slug}/oauth/authorizations/{id}", path: consentPath("default"), check: want("id", epAuthID)},
	}
}
