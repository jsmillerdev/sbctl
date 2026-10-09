package oauth

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

func createReq(org int64) CreateAppRequest {
	return CreateAppRequest{
		OrgID: org, CreatedBy: testUserA, Name: "Reporting", Website: "https://reports.example.test", Icon: "https://reports.example.test/icon.png",
		Scopes: []string{ScopeProjectsRead, ScopeDatabaseRead, ScopeProjectsRead}, RedirectURIs: []string{"https://reports.example.test/callback"},
	}
}

func TestCreateApp(t *testing.T) {
	fx := newSvc(t)
	ca, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	mustNoErr(t, err)

	if ca.App.RegistrationType != RegistrationManual || ca.App.OrgID != testOrgAcme || ca.App.CreatedBy != testUserA || ca.App.Name != "Reporting" ||
		ca.App.TokenEndpointAuthMethod != AuthMethodBasic || !slices.Equal(ca.App.Scopes, []string{ScopeDatabaseRead, ScopeProjectsRead}) {
		t.Errorf("app: %+v", ca.App)
	}
	if _, ok := canonUUID(ca.App.ID); !ok {
		t.Errorf("app id %q", ca.App.ID)
	}
	if !secrets.IsClientSecret(ca.ClientSecret) || ca.Secret.Alias != secrets.ClientSecretAlias(ca.ClientSecret) || ca.Secret.Hash != nil || ca.Secret.AppID != ca.App.ID || ca.Secret.CreatedBy != testUserA {
		t.Errorf("secret: %+v plaintext %q", ca.Secret, ca.ClientSecret)
	}
	if _, ok := canonUUID(ca.Secret.ID); !ok {
		t.Errorf("secret id %q", ca.Secret.ID)
	}
	// Stored as a hash.
	ss, _ := fx.store.ListSecrets(fx.ctx(), ca.App.ID)
	if len(ss) != 1 || string(ss[0].Hash) != string(secrets.HashToken(ca.ClientSecret)) || ss[0].ID != ca.Secret.ID {
		t.Errorf("stored secrets: %+v", ss)
	}
	if ev := fx.eventsOf(EventAppCreated); len(ev) != 1 || ev[0].Payload["app_id"] != ca.App.ID || ev[0].Payload["user_id"] != testUserA || ev[0].Payload["org_id"] != testOrgAcme {
		t.Errorf("app_created: %v", ev)
	}
	if ev := fx.eventsOf(EventClientSecretCreated); len(ev) != 1 || ev[0].Payload["secret_id"] != ca.Secret.ID {
		t.Errorf("client_secret_created: %v", ev)
	}

	list, err := fx.svc.ListPublishedApps(fx.ctx(), testOrgAcme)
	mustNoErr(t, err)
	if len(list) != 1 || list[0].ID != ca.App.ID {
		t.Errorf("published: %+v", list)
	}
	if other, _ := fx.svc.ListPublishedApps(fx.ctx(), testOrgOther); len(other) != 0 {
		t.Errorf("another organization lists %+v", other)
	}
	if _, err := fx.svc.ListPublishedApps(fx.ctx(), 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("organization 0: %v", err)
	}
}

