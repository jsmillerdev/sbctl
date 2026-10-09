package oauth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

func TestCodeExchange(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	req := authorizeReq(&ra.App)
	req.Resource = testIssuer + "/mcp/"
	req.Scope = "projects:read database:read database:write"
	authID := fx.start(req)
	_, code := fx.approve(authID, testUserB, testOrgOther)
	fx.clock.Advance(30 * time.Second)

	tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{
		GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: code, RedirectURI: req.RedirectURI,
		CodeVerifier: testVerifier, Resource: testIssuer + "/mcp",
	})
	mustNoErr(t, err)
	if !secrets.IsOAuthAccessToken(tr.AccessToken) || !secrets.IsOAuthRefreshToken(tr.RefreshToken) {
		t.Fatalf("token shapes: %q %q", tr.AccessToken, tr.RefreshToken)
	}
	if tr.TokenType != "Bearer" || tr.ExpiresIn != 3600 || tr.Scope != "database:read database:write projects:read" {
		t.Errorf("response: type %q expires_in %d scope %q", tr.TokenType, tr.ExpiresIn, tr.Scope)
	}

	now := fx.clock.Now()
	at, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(tr.AccessToken))
	mustNoErr(t, err)
	if at.Kind != KindAccess || at.Prefix != "sbp_oaut" || !at.ExpiresAt.Equal(now.Add(time.Hour)) || !at.CreatedAt.Equal(now) {
		t.Errorf("access token row: %+v", at)
	}
	rt, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(tr.RefreshToken))
	mustNoErr(t, err)
	if rt.Kind != KindRefresh || !rt.ExpiresAt.Equal(now.Add(90*24*time.Hour)) || rt.GrantID != at.GrantID || rt.UsedAt != nil {
		t.Errorf("refresh token row: %+v", rt)
	}
	g, err := fx.store.GetGrant(fx.ctx(), at.GrantID)
	mustNoErr(t, err)
	if g.AppID != ra.App.ID || g.UserID != testUserB || g.OrgID != testOrgOther || g.Resource != testIssuer+"/mcp" ||
		!slices.Equal(g.Scopes, []string{ScopeDatabaseRead, ScopeDatabaseWrite, ScopeProjectsRead}) || g.RevokedAt != nil {
		t.Errorf("grant: %+v", g)
	}
	a, _ := fx.store.GetAuthorization(fx.ctx(), authID)
	if a.Status != StatusExchanged || a.GrantID != g.ID || a.CodeUsedAt == nil {
		t.Errorf("authorization: %+v", a)
	}
	if app, _ := fx.store.GetApp(fx.ctx(), ra.App.ID); app.LastAuthorizedAt == nil || !app.LastAuthorizedAt.Equal(now) {
		t.Errorf("last_authorized_at = %v", app.LastAuthorizedAt)
	}

	// The access token resolves; the refresh token does not.
	info, err := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken)
	mustNoErr(t, err)
	if info.UserID != testUserB || info.OrgID != testOrgOther || info.OrgSlug != "other" || info.AppID != ra.App.ID || info.AppName != "Test Client" ||
		info.Resource != testIssuer+"/mcp" || info.GrantID != g.ID || !slices.Equal(info.Scopes, g.Scopes) {
		t.Errorf("access info: %+v", info)
	}
	if _, err := fx.svc.LookupAccess(fx.ctx(), tr.RefreshToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refresh token resolves as an access token: %v", err)
	}

	// Events: the grant, with no secret.
	ev := fx.eventsOf(EventGrantCreated)
	if len(ev) != 1 || ev[0].Payload["grant_id"] != g.ID || ev[0].Payload["org_slug"] != "other" || ev[0].Payload["user_id"] != testUserB {
		t.Errorf("grant_created: %v", ev)
	}
	// Admit saw the approver and the organization of the approval.
	if fx.admitCalls != 1 {
		t.Errorf("Admit called %d times", fx.admitCalls)
	}
}

// redeemCase prepares an approved authorization of a fresh dynamic app and returns what a test needs
// to redeem it.
type redeemCase struct {
	fx   *svcFixture
	ra   *RegisteredApp
	req  AuthorizeRequest
	id   string
	code string
}

func newRedeem(fx *svcFixture, mod func(*AuthorizeRequest)) *redeemCase {
	fx.t.Helper()
	ra := fx.register()
	req := authorizeReq(&ra.App)
	if mod != nil {
		mod(&req)
	}
	c := &redeemCase{fx: fx, ra: ra, req: req, id: fx.start(req)}
	_, c.code = fx.approve(c.id, testUserA, testOrgAcme)
	return c
}

func (c *redeemCase) request() TokenRequest { return codeRequest(c.ra, c.req, c.code) }

func (c *redeemCase) redeem(mod func(*TokenRequest)) error {
	r := c.request()
	if mod != nil {
		mod(&r)
	}
	_, err := c.fx.svc.Exchange(c.fx.ctx(), r)
	return err
}

func (c *redeemCase) status() string {
	a, err := c.fx.store.GetAuthorization(c.fx.ctx(), c.id)
	mustNoErr(c.fx.t, err)
	return a.Status
}

