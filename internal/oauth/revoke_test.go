package oauth

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// TestRevoke is T6: POST /v1/oauth/revoke needs the client, answers nothing for tokens that are not
// the client's, and revokes the whole grant.
func TestRevoke(t *testing.T) {
	t.Run("an access token or a refresh token ends the grant", func(t *testing.T) {
		for _, which := range []string{"access", "refresh"} {
			fx := newSvc(t)
			f := fx.grant(testUserA, testOrgAcme)
			tok := f.Tokens.AccessToken
			if which == "refresh" {
				tok = f.Tokens.RefreshToken
			}
			mustNoErr(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: tok, TokenTypeHint: "refresh_token"}))
			if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: the access token survives: %v", which, err)
			}
			_, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(f.Tokens.RefreshToken))
			wantCode(t, err, CodeInvalidGrant)
			ev := fx.eventsOf(EventGrantRevoked)
			if len(ev) != 1 || ev[0].Payload["reason"] != ReasonClient || ev[0].Payload["actor"] != "app:"+f.App.App.ID || ev[0].Payload["org_slug"] != "acme" {
				t.Errorf("%s: events %v", which, ev)
			}
			// Revoking again is quiet.
			mustNoErr(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: tok}))
			if n := len(fx.eventsOf(EventGrantRevoked)); n != 1 {
				t.Errorf("%s: %d revocation events", which, n)
			}
		}
	})
	t.Run("tokens that are unknown, malformed or another app's change nothing", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		other := fx.register()
		for name, req := range map[string]RevokeRequest{
			"unknown access token":  {ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: secrets.NewOAuthAccessToken()},
			"unknown refresh token": {ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: secrets.NewOAuthRefreshToken()},
			"garbage":               {ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: "garbage"},
			"a PAT":                 {ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: secrets.NewPAT()},
			"a code":                {ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, Token: f.Code},
			"another app's token":   {ClientID: other.App.ID, ClientSecret: other.ClientSecret, Token: f.Tokens.AccessToken},
			"another app's refresh": {ClientID: other.App.ID, ClientSecret: other.ClientSecret, Token: f.Tokens.RefreshToken},
		} {
			if err := fx.svc.Revoke(fx.ctx(), req); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); err != nil {
			t.Errorf("the grant died: %v", err)
		}
		if len(fx.eventsOf(EventGrantRevoked)) != 0 {
			t.Error("a revocation was recorded")
		}
	})
	t.Run("client credentials", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		other := fx.register()
		tok := f.Tokens.AccessToken
		for name, req := range map[string]RevokeRequest{
			"wrong secret":         {ClientID: f.App.App.ID, ClientSecret: other.ClientSecret, Token: tok},
			"malformed secret":     {ClientID: f.App.App.ID, ClientSecret: "x", Token: tok},
			"no secret (not none)": {ClientID: f.App.App.ID, Token: tok},
			"unknown client":       {ClientID: newUUID(), ClientSecret: f.App.ClientSecret, Token: tok},
			"malformed client":     {ClientID: "x", ClientSecret: f.App.ClientSecret, Token: tok},
		} {
			err := fx.svc.Revoke(fx.ctx(), req)
			wantCode(t, err, CodeInvalidClient)
			if oauthErr(t, err).HTTPStatus() != http.StatusUnauthorized {
				t.Errorf("%s: status %d", name, oauthErr(t, err).HTTPStatus())
			}
		}
		wantCode(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientSecret: f.App.ClientSecret, Token: tok}), CodeInvalidRequest)
		wantCode(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret}), CodeInvalidRequest)
		if _, err := fx.svc.LookupAccess(fx.ctx(), tok); err != nil {
			t.Errorf("a refused revocation ended the grant: %v", err)
		}
	})
	t.Run("a public app revokes without a secret, a manual app never does", func(t *testing.T) {
		fx := newSvc(t)
		ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{ClientName: "C", RedirectURIs: []string{"http://127.0.0.1/callback"}, TokenEndpointAuthMethod: AuthMethodNone})
		mustNoErr(t, err)
		f := fx.grantFor(ra, testUserA, testOrgAcme)
		mustNoErr(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: ra.App.ID, Token: f.Tokens.AccessToken}))
		if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
			t.Errorf("the grant survives: %v", err)
		}
		app, secret := fx.manual(testOrgAcme)
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		mustNoErr(t, err)
		wantCode(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: app.ID, Token: tr.AccessToken}), CodeInvalidClient)
		mustNoErr(t, fx.svc.Revoke(fx.ctx(), RevokeRequest{ClientID: app.ID, ClientSecret: secret, Token: tr.AccessToken}))
	})
}

