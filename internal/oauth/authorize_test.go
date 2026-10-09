package oauth

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// redirectErr returns the *RedirectError inside err, or fails the test.
func redirectErr(t *testing.T, err error) *RedirectError {
	t.Helper()
	var re *RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("want a *RedirectError, got %T %v", err, err)
	}
	return re
}

func TestStartAuthorization(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	req := authorizeReq(&ra.App)

	res, err := fx.svc.StartAuthorization(fx.ctx(), req)
	mustNoErr(t, err)
	if _, ok := canonUUID(res.AuthID); !ok {
		t.Fatalf("auth id %q", res.AuthID)
	}
	if want := testDashboard + "/authorize?auth_id=" + res.AuthID; res.ConsentURL != want {
		t.Errorf("consent URL = %q, want %q", res.ConsentURL, want)
	}
	a, err := fx.store.GetAuthorization(fx.ctx(), res.AuthID)
	mustNoErr(t, err)
	if a.Status != StatusPending || a.AppID != ra.App.ID || a.RedirectURI != testRedirect || a.State != testState ||
		a.CodeChallenge != pkceChallengeS256(testVerifier) || a.Resource != "" || a.OrgHint != "" {
		t.Errorf("stored authorization: %+v", a)
	}
	if !a.CreatedAt.Equal(fx.clock.Now()) || !a.ExpiresAt.Equal(fx.clock.Now().Add(10*time.Minute)) {
		t.Errorf("times: created %v expires %v", a.CreatedAt, a.ExpiresAt)
	}
	if !slices.Equal(a.Scopes, NormalizeScopes(AdvertisedScopes)) {
		t.Errorf("scopes = %v", a.Scopes)
	}

	// An organization hint is stored and carried to the consent page.
	req.OrganizationSlug = "acme"
	res, err = fx.svc.StartAuthorization(fx.ctx(), req)
	mustNoErr(t, err)
	if want := testDashboard + "/authorize?auth_id=" + res.AuthID + "&organization_slug=acme"; res.ConsentURL != want {
		t.Errorf("consent URL = %q, want %q", res.ConsentURL, want)
	}
	if a, _ := fx.store.GetAuthorization(fx.ctx(), res.AuthID); a.OrgHint != "acme" {
		t.Errorf("org hint = %q", a.OrgHint)
	}
	req.OrganizationSlug = ""

	// The scope parameter narrows; unknown and unavailable scopes drop out.
	req.Scope = "projects:read bogus:scope database:read projects:read organizations:write"
	res, err = fx.svc.StartAuthorization(fx.ctx(), req)
	mustNoErr(t, err)
	if a, _ := fx.store.GetAuthorization(fx.ctx(), res.AuthID); !slices.Equal(a.Scopes, []string{ScopeDatabaseRead, ScopeProjectsRead}) {
		t.Errorf("narrowed scopes = %v", a.Scopes)
	}
}

// TestAuthorizeNoRedirectBeforeValidation is C6 at the service: an unknown client or an unregistered
// redirect_uri is a plain error, never a *RedirectError, and stores nothing.
func TestAuthorizeNoRedirectBeforeValidation(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register("http://127.0.0.1/callback", "https://app.example.test/cb")
	manual, _ := fx.manual(testOrgAcme)
	if _, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: manual.ID, Actor: testUserA}); err != nil {
		t.Fatal(err)
	}
	good := authorizeReq(&ra.App)

	tests := []struct {
		name string
		mod  func(*AuthorizeRequest)
		want error
	}{
		{"no client_id", func(r *AuthorizeRequest) { r.ClientID = "" }, ErrUnknownClient},
		{"malformed client_id", func(r *AuthorizeRequest) { r.ClientID = "not-a-uuid" }, ErrUnknownClient},
		{"unknown client_id", func(r *AuthorizeRequest) { r.ClientID = secrets.NewUUID() }, ErrUnknownClient},
		{"deleted client", func(r *AuthorizeRequest) { r.ClientID = manual.ID }, ErrUnknownClient},
		{"no redirect_uri", func(r *AuthorizeRequest) { r.RedirectURI = "" }, ErrInvalidRedirectURI},
		{"unregistered redirect_uri", func(r *AuthorizeRequest) { r.RedirectURI = "https://evil.example.test/cb" }, ErrInvalidRedirectURI},
		{"registered path, other host", func(r *AuthorizeRequest) { r.RedirectURI = "https://app.example.test.evil.test/cb" }, ErrInvalidRedirectURI},
		{"https with another port", func(r *AuthorizeRequest) { r.RedirectURI = "https://app.example.test:8443/cb" }, ErrInvalidRedirectURI},
		{"loopback other path", func(r *AuthorizeRequest) { r.RedirectURI = "http://127.0.0.1:5000/other" }, ErrInvalidRedirectURI},
		{"localhost for 127.0.0.1", func(r *AuthorizeRequest) { r.RedirectURI = "http://localhost:5000/callback" }, ErrInvalidRedirectURI},
		// Even with every other parameter broken, the answer is still a page: nothing is trusted yet.
		{"unregistered redirect and a bad response_type", func(r *AuthorizeRequest) {
			r.RedirectURI, r.ResponseType = "https://evil.example.test/cb", "token"
		}, ErrInvalidRedirectURI},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := good
			tc.mod(&req)
			_, err := fx.svc.StartAuthorization(fx.ctx(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			var re *RedirectError
			if errors.As(err, &re) {
				t.Fatalf("the error would redirect to %q", re.RedirectURI)
			}
		})
	}
	if n, _ := fx.store.CountPending(fx.ctx(), "", fx.clock.Now()); n != 0 {
		t.Errorf("%d authorizations were stored by refused requests", n)
	}
}