// TestCodeBinding is C4: a code is bound to its client, exact redirect_uri, resource, challenge, approver
// and organization; any mismatch burns it.
func TestCodeBinding(t *testing.T) {
	withResource := func(r *AuthorizeRequest) { r.Resource = testIssuer + "/mcp" }
	tests := []struct {
		name   string
		setup  func(*AuthorizeRequest)
		mod    func(*TokenRequest)
		code   string // "" = success
		burned bool
	}{
		{"all matching", nil, nil, "", false},
		{"matching resource, spelled differently", withResource, func(r *TokenRequest) { r.Resource = testIssuer + "/mcp/" }, "", false},
		{"resource omitted", withResource, func(r *TokenRequest) { r.Resource = "" }, "", false},
		{"another port", nil, func(r *TokenRequest) { r.RedirectURI = "http://127.0.0.1:53124/callback" }, CodeInvalidGrant, true},
		{"no port", nil, func(r *TokenRequest) { r.RedirectURI = "http://127.0.0.1/callback" }, CodeInvalidGrant, true},
		{"another path", nil, func(r *TokenRequest) { r.RedirectURI = "http://127.0.0.1:53123/other" }, CodeInvalidGrant, true},
		{"another host", nil, func(r *TokenRequest) { r.RedirectURI = "https://evil.example.test/callback" }, CodeInvalidGrant, true},
		{"resource of the issuer when the mcp resource was asked", withResource, func(r *TokenRequest) { r.Resource = testIssuer }, CodeInvalidTarget, true},
		{"resource sent but none requested", nil, func(r *TokenRequest) { r.Resource = testIssuer + "/mcp" }, CodeInvalidTarget, true},
		{"resource of another server", withResource, func(r *TokenRequest) { r.Resource = "https://evil.example.test/mcp" }, CodeInvalidTarget, true},
		{"wrong verifier", nil, func(r *TokenRequest) { r.CodeVerifier = strings.Repeat("A", 43) }, CodeInvalidGrant, true},
		{"verifier of 42 characters", nil, func(r *TokenRequest) { r.CodeVerifier = testVerifier[:42] }, CodeInvalidGrant, true},
		{"verifier of 129 characters", nil, func(r *TokenRequest) { r.CodeVerifier = strings.Repeat("a", 129) }, CodeInvalidGrant, true},
		{"the challenge as the verifier", nil, func(r *TokenRequest) { r.CodeVerifier = pkceChallengeS256(testVerifier) }, CodeInvalidGrant, true},
		{"verifier with a reserved character", nil, func(r *TokenRequest) { r.CodeVerifier = testVerifier[:42] + "=" }, CodeInvalidGrant, true},
		{"no verifier", nil, func(r *TokenRequest) { r.CodeVerifier = "" }, CodeInvalidRequest, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSvc(t)
			c := newRedeem(fx, tc.setup)
			err := c.redeem(tc.mod)
			if tc.code == "" {
				mustNoErr(t, err)
				return
			}
			wantCode(t, err, tc.code)
			if tc.burned {
				if c.status() != StatusExchanged {
					t.Errorf("status = %s, the code should be burned", c.status())
				}
				// Even the right request fails now, and it is not a replay of an issued grant.
				wantCode(t, c.redeem(nil), CodeInvalidGrant)
				if gs, _ := fx.store.ListGrants(fx.ctx(), GrantFilter{}); len(gs) != 0 {
					t.Errorf("a burned code created %d grants", len(gs))
				}
				if len(fx.alertList()) != 0 {
					t.Errorf("a burned code raised alerts: %v", fx.alertList())
				}
				return
			}
			// Not burned: the right request still works.
			if c.status() != StatusApproved {
				t.Errorf("status = %s, the code should still be valid", c.status())
			}
			mustNoErr(t, c.redeem(nil))
		})
	}
}