func TestCreateAppValidation(t *testing.T) {
	fx := newSvc(t)
	tests := []struct {
		name string
		mod  func(*CreateAppRequest)
		ok   bool
	}{
		{"valid", func(r *CreateAppRequest) {}, true},
		{"all 24 scopes", func(r *CreateAppRequest) { r.Scopes = AllScopes }, true},
		{"no website, no icon", func(r *CreateAppRequest) { r.Website, r.Icon = "", "" }, true},
		{"http website", func(r *CreateAppRequest) { r.Website = "http://reports.example.test" }, true},
		{"no organization", func(r *CreateAppRequest) { r.OrgID = 0 }, false},
		{"created_by that is not a user id", func(r *CreateAppRequest) { r.CreatedBy = "root" }, false},
		{"no name", func(r *CreateAppRequest) { r.Name = "" }, false},
		{"name of control characters", func(r *CreateAppRequest) { r.Name = "‮\x00" }, false},
		{"name of 101 characters", func(r *CreateAppRequest) { r.Name = strings.Repeat("n", 101) }, false},
		{"javascript website", func(r *CreateAppRequest) { r.Website = "javascript:alert(1)" }, false},
		{"data icon", func(r *CreateAppRequest) { r.Icon = "data:image/png;base64,AAAA" }, false},
		{"no scopes", func(r *CreateAppRequest) { r.Scopes = nil }, false},
		{"unknown scope", func(r *CreateAppRequest) { r.Scopes = []string{"projects:read", "root:everything"} }, false},
		{"no redirect URIs", func(r *CreateAppRequest) { r.RedirectURIs = nil }, false},
		{"custom scheme redirect", func(r *CreateAppRequest) { r.RedirectURIs = []string{"myapp://callback"} }, false},
		{"http redirect to a remote host", func(r *CreateAppRequest) { r.RedirectURIs = []string{"http://reports.example.test/callback"} }, false},
		{"fragment redirect", func(r *CreateAppRequest) { r.RedirectURIs = []string{"https://reports.example.test/callback#x"} }, false},
	}
	n := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := createReq(testOrgAcme)
			tc.mod(&req)
			_, err := fx.svc.CreateApp(fx.ctx(), req)
			if tc.ok {
				mustNoErr(t, err)
				n++
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if msg := ErrorMessage(err); msg == "" || strings.HasPrefix(msg, "oauth:") {
				t.Errorf("message %q", msg)
			}
		})
	}
	// Refused requests created nothing, and not even an audit event.
	if got, _ := fx.store.CountManualApps(fx.ctx(), testOrgAcme); got != n {
		t.Errorf("%d apps stored, %d accepted", got, n)
	}
	if len(fx.eventsOf(EventAppCreated)) != n {
		t.Errorf("%d app_created events", len(fx.eventsOf(EventAppCreated)))
	}
}

func TestCreateAppLimit(t *testing.T) {
	fx := newSvc(t)
	for i := 0; i < MaxManualAppsPerOrg; i++ {
		_, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
		mustNoErr(t, err)
	}
	_, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("the 21st app: %v", err)
	}
	// Another organization has its own allowance, and a deleted app frees a slot.
	_, err = fx.svc.CreateApp(fx.ctx(), createReq(testOrgOther))
	mustNoErr(t, err)
	apps, _ := fx.svc.ListPublishedApps(fx.ctx(), testOrgAcme)
	_, err = fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: apps[0].ID, Actor: testUserA})
	mustNoErr(t, err)
	_, err = fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	mustNoErr(t, err)
}