// TestAuthorizeRedirectErrors is C1, C7 and C8 at the service: once the client and the redirect are
// valid, every error redirects, with the state unchanged.
func TestAuthorizeRedirectErrors(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register("http://127.0.0.1/callback")
	good := authorizeReq(&ra.App)
	challenge := pkceChallengeS256(testVerifier)
	tests := []struct {
		name string
		mod  func(*AuthorizeRequest)
		code string
	}{
		{"response_type token", func(r *AuthorizeRequest) { r.ResponseType = "token" }, CodeUnsupportedResponseType},
		{"response_type id_token", func(r *AuthorizeRequest) { r.ResponseType = "code id_token" }, CodeUnsupportedResponseType},
		{"no response_type", func(r *AuthorizeRequest) { r.ResponseType = "" }, CodeInvalidRequest},
		{"response_mode fragment", func(r *AuthorizeRequest) { r.ResponseMode = "fragment" }, CodeInvalidRequest},
		{"response_mode form_post", func(r *AuthorizeRequest) { r.ResponseMode = "form_post" }, CodeInvalidRequest},
		{"no PKCE at all", func(r *AuthorizeRequest) { r.CodeChallenge, r.CodeChallengeMethod = "", "" }, CodeInvalidRequest},
		{"plain", func(r *AuthorizeRequest) { r.CodeChallengeMethod = "plain" }, CodeInvalidRequest},
		{"s256 in lower case", func(r *AuthorizeRequest) { r.CodeChallengeMethod = "s256" }, CodeInvalidRequest},
		{"sha256", func(r *AuthorizeRequest) { r.CodeChallengeMethod = "sha256" }, CodeInvalidRequest},
		{"challenge without a method", func(r *AuthorizeRequest) { r.CodeChallengeMethod = "" }, CodeInvalidRequest},
		{"method without a challenge", func(r *AuthorizeRequest) { r.CodeChallenge = "" }, CodeInvalidRequest},
		{"challenge of 42 characters", func(r *AuthorizeRequest) { r.CodeChallenge = challenge[:42] }, CodeInvalidRequest},
		{"challenge of 129 characters", func(r *AuthorizeRequest) { r.CodeChallenge = strings.Repeat("a", 129) }, CodeInvalidRequest},
		{"challenge with a bad character", func(r *AuthorizeRequest) { r.CodeChallenge = challenge[:42] + "+" }, CodeInvalidRequest},
		{"only unknown scopes", func(r *AuthorizeRequest) { r.Scope = "bogus other" }, CodeInvalidScope},
		{"only a scope the app may not hold", func(r *AuthorizeRequest) { r.Scope = "organizations:write" }, CodeInvalidScope},
		{"resource of another host", func(r *AuthorizeRequest) { r.Resource = "https://evil.example.test/mcp" }, CodeInvalidTarget},
		{"resource of another path", func(r *AuthorizeRequest) { r.Resource = testIssuer + "/other" }, CodeInvalidTarget},
		{"resource that is not a URL", func(r *AuthorizeRequest) { r.Resource = "mcp" }, CodeInvalidTarget},
		{"organization_slug with a space", func(r *AuthorizeRequest) { r.OrganizationSlug = "my org" }, CodeInvalidRequest},
		{"organization_slug with a slash", func(r *AuthorizeRequest) { r.OrganizationSlug = "a/b" }, CodeInvalidRequest},
		{"organization_slug of 129 characters", func(r *AuthorizeRequest) { r.OrganizationSlug = strings.Repeat("a", 129) }, CodeInvalidRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := good
			tc.mod(&req)
			_, err := fx.svc.StartAuthorization(fx.ctx(), req)
			re := redirectErr(t, err)
			if re.Err.Code != tc.code {
				t.Errorf("code = %s (%s), want %s", re.Err.Code, re.Err.Description, tc.code)
			}
			if re.RedirectURI != req.RedirectURI || re.State != testState {
				t.Errorf("redirect %q state %q; want the request's own", re.RedirectURI, re.State)
			}
			if strings.Contains(re.Err.Description, testState) || strings.Contains(re.Err.Description, testVerifier) {
				t.Errorf("the description echoes a parameter: %q", re.Err.Description)
			}
		})
	}

	// redirect_uri plus state is bounded, and an oversized state is not echoed.
	req := good
	req.State = strings.Repeat("s", MaxRedirectAndState)
	_, err := fx.svc.StartAuthorization(fx.ctx(), req)
	re := redirectErr(t, err)
	if re.Err.Code != CodeInvalidRequest || re.State != "" {
		t.Errorf("oversized state: code %s, state %d bytes", re.Err.Code, len(re.State))
	}
	req.State = strings.Repeat("s", MaxRedirectAndState-len(req.RedirectURI))
	if _, err := fx.svc.StartAuthorization(fx.ctx(), req); err != nil {
		t.Errorf("redirect_uri plus state of exactly %d bytes: %v", MaxRedirectAndState, err)
	}
	if n, _ := fx.store.CountPending(fx.ctx(), "", fx.clock.Now()); n != 1 {
		t.Errorf("%d pending authorizations, want 1: refused requests store nothing", n)
	}
}