// TestCodeBindingOther covers the bindings that need more setup than a modified request.
func TestCodeBindingOther(t *testing.T) {
	t.Run("another client cannot burn the code", func(t *testing.T) {
		fx := newSvc(t)
		c := newRedeem(fx, nil)
		other := fx.register()
		err := c.redeem(func(r *TokenRequest) { r.ClientID = other.App.ID })
		wantCode(t, err, CodeInvalidGrant)
		if c.status() != StatusApproved {
			t.Fatalf("status = %s: a different client burned the code", c.status())
		}
		mustNoErr(t, c.redeem(nil))
	})
	t.Run("a verifier for a request that had no challenge", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		id := fx.start(req)
		_, code := fx.approve(id, testUserA, testOrgAcme)
		tr := TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}
		_, err := fx.svc.Exchange(fx.ctx(), tr)
		wantCode(t, err, CodeInvalidGrant)
		tr.CodeVerifier = ""
		_, err = fx.svc.Exchange(fx.ctx(), tr)
		wantCode(t, err, CodeInvalidGrant) // burned by the downgrade attempt
	})
	t.Run("a manual app without PKCE redeems with its secret", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		mustNoErr(t, err)
		if tr.Scope != JoinScopes(NormalizeScopes(AdvertisedScopes)) {
			t.Errorf("scope = %q", tr.Scope)
		}
	})
	t.Run("the grant belongs to the approver and the chosen organization", func(t *testing.T) {
		fx := newSvc(t)
		var asked struct {
			user string
			org  int64
		}
		fx.setAdmit(func(u string, o int64) error { asked.user, asked.org = u, o; return nil })
		ra := fx.register()
		req := authorizeReq(&ra.App)
		_, code := fx.approve(fx.start(req), testUserB, testOrgOther)
		tr, err := fx.svc.Exchange(fx.ctx(), codeRequest(ra, req, code))
		mustNoErr(t, err)
		info, _ := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken)
		if info.UserID != testUserB || info.OrgID != testOrgOther || asked.user != testUserB || asked.org != testOrgOther {
			t.Errorf("grant of %s/%d; Admit asked about %s/%d", info.UserID, info.OrgID, asked.user, asked.org)
		}
	})
	t.Run("an expired code", func(t *testing.T) {
		fx := newSvc(t)
		c := newRedeem(fx, nil)
		fx.clock.Advance(AuthCodeTTL) // the code is valid while now < code_expires_at
		wantCode(t, c.redeem(nil), CodeInvalidGrant)
	})
	t.Run("a code just before it expires", func(t *testing.T) {
		fx := newSvc(t)
		c := newRedeem(fx, nil)
		fx.clock.Advance(AuthCodeTTL - time.Nanosecond)
		mustNoErr(t, c.redeem(nil))
	})
	t.Run("narrowing the app between approval and redemption", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		id := fx.start(req)
		_, code := fx.approve(id, testUserA, testOrgAcme)
		_, err := fx.svc.UpdateApp(fx.ctx(), UpdateAppRequest{OrgID: testOrgAcme, AppID: app.ID, Actor: testUserA, Name: app.Name,
			Website: app.Website, Scopes: []string{ScopeProjectsRead}, RedirectURIs: app.RedirectURIs})
		mustNoErr(t, err)
		tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		mustNoErr(t, err)
		if tr.Scope != ScopeProjectsRead {
			t.Errorf("scope = %q, want the app's narrower one", tr.Scope)
		}
	})
	t.Run("an app that holds none of the approved scopes any more", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0], Scope: ScopeDatabaseWrite}
		id := fx.start(req)
		_, code := fx.approve(id, testUserA, testOrgAcme)
		_, err := fx.svc.UpdateApp(fx.ctx(), UpdateAppRequest{OrgID: testOrgAcme, AppID: app.ID, Actor: testUserA, Name: app.Name,
			Website: app.Website, Scopes: []string{ScopeProjectsRead}, RedirectURIs: app.RedirectURIs})
		mustNoErr(t, err)
		_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		wantCode(t, err, CodeInvalidGrant)
		if a, _ := fx.store.GetAuthorization(fx.ctx(), id); a.Status != StatusExchanged || a.GrantID != 0 {
			t.Errorf("authorization: %+v", a)
		}
	})
	t.Run("a deleted app", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		_, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: app.ID, Actor: testUserA})
		mustNoErr(t, err)
		_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		wantCode(t, err, CodeInvalidClient)
	})
}

// TestCodeAdmission: Admit decides whether a code becomes a grant.
func TestCodeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		admit  error
		code   string
		status int
		burned bool
	}{
		{"user removed", ErrNotAdmitted, CodeInvalidGrant, http.StatusBadRequest, true},
		{"not a member any more", ErrNotMember, CodeInvalidGrant, http.StatusBadRequest, true},
		{"admission could not be checked", errors.New("registry unreachable: password=hunter2"), CodeServerError, http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSvc(t)
			c := newRedeem(fx, nil)
			fx.setAdmit(func(string, int64) error { return tc.admit })
			err := c.redeem(nil)
			wantCode(t, err, tc.code)
			e := oauthErr(t, err)
			if e.HTTPStatus() != tc.status {
				t.Errorf("status = %d", e.HTTPStatus())
			}
			if strings.Contains(e.Description, "hunter2") || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("an internal error leaks into the answer: %v", err)
			}
			if gs, _ := fx.store.ListGrants(fx.ctx(), GrantFilter{}); len(gs) != 0 {
				t.Errorf("%d grants were created", len(gs))
			}
			if tc.burned {
				if c.status() != StatusExchanged {
					t.Errorf("status = %s, want burned", c.status())
				}
				return
			}
			// Nothing was burned: when the check works again, the code redeems.
			if c.status() != StatusApproved {
				t.Fatalf("status = %s, the code should survive a failed check", c.status())
			}
			fx.setAdmit(nil)
			mustNoErr(t, c.redeem(nil))
		})
	}
	t.Run("a service without Admit fails closed", func(t *testing.T) {
		fx := newSvc(t)
		c := newRedeem(fx, nil)
		fx.svc.Admit = nil
		wantCode(t, c.redeem(nil), CodeInvalidGrant)
	})
}