// TestRevokeGrants covers the operator and hook entry points: RevokeGrant, RevokeGrants, RevokeUser, RevokeApp.
func TestRevokeGrants(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	a1 := fx.grantFor(ra, testUserA, testOrgAcme)
	a2 := fx.grantFor(ra, testUserA, testOrgOther)
	b1 := fx.grantFor(ra, testUserB, testOrgAcme)
	other := fx.grant(testUserA, testOrgAcme)
	live := func(f *grantFlow) bool {
		_, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken)
		return err == nil
	}
	info := func(f *grantFlow) *AccessInfo {
		i, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	// Reasons are checked, filters that select everything need All, ids must exist.
	if err := fx.svc.RevokeGrant(fx.ctx(), info(b1).GrantID, "because", ActorOperator); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown reason: %v", err)
	}
	if err := fx.svc.RevokeGrant(fx.ctx(), 0, ReasonOperator, ActorOperator); !errors.Is(err, ErrNotFound) {
		t.Errorf("grant 0: %v", err)
	}
	if err := fx.svc.RevokeGrant(fx.ctx(), 9999, ReasonOperator, ActorOperator); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing grant: %v", err)
	}
	if _, err := fx.svc.RevokeGrants(fx.ctx(), GrantFilter{Live: true, Limit: 5}, ReasonOperator, ActorOperator); !errors.Is(err, ErrInvalid) {
		t.Errorf("a filter that selects everything without All: %v", err)
	}
	if _, err := fx.svc.RevokeUser(fx.ctx(), "", ReasonUserRemoved, ActorSystem); !errors.Is(err, ErrInvalid) {
		t.Errorf("RevokeUser without a user: %v", err)
	}
	if _, err := fx.svc.RevokeUser(fx.ctx(), testUserA, "nope", ActorSystem); !errors.Is(err, ErrInvalid) {
		t.Errorf("RevokeUser with a bad reason: %v", err)
	}
	if n, err := fx.svc.RevokeGrants(fx.ctx(), GrantFilter{AppID: "not-a-uuid"}, ReasonOperator, ActorOperator); err != nil || n != 0 {
		t.Errorf("a malformed app id selects nothing: %d, %v", n, err)
	}
	for _, f := range []*grantFlow{a1, a2, b1, other} {
		if !live(f) {
			t.Fatal("a refused call revoked a grant")
		}
	}

	// One grant.
	mustNoErr(t, fx.svc.RevokeGrant(fx.ctx(), info(b1).GrantID, ReasonOperator, ActorOperator))
	if live(b1) || !live(a1) {
		t.Error("RevokeGrant revoked the wrong grants")
	}

	// A user, in every organization.
	n, err := fx.svc.RevokeUser(fx.ctx(), testUserA, ReasonUserRemoved, ActorSystem)
	mustNoErr(t, err)
	if n != 3 || live(a1) || live(a2) || live(other) {
		t.Errorf("RevokeUser revoked %d", n)
	}
	n, err = fx.svc.RevokeUser(fx.ctx(), testUserA, ReasonUserRemoved, ActorSystem)
	if err != nil || n != 0 {
		t.Errorf("a user with no live grant: %d, %v", n, err)
	}
	var removed int
	for _, e := range fx.eventsOf(EventGrantRevoked) {
		if e.Payload["reason"] == ReasonUserRemoved {
			removed++
			if e.Payload["actor"] != ActorSystem || e.Payload["user_id"] != testUserA || e.Payload["app_name"] == nil || e.Payload["org_slug"] == nil {
				t.Errorf("event payload: %v", e.Payload)
			}
		}
	}
	if removed != 3 {
		t.Errorf("%d user_removed events", removed)
	}

	// Every live grant, with All.
	g1 := fx.grantFor(ra, testUserA, testOrgAcme)
	g2 := fx.grant(testUserB, testOrgOther)
	n, err = fx.svc.RevokeGrants(fx.ctx(), GrantFilter{All: true}, ReasonOperator, ActorOperator)
	mustNoErr(t, err)
	if n != 2 || live(g1) || live(g2) {
		t.Errorf("revoking all: %d", n)
	}
}