// TestAuthorizeResource is C8: the resource is canonicalized and stored in the server's spelling.
func TestAuthorizeResource(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	for in, want := range map[string]string{
		"":                                    "",
		testIssuer + "/mcp":                   testIssuer + "/mcp",
		testIssuer + "/mcp/":                  testIssuer + "/mcp",
		"HTTPS://API.EXAMPLE.TEST/mcp":        testIssuer + "/mcp",
		testIssuer + "/mcp?project_ref=abcde": testIssuer + "/mcp",
		testIssuer + "/mcp#x":                 testIssuer + "/mcp",
		testIssuer:                            testIssuer,
		testIssuer + "/":                      testIssuer,
	} {
		req := authorizeReq(&ra.App)
		req.Resource = in
		res, err := fx.svc.StartAuthorization(fx.ctx(), req)
		if err != nil {
			t.Errorf("resource %q: %v", in, err)
			continue
		}
		if a, _ := fx.store.GetAuthorization(fx.ctx(), res.AuthID); a.Resource != want {
			t.Errorf("resource %q stored as %q, want %q", in, a.Resource, want)
		}
	}
}

// TestAuthorizeManualApp: PKCE is optional for a manual app, but when it is used it must be S256.
func TestAuthorizeManualApp(t *testing.T) {
	fx := newSvc(t)
	app, _ := fx.manual(testOrgAcme)
	req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0], State: "s"}
	res, err := fx.svc.StartAuthorization(fx.ctx(), req)
	mustNoErr(t, err)
	if a, _ := fx.store.GetAuthorization(fx.ctx(), res.AuthID); a.CodeChallenge != "" {
		t.Errorf("challenge = %q", a.CodeChallenge)
	}
	req.CodeChallenge = pkceChallengeS256(testVerifier)
	_, err = fx.svc.StartAuthorization(fx.ctx(), req) // a challenge without a method
	wantCode(t, err, CodeInvalidRequest)
	req.CodeChallengeMethod = "plain"
	_, err = fx.svc.StartAuthorization(fx.ctx(), req)
	wantCode(t, err, CodeInvalidRequest)
	req.CodeChallengeMethod = PKCEMethodS256
	mustNoErr(t, errOf(fx.svc.StartAuthorization(fx.ctx(), req)))
}

func errOf[T any](_ T, err error) error { return err }