// TestAdmitRunsOutsideTheStoreTransaction: Admit reaches into other tables, so the Service must not
// call it while it holds the store's transaction (a pooled connection). The memory store holds its lock
// for the whole of WithCode and RotateRefresh, so a call back into the store from Admit would deadlock.
func TestAdmitRunsOutsideTheStoreTransaction(t *testing.T) {
	fx := newSvc(t)
	c := newRedeem(fx, nil)
	f := fx.grant(testUserA, testOrgAcme)
	fx.svc.Admit = func(ctx context.Context, userID string, orgID int64) error {
		_, err := fx.store.CountDynamicApps(ctx)
		return err
	}
	errc := make(chan error, 1)
	go func() {
		if _, err := fx.svc.Exchange(fx.ctx(), c.request()); err != nil {
			errc <- err
			return
		}
		_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))
		errc <- err
	}()
	select {
	case err := <-errc:
		mustNoErr(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Exchange hangs: Admit runs inside a store transaction")
	}
}

// TestCodeReplayRevokesGrant is C3.
func TestCodeReplayRevokesGrant(t *testing.T) {
	fx := newSvc(t)
	c := newRedeem(fx, nil)
	first, err := fx.svc.Exchange(fx.ctx(), c.request())
	mustNoErr(t, err)
	if _, err := fx.svc.LookupAccess(fx.ctx(), first.AccessToken); err != nil {
		t.Fatalf("the issued token does not work: %v", err)
	}

	_, err = fx.svc.Exchange(fx.ctx(), c.request())
	wantCode(t, err, CodeInvalidGrant)

	// The token the code created is dead, and so is its refresh token.
	if _, err := fx.svc.LookupAccess(fx.ctx(), first.AccessToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("the access token survives a replay: %v", err)
	}
	_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: c.ra.App.ID, ClientSecret: c.ra.ClientSecret, RefreshToken: first.RefreshToken})
	wantCode(t, err, CodeInvalidGrant)
	info, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(first.AccessToken))
	g, _ := fx.store.GetGrant(fx.ctx(), info.GrantID)
	if g.RevokedAt == nil || g.RevokedReason != ReasonCodeReuse {
		t.Errorf("grant: %+v", g)
	}

	// An event, a revocation event, and one alert for the grant; a third attempt adds nothing.
	if ev := fx.eventsOf(EventCodeReuse); len(ev) != 1 || ev[0].Payload["grant_id"] != g.ID || ev[0].Payload["app_id"] != c.ra.App.ID {
		t.Errorf("code_reuse events: %v", ev)
	}
	if ev := fx.eventsOf(EventGrantRevoked); len(ev) != 1 || ev[0].Payload["reason"] != ReasonCodeReuse || ev[0].Payload["actor"] != ActorSystem || ev[0].Payload["org_slug"] != "acme" {
		t.Errorf("grant_revoked events: %v", ev)
	}
	al := fx.alertList()
	if len(al) != 1 || al[0].Kind != AlertKindTokenReuse || al[0].Key != AlertKindTokenReuse+"/"+strconv.FormatInt(g.ID, 10) {
		t.Fatalf("alerts: %+v", al)
	}
	_, _ = fx.svc.Exchange(fx.ctx(), c.request())
	if len(fx.alertList()) != 1 || len(fx.eventsOf(EventCodeReuse)) != 1 {
		t.Errorf("a repeated replay raised more: %d alerts, %d events", len(fx.alertList()), len(fx.eventsOf(EventCodeReuse)))
	}
	if strings.Contains(al[0].Title+al[0].Detail, c.code) {
		t.Errorf("the alert carries the code: %+v", al[0])
	}
}

// TestCodeSingleUseConcurrent is C2 against the memory store: of N concurrent redemptions one issues tokens.
func TestCodeSingleUseConcurrent(t *testing.T) {
	fx := newSvc(t)
	c := newRedeem(fx, nil)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := fx.svc.Exchange(fx.ctx(), c.request()); {
			case err == nil:
				wins.Add(1)
			case oauthErr(t, err).Code != CodeInvalidGrant:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d redemptions succeeded", wins.Load())
	}
}