func TestUpdateAndDeleteApp(t *testing.T) {
	fx := newSvc(t)
	ca, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	mustNoErr(t, err)
	app := ca.App
	fx.clock.Advance(time.Hour)

	// A grant of the app, to see the effect of narrowing and of deletion.
	req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
	_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
	tr, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: ca.ClientSecret, Code: code, RedirectURI: req.RedirectURI})
	mustNoErr(t, err)

	up := UpdateAppRequest{OrgID: testOrgAcme, AppID: app.ID, Actor: testUserA, Name: "Reporting v2", Website: "https://v2.example.test",
		Scopes: []string{ScopeProjectsRead}, RedirectURIs: []string{"https://v2.example.test/callback", "http://localhost/callback"}}
	got, err := fx.svc.UpdateApp(fx.ctx(), up)
	mustNoErr(t, err)
	if got.Name != "Reporting v2" || got.Icon != "" || got.Website != "https://v2.example.test" || !slices.Equal(got.Scopes, []string{ScopeProjectsRead}) ||
		!slices.Equal(got.RedirectURIs, up.RedirectURIs) || !got.UpdatedAt.Equal(fx.clock.Now()) {
		t.Errorf("updated app: %+v", got)
	}
	if got.RegistrationType != RegistrationManual || got.OrgID != testOrgAcme || got.CreatedBy != testUserA || !got.CreatedAt.Equal(app.CreatedAt) {
		t.Errorf("an update changed what it must not: %+v", got)
	}
	// Existing tokens keep working, narrowed to the app's new scopes.
	info, err := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken)
	mustNoErr(t, err)
	if !slices.Equal(info.Scopes, []string{ScopeProjectsRead}) {
		t.Errorf("scopes after narrowing: %v", info.Scopes)
	}
	if ev := fx.eventsOf(EventAppUpdated); len(ev) != 1 || ev[0].Payload["user_id"] != testUserA || ev[0].Payload["app_name"] != "Reporting v2" {
		t.Errorf("app_updated: %v", ev)
	}
	// Validation applies, and a refused update changes nothing.
	bad := up
	bad.Scopes = []string{"bogus"}
	if _, err := fx.svc.UpdateApp(fx.ctx(), bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid update: %v", err)
	}
	if again, _ := fx.store.GetApp(fx.ctx(), app.ID); again.Name != "Reporting v2" {
		t.Errorf("a refused update changed the app: %+v", again)
	}

	// Another organization, a dynamic app, a missing id and a malformed id are all "not found".
	dyn := fx.register()
	for name, r := range map[string]UpdateAppRequest{
		"another organization": {OrgID: testOrgOther, AppID: app.ID},
		"a dynamic app":        {OrgID: testOrgAcme, AppID: dyn.App.ID},
		"a missing id":         {OrgID: testOrgAcme, AppID: secrets.NewUUID()},
		"a malformed id":       {OrgID: testOrgAcme, AppID: "x"},
		"organization 0":       {OrgID: 0, AppID: app.ID},
	} {
		r.Name, r.Scopes, r.RedirectURIs = "N", []string{ScopeProjectsRead}, []string{"https://x.example.test/cb"}
		if _, err := fx.svc.UpdateApp(fx.ctx(), r); !errors.Is(err, ErrNotFound) {
			t.Errorf("update of %s: %v", name, err)
		}
		if _, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: r.OrgID, AppID: r.AppID, Actor: testUserA}); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete of %s: %v", name, err)
		}
	}

	// Delete: the app is gone and its grants are revoked.
	del, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: app.ID, Actor: testUserA})
	mustNoErr(t, err)
	if del.ID != app.ID || del.Name != "Reporting v2" {
		t.Errorf("deleted app: %+v", del)
	}
	if _, err := fx.svc.LookupAccess(fx.ctx(), tr.AccessToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("a token of a deleted app: %v", err)
	}
	_, err = fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: app.ID, ClientSecret: ca.ClientSecret, RefreshToken: tr.RefreshToken})
	wantCode(t, err, CodeInvalidClient)
	if list, _ := fx.svc.ListPublishedApps(fx.ctx(), testOrgAcme); len(list) != 0 {
		t.Errorf("published after delete: %+v", list)
	}
	if _, err := fx.svc.DeleteApp(fx.ctx(), DeleteAppRequest{OrgID: testOrgAcme, AppID: app.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if ev := fx.eventsOf(EventAppDeleted); len(ev) != 1 {
		t.Errorf("app_deleted: %v", ev)
	}
	rev := fx.eventsOf(EventGrantRevoked)
	if len(rev) != 1 || rev[0].Payload["reason"] != ReasonAppDeleted || rev[0].Payload["app_name"] != "Reporting v2" {
		t.Errorf("grant_revoked: %v", rev)
	}
}

func TestClientSecrets(t *testing.T) {
	fx := newSvc(t)
	ca, err := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
	mustNoErr(t, err)
	app := ca.App
	fx.clock.Advance(time.Minute)

	cs, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgAcme, AppID: app.ID, CreatedBy: testUserB})
	mustNoErr(t, err)
	if !secrets.IsClientSecret(cs.ClientSecret) || cs.ClientSecret == ca.ClientSecret || cs.Secret.Hash != nil || cs.Secret.CreatedBy != testUserB ||
		cs.Secret.Alias != secrets.ClientSecretAlias(cs.ClientSecret) {
		t.Errorf("created secret: %+v / %q", cs.Secret, cs.ClientSecret)
	}

	// Listing: both, oldest first, by alias; no hash, no plaintext.
	list, err := fx.svc.ListClientSecrets(fx.ctx(), testOrgAcme, app.ID)
	mustNoErr(t, err)
	if len(list) != 2 || list[0].ID != ca.Secret.ID || list[1].ID != cs.Secret.ID {
		t.Fatalf("listing: %+v", list)
	}
	for _, s := range list {
		if s.Hash != nil || !strings.HasSuffix(s.Alias, "********") || strings.Contains(s.Alias, cs.ClientSecret) {
			t.Errorf("listed secret leaks: %+v", s)
		}
	}
	// Both work at the token endpoint; a deleted one stops.
	exchange := func(secret string) error {
		req := AuthorizeRequest{ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: app.RedirectURIs[0]}
		_, code := fx.approve(fx.start(req), testUserA, testOrgAcme)
		_, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: GrantTypeAuthorizationCode, ClientID: app.ID, ClientSecret: secret, Code: code, RedirectURI: req.RedirectURI})
		return err
	}
	mustNoErr(t, exchange(ca.ClientSecret))
	mustNoErr(t, exchange(cs.ClientSecret))
	mustNoErr(t, fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgAcme, AppID: app.ID, SecretID: ca.Secret.ID, Actor: testUserA}))
	wantCode(t, exchange(ca.ClientSecret), CodeInvalidClient)
	mustNoErr(t, exchange(cs.ClientSecret))
	list, _ = fx.svc.ListClientSecrets(fx.ctx(), testOrgAcme, app.ID)
	if len(list) != 1 || list[0].ID != cs.Secret.ID || list[0].LastUsedAt == nil {
		t.Errorf("after delete: %+v", list)
	}
	if ev := fx.eventsOf(EventClientSecretDeleted); len(ev) != 1 || ev[0].Payload["secret_id"] != ca.Secret.ID || ev[0].Payload["user_id"] != testUserA {
		t.Errorf("client_secret_deleted: %v", ev)
	}

	// Not found: the secret twice, a malformed id, another organization, a dynamic app.
	dyn := fx.register()
	for name, f := range map[string]func() error{
		"a deleted secret": func() error {
			return fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgAcme, AppID: app.ID, SecretID: ca.Secret.ID})
		},
		"a malformed secret id": func() error {
			return fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgAcme, AppID: app.ID, SecretID: "x"})
		},
		"a secret of another app": func() error {
			other, _ := fx.svc.CreateApp(fx.ctx(), createReq(testOrgAcme))
			return fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgAcme, AppID: app.ID, SecretID: other.Secret.ID})
		},
		"delete in another organization": func() error {
			return fx.svc.DeleteClientSecret(fx.ctx(), DeleteSecretRequest{OrgID: testOrgOther, AppID: app.ID, SecretID: cs.Secret.ID})
		},
		"list in another organization": func() error {
			_, err := fx.svc.ListClientSecrets(fx.ctx(), testOrgOther, app.ID)
			return err
		},
		"create in another organization": func() error {
			_, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgOther, AppID: app.ID})
			return err
		},
		"list for a dynamic app": func() error {
			_, err := fx.svc.ListClientSecrets(fx.ctx(), testOrgAcme, dyn.App.ID)
			return err
		},
		"create for a dynamic app": func() error {
			_, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgAcme, AppID: dyn.App.ID})
			return err
		},
	} {
		if err := f(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgAcme, AppID: app.ID, CreatedBy: "root"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("created_by that is not a user id: %v", err)
	}

	// At most maxSecretsPerApp.
	for {
		_, err := fx.svc.CreateClientSecret(fx.ctx(), CreateSecretRequest{OrgID: testOrgAcme, AppID: app.ID})
		if err != nil {
			if !errors.Is(err, ErrLimit) {
				t.Fatalf("unexpected: %v", err)
			}
			break
		}
	}
	if list, _ := fx.svc.ListClientSecrets(fx.ctx(), testOrgAcme, app.ID); len(list) != maxSecretsPerApp {
		t.Errorf("%d secrets", len(list))
	}
}