// TestAuthorizeCaps is R2: pending requests per app and in total.
func TestAuthorizeCaps(t *testing.T) {
	fx := newSvc(t)
	a1, a2 := fx.register(), fx.register()
	for i := 0; i < MaxPendingPerApp; i++ {
		fx.start(authorizeReq(&a1.App))
	}
	if _, err := fx.svc.StartAuthorization(fx.ctx(), authorizeReq(&a1.App)); !errors.Is(err, ErrLimit) {
		t.Fatalf("the 26th pending request of an app: %v", err)
	}
	fx.start(authorizeReq(&a2.App)) // another app is unaffected

	// An expired request does not count.
	pending, _ := fx.store.CountPending(fx.ctx(), a1.App.ID, fx.clock.Now())
	if pending != MaxPendingPerApp {
		t.Fatalf("pending = %d", pending)
	}
	fx.clock.Advance(AuthorizationTTL + time.Second)
	fx.start(authorizeReq(&a1.App))

	// The total cap: fill the store with pending rows of a third app.
	a3 := fx.register()
	total, _ := fx.store.CountPending(fx.ctx(), "", fx.clock.Now())
	for i := total; i < MaxPendingTotal; i++ {
		a := Authorization{ID: secrets.NewUUID(), AppID: a3.App.ID, RedirectURI: testRedirect, Scopes: []string{ScopeProjectsRead},
			CreatedAt: fx.clock.Now(), ExpiresAt: fx.clock.Now().Add(time.Hour), Status: StatusPending}
		if err := fx.store.CreateAuthorization(fx.ctx(), a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.svc.StartAuthorization(fx.ctx(), authorizeReq(&a2.App)); !errors.Is(err, ErrLimit) {
		t.Fatalf("beyond %d pending requests in total: %v", MaxPendingTotal, err)
	}
}

func TestDescribe(t *testing.T) {
	fx := newSvc(t)
	ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{
		ClientName: "Cursor", ClientURI: "https://cursor.example.test", LogoURI: "https://cursor.example.test/logo.png",
		RedirectURIs: []string{"http://127.0.0.1/callback"}, Scope: "projects:read database:read",
	})
	mustNoErr(t, err)
	req := authorizeReq(&ra.App)
	authID := fx.start(req)

	v, err := fx.svc.Describe(fx.ctx(), authID)
	mustNoErr(t, err)
	if v.Name != "Cursor" || v.Website != "https://cursor.example.test" || v.Domain != "127.0.0.1" ||
		v.RedirectURI != testRedirect || v.RegistrationType != RegistrationDynamic {
		t.Errorf("view: %+v", v)
	}
	if v.Icon != "" {
		t.Errorf("a dynamic app's logo is shown: %q", v.Icon)
	}
	if !slices.Equal(v.Scopes, []string{ScopeDatabaseRead, ScopeProjectsRead}) {
		t.Errorf("scopes = %v", v.Scopes)
	}
	if !v.ExpiresAt.Equal(fx.clock.Now().Add(AuthorizationTTL)) || v.ApprovedAt != nil || v.ApprovedOrgSlug != "" {
		t.Errorf("expires %v approved %v/%q", v.ExpiresAt, v.ApprovedAt, v.ApprovedOrgSlug)
	}

	// Approved: the view says so, with the organization.
	fx.clock.Advance(time.Minute)
	fx.approve(authID, testUserA, testOrgAcme)
	v, err = fx.svc.Describe(fx.ctx(), authID)
	mustNoErr(t, err)
	if v.ApprovedAt == nil || !v.ApprovedAt.Equal(fx.clock.Now()) || v.ApprovedOrgSlug != "acme" {
		t.Errorf("after approval: %v %q", v.ApprovedAt, v.ApprovedOrgSlug)
	}

	// Declined: no approval is shown. Expired: still described.
	other := fx.start(req)
	mustNoErr(t, fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: other, UserID: testUserA}))
	if v, err := fx.svc.Describe(fx.ctx(), other); err != nil || v.ApprovedAt != nil || v.ApprovedOrgSlug != "" {
		t.Errorf("declined: %+v, %v", v, err)
	}
	fx.clock.Advance(2 * AuthorizationTTL)
	if v, err := fx.svc.Describe(fx.ctx(), authID); err != nil || v.ExpiresAt.After(fx.clock.Now()) {
		t.Errorf("expired: %+v, %v", v, err)
	}

	// Unknown, malformed.
	for _, id := range []string{secrets.NewUUID(), "", "nope"} {
		if _, err := fx.svc.Describe(fx.ctx(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Describe(%q) = %v", id, err)
		}
	}

	// A manual app shows its icon and website; a deleted app is not described.
	manual, _ := fx.manual(testOrgAcme)
	up := UpdateAppRequest{OrgID: testOrgAcme, AppID: manual.ID, Actor: testUserA, Name: "Manual App", Website: manual.Website,
		Icon: "https://app.example.test/icon.png", Scopes: AdvertisedScopes, RedirectURIs: manual.RedirectURIs}
	if _, err := fx.svc.UpdateApp(fx.ctx(), up); err != nil {
		t.Fatal(err)
	}
	mreq := AuthorizeRequest{ClientID: manual.ID, ResponseType: ResponseTypeCode, RedirectURI: manual.RedirectURIs[0]}
	mid := fx.start(mreq)
	v, err = fx.svc.Describe(fx.ctx(), mid)
	mustNoErr(t, err)
	if v.Icon != "https://app.example.test/icon.png" || v.Website != "https://app.example.test" || v.RegistrationType != RegistrationManual || v.Domain != "app.example.test" {
		t.Errorf("manual view: %+v", v)
	}
	// Narrowing the app narrows what the consent page lists.
	up.Scopes = []string{ScopeProjectsRead}
	if _, err := fx.svc.UpdateApp(fx.ctx(), up); err != nil {
		t.Fatal(err)
	}
	if v, _ := fx.svc.Describe(fx.ctx(), mid); !slices.Equal(v.Scopes, []string{ScopeProjectsRead}) {
		t.Errorf("scopes after narrowing: %v", v.Scopes)
	}
	if _, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: manual.ID, Actor: testUserA}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Describe(fx.ctx(), mid); !errors.Is(err, ErrNotFound) {
		t.Errorf("describe of a deleted app: %v", err)
	}
}