func TestExchangeParameters(t *testing.T) {
	fx := newSvc(t)
	f := fx.grant(testUserA, testOrgAcme)
	ra := f.App
	tests := []struct {
		name string
		req  TokenRequest
		code string
	}{
		{"no grant_type", TokenRequest{ClientID: ra.App.ID}, CodeInvalidRequest},
		{"jwt-bearer", TokenRequest{GrantType: GrantTypeJWTBearer, ClientID: ra.App.ID, Assertion: "x.y.z"}, CodeUnsupportedGrantType},
		{"password", TokenRequest{GrantType: "password", ClientID: ra.App.ID}, CodeUnsupportedGrantType},
		{"client_credentials", TokenRequest{GrantType: "client_credentials", ClientID: ra.App.ID, ClientSecret: ra.ClientSecret}, CodeUnsupportedGrantType},
		{"no client_id", TokenRequest{GrantType: GrantTypeAuthorizationCode, Code: "x"}, CodeInvalidRequest},
		{"malformed client_id", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: "x", Code: "x"}, CodeInvalidClient},
		{"unknown client_id", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: secrets.NewUUID(), Code: "x", RedirectURI: "x"}, CodeInvalidClient},
		{"code flow without a code", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, RedirectURI: f.Req.RedirectURI}, CodeInvalidRequest},
		{"code flow without a redirect_uri", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: f.Code}, CodeInvalidRequest},
		{"a code of the wrong shape", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: "sbc_short", RedirectURI: f.Req.RedirectURI}, CodeInvalidGrant},
		{"an unknown code", TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: secrets.NewAuthCode(), RedirectURI: f.Req.RedirectURI, CodeVerifier: testVerifier}, CodeInvalidGrant},
		{"refresh without a token", TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, ClientSecret: ra.ClientSecret}, CodeInvalidRequest},
		{"a refresh token of the wrong shape", TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, ClientSecret: ra.ClientSecret, RefreshToken: "sbr_x"}, CodeInvalidGrant},
		{"an unknown refresh token", TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, ClientSecret: ra.ClientSecret, RefreshToken: secrets.NewOAuthRefreshToken()}, CodeInvalidGrant},
		{"an access token as a refresh token", TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, ClientSecret: ra.ClientSecret, RefreshToken: f.Tokens.AccessToken}, CodeInvalidGrant},
		{"a code as a refresh token", TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, ClientSecret: ra.ClientSecret, RefreshToken: f.Code}, CodeInvalidGrant},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.svc.Exchange(fx.ctx(), tc.req)
			wantCode(t, err, tc.code)
			e := oauthErr(t, err)
			if want := StatusFor(tc.code); e.HTTPStatus() != want {
				t.Errorf("status = %d, want %d", e.HTTPStatus(), want)
			}
		})
	}
}

// TestExchangeClientAuth is T1 at the service: who may redeem and refresh without a secret.
func TestExchangeClientAuth(t *testing.T) {
	t.Run("wrong, malformed and foreign secrets", func(t *testing.T) {
		fx := newSvc(t)
		a, b := fx.register(), fx.register()
		for name, secret := range map[string]string{
			"another app's secret": b.ClientSecret, "a random secret": secrets.NewClientSecret(),
			"a malformed secret": "sba_x", "a PAT": secrets.NewPAT(), "its hash": string(secrets.HashToken(a.ClientSecret)),
		} {
			c := newRedeemFor(fx, a)
			tr := c.request()
			tr.ClientSecret = secret
			_, err := fx.svc.Exchange(fx.ctx(), tr)
			wantCode(t, err, CodeInvalidClient)
			if oauthErr(t, err).HTTPStatus() != http.StatusUnauthorized {
				t.Errorf("%s: status %d", name, oauthErr(t, err).HTTPStatus())
			}
			if c.status() != StatusApproved {
				t.Errorf("%s: a failed client authentication burned the code", name)
			}
		}
	})
	t.Run("a dynamic app redeems with a correct secret or with none", func(t *testing.T) {
		fx := newSvc(t)
		ra := fx.register()
		c := newRedeemFor(fx, ra)
		tr := c.request()
		tr.ClientSecret = ra.ClientSecret
		_, err := fx.svc.Exchange(fx.ctx(), tr)
		mustNoErr(t, err)
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), newRedeemFor(fx, ra).request()))) // no secret, valid PKCE
	})
	t.Run("a correct secret does not replace PKCE", func(t *testing.T) {
		fx := newSvc(t)
		ra := fx.register()
		c := newRedeemFor(fx, ra)
		tr := c.request()
		tr.ClientSecret, tr.CodeVerifier = ra.ClientSecret, strings.Repeat("B", 43)
		_, err := fx.svc.Exchange(fx.ctx(), tr)
		wantCode(t, err, CodeInvalidGrant)
	})
	t.Run("a manual app needs its secret", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0],
			CodeChallenge: pkceChallengeS256(testVerifier), CodeChallengeMethod: PKCEMethodS256}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		tr := TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, Code: code, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}
		_, err := fx.svc.Exchange(fx.ctx(), tr)
		wantCode(t, err, CodeInvalidClient) // PKCE alone is not enough for a confidential client
		tr.ClientSecret = secret
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), tr)))
	})
	t.Run("refresh without a secret: only for apps registered as public", func(t *testing.T) {
		fx := newSvc(t)
		for _, method := range []string{AuthMethodBasic, AuthMethodPost, AuthMethodNone} {
			ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{ClientName: "C", RedirectURIs: []string{"http://127.0.0.1/callback"}, TokenEndpointAuthMethod: method})
			mustNoErr(t, err)
			f := fx.grantFor(ra, testUserA, testOrgAcme)
			_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: ra.App.ID, RefreshToken: f.Tokens.RefreshToken})
			if method == AuthMethodNone {
				mustNoErr(t, err)
			} else {
				wantCode(t, err, CodeInvalidClient)
			}
			// With the secret it always works (the refresh token above was not used up by the refusal).
			if method != AuthMethodNone {
				mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))))
			}
		}
	})
	t.Run("a manual app never refreshes without its secret", func(t *testing.T) {
		fx := newSvc(t)
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		mustNoErr(t, err)
		_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: app.ID, RefreshToken: tr.RefreshToken})
		wantCode(t, err, CodeInvalidClient)
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: app.ID, ClientSecret: secret, RefreshToken: tr.RefreshToken})))
	})
	t.Run("a refresh token belongs to one app", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		other := fx.register()
		_, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: other.App.ID, ClientSecret: other.ClientSecret, RefreshToken: f.Tokens.RefreshToken})
		wantCode(t, err, CodeInvalidGrant)
		// The attempt neither used up the token nor revoked the grant.
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))))
	})
	t.Run("last_used_at of the secret", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))))
		ss, _ := fx.store.ListSecrets(fx.ctx(), f.App.App.ID)
		if len(ss) != 1 || ss[0].LastUsedAt == nil || !ss[0].LastUsedAt.Equal(fx.clock.Now()) {
			t.Fatalf("secrets: %+v", ss)
		}
	})
}