func TestRevokeApp(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	g1 := fx.grantFor(ra, testUserA, testOrgAcme)
	fx.clock.Advance(time.Hour)
	t1 := fx.clock.Now()
	g2 := fx.grantFor(ra, testUserB, testOrgAcme)
	g3 := fx.grantFor(ra, testUserA, testOrgOther)
	bystander := fx.grant(testUserA, testOrgAcme)

	if _, err := fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ra.App.ID, OrgID: testOrgAcme, Reason: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad reason: %v", err)
	}
	for _, id := range []string{newUUID(), "nope", ""} {
		if _, err := fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: id, Reason: ReasonAdmin}); !errors.Is(err, ErrNotFound) {
			t.Errorf("RevokeApp(%q): %v", id, err)
		}
	}

	// In one organization: the app's grants of the others stay.
	res, err := fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ra.App.ID, OrgID: testOrgAcme, Reason: ReasonAdmin, Actor: testUserA})
	mustNoErr(t, err)
	if res.Revoked != 2 || res.App.ID != ra.App.ID || res.App.Name != "Test Client" || !res.AuthorizedAt.Equal(t1) {
		t.Errorf("result: revoked %d authorized %v (want %v), app %+v", res.Revoked, res.AuthorizedAt, t1, res.App.ID)
	}
	for _, f := range []*grantFlow{g1, g2} {
		if _, err := fx.svc.LookupAccess(fx.ctx(), f.Tokens.AccessToken); !errors.Is(err, ErrNotFound) {
			t.Errorf("a grant of the app survives in the organization: %v", err)
		}
	}
	if _, err := fx.svc.LookupAccess(fx.ctx(), g3.Tokens.AccessToken); err != nil {
		t.Errorf("a grant in another organization was revoked: %v", err)
	}
	if _, err := fx.svc.LookupAccess(fx.ctx(), bystander.Tokens.AccessToken); err != nil {
		t.Errorf("another app's grant was revoked: %v", err)
	}
	for _, e := range fx.eventsOf(EventGrantRevoked) {
		if e.Payload["reason"] != ReasonAdmin || e.Payload["actor"] != testUserA {
			t.Errorf("event payload: %v", e.Payload)
		}
	}
	// An app with no live grant in the scope: no error, nothing revoked, no time.
	res, err = fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ra.App.ID, OrgID: testOrgAcme, Reason: ReasonAdmin})
	if err != nil || res.Revoked != 0 || !res.AuthorizedAt.IsZero() {
		t.Errorf("second call: %+v, %v", res, err)
	}
	// All organizations at once.
	res, err = fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ra.App.ID, Reason: ReasonOperator, Actor: ActorOperator})
	if err != nil || res.Revoked != 1 {
		t.Errorf("all organizations: %+v, %v", res, err)
	}
}