// TestApproveURL is C7: the URL that delivers the code.
func TestApproveURL(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register("http://127.0.0.1/callback?flow=a")
	req := authorizeReq(&ra.App)
	req.RedirectURI = "http://127.0.0.1:53123/callback?flow=a"
	authID := fx.start(req)
	u, code := fx.approve(authID, testUserA, testOrgAcme)

	if !secrets.IsAuthCode(code) {
		t.Fatalf("code %q has the wrong shape", code)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "http://127.0.0.1:53123/callback" {
		t.Errorf("target = %q", got)
	}
	q := u.Query()
	if q.Get("flow") != "a" || q.Get("state") != testState || q.Get("iss") != testIssuer {
		t.Errorf("query %v", q)
	}
	// The existing query comes first, then code, state and iss in that order.
	if !strings.HasPrefix(u.RawQuery, "flow=a&code=sbc_") || !strings.HasSuffix(u.RawQuery, "&iss="+url.QueryEscape(testIssuer)) {
		t.Errorf("raw query = %q", u.RawQuery)
	}
	if strings.Index(u.RawQuery, "&state=") > strings.Index(u.RawQuery, "&iss=") || !strings.Contains(u.RawQuery, "&state=") {
		t.Errorf("state must sit between code and iss: %q", u.RawQuery)
	}

	// The row holds the hash of the code, the approver and the organization; the code is not stored.
	a, err := fx.store.GetAuthorization(fx.ctx(), authID)
	mustNoErr(t, err)
	if a.Status != StatusApproved || a.DecidedBy != testUserA || a.OrgID != testOrgAcme || a.OrgSlug != "acme" {
		t.Errorf("row: %+v", a)
	}
	if string(a.CodeHash) != string(secrets.HashToken(code)) || a.CodeExpiresAt == nil || !a.CodeExpiresAt.Equal(fx.clock.Now().Add(AuthCodeTTL)) {
		t.Errorf("code hash/expiry: %x %v", a.CodeHash, a.CodeExpiresAt)
	}

	// No state: no state parameter.
	req.State = ""
	u2, _ := fx.approve(fx.start(req), testUserA, testOrgAcme)
	if u2.Query().Has("state") {
		t.Errorf("an empty state is sent: %q", u2.RawQuery)
	}
	// Two approvals never share a code.
	_, code2 := fx.approve(fx.start(req), testUserA, testOrgAcme)
	if code2 == code {
		t.Error("codes repeat")
	}
	// A state with reserved characters survives the round trip unchanged.
	req.State = "a b&c=d/é?#"
	u3, _ := fx.approve(fx.start(req), testUserA, testOrgAcme)
	if u3.Query().Get("state") != req.State {
		t.Errorf("state = %q", u3.Query().Get("state"))
	}
}

// TestApproveStates is K3, K2 (the hint) and the audit of decisions.
func TestApproveStates(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	req := authorizeReq(&ra.App)
	approve := func(id string, org int64, slug string) error {
		_, err := fx.svc.Approve(fx.ctx(), ApproveRequest{AuthID: id, UserID: testUserA, OrgID: org, OrgSlug: slug})
		return err
	}

	// Unknown, malformed.
	for _, id := range []string{secrets.NewUUID(), "", "nope"} {
		if err := approve(id, testOrgAcme, "acme"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Approve(%q) = %v", id, err)
		}
	}
	// The hint binds the organization.
	hinted := req
	hinted.OrganizationSlug = "acme"
	id := fx.start(hinted)
	if err := approve(id, testOrgOther, "other"); !errors.Is(err, ErrOrgMismatch) {
		t.Errorf("approving for another organization than the hint: %v", err)
	}
	if a, _ := fx.store.GetAuthorization(fx.ctx(), id); a.Status != StatusPending {
		t.Errorf("a refused approval changed the row: %s", a.Status)
	}
	mustNoErr(t, approve(id, testOrgAcme, "acme"))
	// Double approve, decline after approve.
	if err := approve(id, testOrgAcme, "acme"); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("second approval: %v", err)
	}
	if err := fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: id, UserID: testUserA}); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("decline after approval: %v", err)
	}
	// Approve after decline.
	id = fx.start(req)
	mustNoErr(t, fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: id, UserID: testUserA}))
	if err := approve(id, testOrgAcme, "acme"); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("approval after decline: %v", err)
	}
	if err := fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: id, UserID: testUserA}); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("second decline: %v", err)
	}
	// Expired.
	id = fx.start(req)
	fx.clock.Advance(AuthorizationTTL)
	if err := approve(id, testOrgAcme, "acme"); !errors.Is(err, ErrExpired) {
		t.Errorf("approval at the expiry instant: %v", err)
	}
	if err := fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: id, UserID: testUserA}); !errors.Is(err, ErrExpired) {
		t.Errorf("decline of an expired request: %v", err)
	}
	// Input the caller must supply.
	id = fx.start(req)
	if _, err := fx.svc.Approve(fx.ctx(), ApproveRequest{AuthID: id, OrgID: testOrgAcme}); !errors.Is(err, ErrInvalid) {
		t.Errorf("approval without a user: %v", err)
	}
	if _, err := fx.svc.Approve(fx.ctx(), ApproveRequest{AuthID: id, UserID: testUserA}); !errors.Is(err, ErrInvalid) {
		t.Errorf("approval without an organization: %v", err)
	}
	if err := fx.svc.Decline(fx.ctx(), DeclineRequest{AuthID: id}); !errors.Is(err, ErrInvalid) {
		t.Errorf("decline without a user: %v", err)
	}

	// The decisions are audited, with ids and the host but no code or state.
	ap, dc := fx.eventsOf(EventAuthorizationApproved), fx.eventsOf(EventAuthorizationDeclined)
	if len(ap) != 1 || len(dc) != 1 {
		t.Fatalf("events: %d approved, %d declined", len(ap), len(dc))
	}
	p := ap[0].Payload
	if p["app_id"] != ra.App.ID || p["user_id"] != testUserA || p["org_id"] != testOrgAcme || p["org_slug"] != "acme" || p["redirect_host"] != "127.0.0.1" {
		t.Errorf("approved payload: %v", p)
	}
	if dc[0].Payload["app_id"] != ra.App.ID || dc[0].Payload["user_id"] != testUserA {
		t.Errorf("declined payload: %v", dc[0].Payload)
	}
}

// TestApproveRace: of N concurrent approvals of one request, exactly one succeeds.
func TestApproveRace(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	id := fx.start(authorizeReq(&ra.App))
	var wins, others atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fx.svc.Approve(fx.ctx(), ApproveRequest{AuthID: id, UserID: testUserA, OrgID: testOrgAcme, OrgSlug: "acme"})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrAlreadyDecided):
				others.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || others.Load() != 15 {
		t.Fatalf("%d winners, %d refused", wins.Load(), others.Load())
	}
}