// newRedeemFor is newRedeem for an app that is registered already.
func newRedeemFor(fx *svcFixture, ra *RegisteredApp) *redeemCase {
	fx.t.Helper()
	req := authorizeReq(&ra.App)
	c := &redeemCase{fx: fx, ra: ra, req: req, id: fx.start(req)}
	_, c.code = fx.approve(c.id, testUserA, testOrgAcme)
	return c
}

// TestRefreshRotation is T2: a refresh issues a new pair and the old token is single use, with a grace
// window for two processes of one client.
func TestRefreshRotation(t *testing.T) {
	fx := newSvc(t)
	f := fx.grant(testUserA, testOrgAcme)
	r0 := f.Tokens.RefreshToken

	fx.clock.Advance(20 * time.Minute)
	t1, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(r0))
	mustNoErr(t, err)
	if t1.RefreshToken == r0 || t1.AccessToken == f.Tokens.AccessToken || t1.Scope != f.Tokens.Scope || t1.ExpiresIn != 3600 || t1.TokenType != "Bearer" {
		t.Errorf("rotation: %+v", t1)
	}
	if !secrets.IsOAuthAccessToken(t1.AccessToken) || !secrets.IsOAuthRefreshToken(t1.RefreshToken) {
		t.Errorf("shapes: %q %q", t1.AccessToken, t1.RefreshToken)
	}
	// The old refresh token is stamped and points to its successor; its sliding expiry restarts.
	old, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(r0))
	nw, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(t1.RefreshToken))
	if old.UsedAt == nil || !old.UsedAt.Equal(fx.clock.Now()) || old.ReplacedBy != nw.ID || !nw.ExpiresAt.Equal(fx.clock.Now().Add(RefreshTokenTTL)) {
		t.Errorf("old %+v new %+v", old, nw)
	}
	// Both access tokens work until they expire (the first one for 40 more minutes).
	for _, tok := range []string{f.Tokens.AccessToken, t1.AccessToken} {
		if _, err := fx.svc.LookupAccess(fx.ctx(), tok); err != nil {
			t.Errorf("access token: %v", err)
		}
	}

	// Inside the grace window the old token gives another fresh pair (two processes refreshing at once).
	fx.clock.Advance(RefreshGrace - time.Second)
	t2, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(r0))
	mustNoErr(t, err)
	if t2.RefreshToken == t1.RefreshToken || t2.RefreshToken == r0 {
		t.Error("the second refresh inside the grace window repeats a token")
	}
	if len(fx.alertList()) != 0 {
		t.Errorf("alerts inside the grace window: %v", fx.alertList())
	}
	// ReplacedBy keeps the first successor.
	if again, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(r0)); again.ReplacedBy != nw.ID {
		t.Errorf("replaced_by = %d, want the first successor %d", again.ReplacedBy, nw.ID)
	}

	// The newest token rotates on.
	t3, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(t2.RefreshToken))
	mustNoErr(t, err)
	if _, err := fx.svc.LookupAccess(fx.ctx(), t3.AccessToken); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshReuse is T2 with the fake clock: a rotated token presented after the grace window kills the grant.
func TestRefreshReuse(t *testing.T) {
	fx := newSvc(t)
	f := fx.grant(testUserA, testOrgAcme)
	r0 := f.Tokens.RefreshToken
	t1, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(r0))
	mustNoErr(t, err)

	// Exactly at the edge of the window the token is a replay.
	fx.clock.Advance(RefreshGrace)
	_, err = fx.svc.Exchange(fx.ctx(), f.refreshRequest(r0))
	wantCode(t, err, CodeInvalidGrant)

	// The whole line is dead: the new refresh token and the access tokens.
	_, err = fx.svc.Exchange(fx.ctx(), f.refreshRequest(t1.RefreshToken))
	wantCode(t, err, CodeInvalidGrant)
	for _, tok := range []string{f.Tokens.AccessToken, t1.AccessToken} {
		if _, err := fx.svc.LookupAccess(fx.ctx(), tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("an access token survives the reuse: %v", err)
		}
	}
	info, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(r0))
	g, _ := fx.store.GetGrant(fx.ctx(), info.GrantID)
	if g.RevokedAt == nil || g.RevokedReason != ReasonRefreshReuse {
		t.Errorf("grant: %+v", g)
	}
	if ev := fx.eventsOf(EventRefreshReuse); len(ev) != 1 || ev[0].Payload["grant_id"] != g.ID {
		t.Errorf("refresh_reuse events: %v", ev)
	}
	if ev := fx.eventsOf(EventGrantRevoked); len(ev) != 1 || ev[0].Payload["reason"] != ReasonRefreshReuse || ev[0].Payload["actor"] != ActorSystem {
		t.Errorf("grant_revoked events: %v", ev)
	}
	al := fx.alertList()
	if len(al) != 1 || al[0].Kind != AlertKindTokenReuse || al[0].Key != AlertKindTokenReuse+"/"+strconv.FormatInt(g.ID, 10) {
		t.Fatalf("alerts: %+v", al)
	}
	if strings.Contains(al[0].Title+al[0].Detail, r0) {
		t.Errorf("the alert carries the token: %+v", al[0])
	}
	// Presenting it again changes nothing and raises nothing.
	_, _ = fx.svc.Exchange(fx.ctx(), f.refreshRequest(r0))
	if len(fx.alertList()) != 1 {
		t.Errorf("%d alerts", len(fx.alertList()))
	}
}