func TestListGrants(t *testing.T) {
	fx := newSvc(t)
	ra := fx.register()
	fx.grantFor(ra, testUserA, testOrgAcme)
	fx.clock.Advance(time.Minute)
	fx.grantFor(ra, testUserB, testOrgOther)
	list, err := fx.svc.ListGrants(fx.ctx(), GrantFilter{AppID: ra.App.ID})
	mustNoErr(t, err)
	if len(list) != 2 || list[0].Grant.UserID != testUserB || list[0].OrgSlug != "other" || list[0].App.Name != "Test Client" || list[1].OrgSlug != "acme" {
		t.Errorf("listing: %+v", list)
	}
	if got, err := fx.svc.ListGrants(fx.ctx(), GrantFilter{AppID: "nope"}); err != nil || len(got) != 0 {
		t.Errorf("a malformed app id: %v, %v", got, err)
	}
	if got, _ := fx.svc.ListGrants(fx.ctx(), GrantFilter{Limit: 1}); len(got) != 1 {
		t.Errorf("limit: %d", len(got))
	}
}

func TestListAuthorizedApps(t *testing.T) {
	fx := newSvc(t)
	if _, err := fx.svc.ListAuthorizedApps(fx.ctx(), 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("organization 0 would list every organization: %v", err)
	}
	ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{
		ClientName: "Dyn", ClientURI: "https://dyn.example.test", LogoURI: "https://dyn.example.test/logo.png",
		RedirectURIs: []string{"http://127.0.0.1/callback"}, Scope: "projects:read database:read",
	})
	mustNoErr(t, err)
	first := fx.clock.Now()
	fx.grantFor(ra, testUserA, testOrgAcme)
	fx.clock.Advance(time.Hour)
	newest := fx.clock.Now()
	fx.grantFor(ra, testUserB, testOrgAcme)
	fx.grantFor(ra, testUserB, testOrgOther) // another organization
	manual, secret := fx.manual(testOrgAcme)
	fx.clock.Advance(time.Hour)
	req := AuthorizeRequest{ClientID: manual.ID, ResponseType: ResponseTypeCode, RedirectURI: manual.RedirectURIs[0], Scope: "projects:read organizations:read"}
	_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
	_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: manual.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
	mustNoErr(t, err)
	manualAt := fx.clock.Now()

	list, err := fx.svc.ListAuthorizedApps(fx.ctx(), testOrgAcme)
	mustNoErr(t, err)
	if len(list) != 2 {
		t.Fatalf("%d apps: %+v", len(list), list)
	}
	// Newest authorization first.
	m, d := list[0], list[1]
	if m.App.ID != manual.ID || !m.AuthorizedAt.Equal(manualAt) || m.FirstApprover != testUserA ||
		!slices.Equal(m.Scopes, []string{ScopeOrganizationsRead, ScopeProjectsRead}) || m.App.Website != "https://app.example.test" {
		t.Errorf("manual entry: %+v", m)
	}
	if d.App.ID != ra.App.ID || !d.AuthorizedAt.Equal(newest) || d.FirstApprover != testUserA || !slices.Equal(d.Scopes, []string{ScopeDatabaseRead, ScopeProjectsRead}) {
		t.Errorf("dynamic entry: %+v (first %v)", d, first)
	}
	// What a dynamic app asserted about itself is not returned.
	if d.App.Website != "" || d.App.Icon != "" || d.App.RegistrationType != RegistrationDynamic {
		t.Errorf("dynamic entry shows %q / %q", d.App.Website, d.App.Icon)
	}
	// The other organization sees its own.
	other, _ := fx.svc.ListAuthorizedApps(fx.ctx(), testOrgOther)
	if len(other) != 1 || other[0].App.ID != ra.App.ID {
		t.Errorf("other organization: %+v", other)
	}
	// Revoked and deleted apps drop out.
	_, err = fx.svc.RevokeApp(fx.ctx(), RevokeAppRequest{AppID: ra.App.ID, OrgID: testOrgAcme, Reason: ReasonAdmin})
	mustNoErr(t, err)
	_, err = fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: manual.ID, Actor: testUserA})
	mustNoErr(t, err)
	if list, _ := fx.svc.ListAuthorizedApps(fx.ctx(), testOrgAcme); len(list) != 0 {
		t.Errorf("after revoking and deleting: %+v", list)
	}
}