func TestRefreshExpiry(t *testing.T) {
	fx := newSvc(t)
	f := fx.grant(testUserA, testOrgAcme)

	// The access token lives one hour exactly.
	fx.clock.Advance(AccessTokenTTL - time.Nanosecond)
	if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); err != nil {
		t.Errorf("just before the hour: %v", err)
	}
	fx.clock.Advance(time.Nanosecond)
	if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("at the hour: %v", err)
	}
	// The refresh token slides: every rotation restarts the 90 days.
	cur := f.Tokens.RefreshToken
	for i := 0; i < 3; i++ {
		fx.clock.Advance(60 * 24 * time.Hour)
		tr, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(cur))
		if err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
		cur = tr.RefreshToken
	}
	// Left alone for 90 days it dies.
	fx.clock.Advance(RefreshTokenTTL)
	_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(cur))
	wantCode(t, err, CodeInvalidGrant)
	if len(fx.alertList()) != 0 {
		t.Errorf("an expired token is not a replay: %v", fx.alertList())
	}
}

func TestRefreshScopeAndResource(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	req := authorizeReq(&ra.App)
	req.Scope = "projects:read database:read database:write"
	req.Resource = testIssuer + "/mcp"
	_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
	tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: code, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier})
	mustNoErr(t, err)
	f := &grantFlow{App: ra, Tokens: tr}
	info, _ := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken)

	refresh := func(token string, mod func(*TokenRequest)) (*TokenResponse, error) {
		r := f.refreshRequest(token)
		if mod != nil {
			mod(&r)
		}
		return fx.svc.Exchange(fx.ctx(), r)
	}
	// A scope outside the grant, or one that is not a subset, is refused and the token stays usable.
	for _, scope := range []string{"organizations:read", "projects:read projects:write", "bogus"} {
		_, err := refresh(tr.RefreshToken, func(r *TokenRequest) { r.Scope = scope })
		wantCode(t, err, CodeInvalidScope)
	}
	// So is another resource.
	for _, res := range []string{testIssuer, "https://evil.example.test/mcp", "x"} {
		_, err := refresh(tr.RefreshToken, func(r *TokenRequest) { r.Resource = res })
		wantCode(t, err, CodeInvalidTarget)
	}
	// None of that consumed the token: the plain refresh works, and so does the matching resource.
	r1, err := refresh(tr.RefreshToken, func(r *TokenRequest) { r.Resource = testIssuer + "/mcp/" })
	mustNoErr(t, err)
	if r1.Scope != tr.Scope {
		t.Errorf("scope without a scope parameter = %q", r1.Scope)
	}

	// Narrowing is honored and permanent for the grant.
	r2, err := refresh(r1.RefreshToken, func(r *TokenRequest) { r.Scope = "projects:read  database:read" })
	mustNoErr(t, err)
	if r2.Scope != "database:read projects:read" {
		t.Errorf("narrowed scope = %q", r2.Scope)
	}
	got, _ := fx.svc.LookupAccess(fx.ctx(), r2.AccessToken)
	if !slices.Equal(got.Scopes, []string{ScopeDatabaseRead, ScopeProjectsRead}) || got.GrantID != info.GrantID {
		t.Errorf("narrowed access info: %+v", got)
	}
	_, err = refresh(r2.RefreshToken, func(r *TokenRequest) { r.Scope = "database:write" })
	wantCode(t, err, CodeInvalidScope) // cannot ask for the dropped scope back
	r3, err := refresh(r2.RefreshToken, nil)
	mustNoErr(t, err)
	if r3.Scope != "database:read projects:read" {
		t.Errorf("a later refresh without a scope = %q", r3.Scope)
	}
	// The old access token of the same grant is narrowed too: scopes are the grant's.
	if old, err := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken); err != nil || !slices.Equal(old.Scopes, got.Scopes) {
		t.Errorf("old access token after narrowing: %+v, %v", old, err)
	}
}

// TestRefreshAdmission: the user's standing is checked on every refresh.
func TestRefreshAdmission(t *testing.T) {
	t.Run("left the organization: the grant is revoked", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		fx.setAdmit(func(string, int64) error { return ErrNotMember })
		_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))
		wantCode(t, err, CodeInvalidGrant)
		if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
			t.Errorf("the access token survives: %v", err)
		}
		ev := fx.eventsOf(EventGrantRevoked)
		if len(ev) != 1 || ev[0].Payload["reason"] != ReasonMembership || ev[0].Payload["actor"] != ActorSystem {
			t.Errorf("events: %v", ev)
		}
	})
	t.Run("not admitted: refused, nothing revoked", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		fx.setAdmit(func(string, int64) error { return ErrNotAdmitted })
		_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))
		wantCode(t, err, CodeInvalidGrant)
		if len(fx.eventsOf(EventGrantRevoked)) != 0 {
			t.Error("a grant was revoked")
		}
	})
	t.Run("admission cannot be checked: server_error, the token is not used up", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		fx.setAdmit(func(string, int64) error { return errors.New("database is down") })
		_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))
		wantCode(t, err, CodeServerError)
		if oauthErr(t, err).HTTPStatus() != http.StatusInternalServerError || strings.Contains(oauthErr(t, err).Description, "database") {
			t.Errorf("answer: %v", err)
		}
		if !strings.Contains(fx.logbuf.String(), "database is down") {
			t.Errorf("the cause is not logged: %s", fx.logbuf.String())
		}
		fx.setAdmit(nil)
		mustNoErr(t, errOf(fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))))
	})
}

func TestLookupAccess(t *testing.T) {
	fx := newSvc(t)
	f := fx.grant(testUserA, testOrgAcme)
	for name, tok := range map[string]string{
		"empty": "", "garbage": "garbage", "a PAT": secrets.NewPAT(), "an unknown token": secrets.NewOAuthAccessToken(),
		"upper case": strings.ToUpper(f.Tokens.AccessToken), "a prefix": f.Tokens.AccessToken[:20],
		"with a suffix": f.Tokens.AccessToken + "0", "the refresh token": f.Tokens.RefreshToken,
	} {
		if _, err := fx.svc.LookupAccess(fx.ctx(), tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	info, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken)
	mustNoErr(t, err)
	if !info.ExpiresAt.Equal(fx.clock.Now().Add(AccessTokenTTL)) {
		t.Errorf("expires at %v", info.ExpiresAt)
	}
	if info.Scopes == nil || !slices.Equal(info.Scopes, NormalizeScopes(AdvertisedScopes)) {
		t.Errorf("scopes %v", info.Scopes)
	}

	// TouchAccess records the use on the token and the grant.
	fx.clock.Advance(5 * time.Minute)
	mustNoErr(t, fx.svc.TouchAccess(fx.ctx(), info.TokenID, info.GrantID))
	tok, _ := fx.store.GetToken(fx.ctx(), secrets.HashToken(f.Tokens.AccessToken))
	g, _ := fx.store.GetGrant(fx.ctx(), info.GrantID)
	if tok.LastUsedAt == nil || !tok.LastUsedAt.Equal(fx.clock.Now()) || g.LastUsedAt == nil || !g.LastUsedAt.Equal(fx.clock.Now()) {
		t.Errorf("last_used_at: token %v grant %v", tok.LastUsedAt, g.LastUsedAt)
	}
}

// TestSupersede: a new grant of the same app, user and organization replaces the old one, and a user
// keeps at most MaxLiveGrantsPerUserOrg live grants per organization.
func TestSupersede(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	first := fx.grantFor(ra, testUserA, testOrgAcme)
	other := fx.grantFor(ra, testUserB, testOrgAcme) // another user: untouched
	fx.clock.Advance(time.Minute)
	second := fx.grantFor(ra, testUserA, testOrgAcme)

	if _, err := fx.svc.LookupAccess(fx.ctx(), first.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old grant survives: %v", err)
	}
	for _, f := range []*grantFlow{second, other} {
		if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); err != nil {
			t.Errorf("a current grant died: %v", err)
		}
	}
	var superseded []svcEvent
	for _, e := range fx.eventsOf(EventGrantRevoked) {
		if e.Payload["reason"] == ReasonSuperseded {
			superseded = append(superseded, e)
		}
	}
	if len(superseded) != 1 || superseded[0].Payload["app_name"] != "Test Client" || superseded[0].Payload["org_slug"] != "acme" {
		t.Errorf("superseded events: %v", superseded)
	}

	// The cap: grants of many apps for one user and organization.
	var flows []*grantFlow
	for i := 0; i < MaxLiveGrantsPerUserOrg+3; i++ {
		fx.clock.Advance(time.Second)
		flows = append(flows, fx.grant(testUserB, testOrgOther))
	}
	live, _ := fx.store.ListGrants(fx.ctx(), GrantFilter{UserID: testUserB, OrgID: testOrgOther, Live: true})
	if len(live) != MaxLiveGrantsPerUserOrg {
		t.Fatalf("%d live grants", len(live))
	}
	for i, f := range flows {
		_, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken)
		if want := i >= 3; (err == nil) != want { // the three oldest were trimmed
			t.Errorf("flow %d: live=%v, want %v", i, err == nil, want)
		}
	}
	// The same user in another organization has their own allowance.
	if _, err := fx.svc.LookupAccess(fx.ctx(), other.Tokens.AccessToken); err != nil {
		t.Errorf("a grant in another organization was trimmed: %v", err)
	}
}
