package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// The organization's OAuth Apps API (oauth_apps_api.go) over a fake of oauth.Authority that keeps
// the contract of authority.go: an app of another organization, a dynamic app (where the method is
// for published apps) and a deleted one are ErrNotFound; input the Service refuses is ErrInvalid; a
// cap is ErrLimit. TestOAuthAppsWithService runs the same routes over the real Service.

const (
	oauthAppsListKey    = "GET /platform/organizations/{slug}/oauth/apps"
	oauthAppsCreateKey  = "POST /platform/organizations/{slug}/oauth/apps"
	oauthAppsUpdateKey  = "PUT /platform/organizations/{slug}/oauth/apps/{id}"
	oauthAppsDeleteKey  = "DELETE /platform/organizations/{slug}/oauth/apps/{id}"
	oauthAppsRevokeKey  = "POST /platform/organizations/{slug}/oauth/apps/{id}/revoke"
	oauthSecretsListKey = "GET /platform/organizations/{slug}/oauth/apps/{app_id}/client-secrets"
	oauthSecretsMakeKey = "POST /platform/organizations/{slug}/oauth/apps/{app_id}/client-secrets"
	oauthSecretsDropKey = "DELETE /platform/organizations/{slug}/oauth/apps/{app_id}/client-secrets/{secret_id}"

	oauthAppsBase = "/platform/organizations/default/oauth/apps"
)

// ---- the fake ---------------------------------------------------------------------------------

type oauthAppsGrant struct {
	appID, userID string
	orgID         int64
	scopes        []string
	at            time.Time
	revoked       bool
}

// oauthAppsFake implements the methods the handlers call. Every other method of the interface panics
// on the nil embedded value, which is what a test wants.
type oauthAppsFake struct {
	oauth.Authority
	mu      sync.Mutex
	clock   time.Time
	apps    map[string]*oauth.App
	secrets map[string][]oauth.AppSecret // by app id; the hash is kept, the plaintext never
	grants  []*oauthAppsGrant
	seen    []any            // the requests the handlers made, in order
	fail    map[string]error // method name -> the error it returns instead of working
}

func newOAuthAppsFake() *oauthAppsFake {
	return &oauthAppsFake{
		clock:   time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		apps:    map[string]*oauth.App{},
		secrets: map[string][]oauth.AppSecret{},
		fail:    map[string]error{},
	}
}

func oauthAppsTestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// tick returns the clock and moves it on, so that every row has its own time.
func (x *oauthAppsFake) tick() time.Time {
	t := x.clock
	x.clock = x.clock.Add(time.Minute)
	return t
}

// enter records a call and returns the error the test forced on it, if any.
func (x *oauthAppsFake) enter(method string, req any) error {
	x.seen = append(x.seen, req)
	return x.fail[method]
}

func (x *oauthAppsFake) calls() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.seen)
}

func (x *oauthAppsFake) last() any {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.seen[len(x.seen)-1]
}

// newSecret makes a secret record with the plaintext the caller may show once.
func (x *oauthAppsFake) newSecret(appID, createdBy string) (oauth.AppSecret, string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	plain := oauth.ClientSecretPrefix + hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plain))
	return oauth.AppSecret{
		ID: oauthAppsTestID(), AppID: appID, Alias: plain[:8] + "********", Hash: sum[:],
		CreatedBy: createdBy, CreatedAt: x.tick(),
	}, plain
}

// seedManual publishes an app of the organization with one secret and returns both ids.
func (x *oauthAppsFake) seedManual(orgID int64, name string) (appID, secretID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	app := &oauth.App{
		ID: oauthAppsTestID(), RegistrationType: oauth.RegistrationManual, OrgID: orgID, Name: name,
		Website: "https://" + strings.ToLower(strings.ReplaceAll(name, " ", "-")) + ".example.test",
		Icon:    "https://cdn.example.test/icon.png", RedirectURIs: []string{"https://app.example.test/callback"},
		Scopes: []string{oauth.ScopeProjectsRead, oauth.ScopeDatabaseRead}, TokenEndpointAuthMethod: oauth.AuthMethodBasic,
		CreatedBy: "99999999-0000-4000-8000-000000000001", CreatedAt: x.tick(),
	}
	app.UpdatedAt = app.CreatedAt
	x.apps[app.ID] = app
	sec, _ := x.newSecret(app.ID, app.CreatedBy)
	x.secrets[app.ID] = []oauth.AppSecret{sec}
	return app.ID, sec.ID
}

// seedDynamic registers an app the way an MCP client does, with the self-asserted site and logo
// that the API must not return.
func (x *oauthAppsFake) seedDynamic(name string, redirectURIs ...string) *oauth.App {
	x.mu.Lock()
	defer x.mu.Unlock()
	app := &oauth.App{
		ID: oauthAppsTestID(), RegistrationType: oauth.RegistrationDynamic, Name: name,
		Website: "https://client.example.test", Icon: "https://client.example.test/logo.png",
		RedirectURIs: redirectURIs, Scopes: append([]string(nil), oauth.AdvertisedScopes...),
		TokenEndpointAuthMethod: oauth.AuthMethodNone, CreatedAt: x.tick(),
	}
	x.apps[app.ID] = app
	return app
}

func (x *oauthAppsFake) addGrant(appID string, orgID int64, userID string, scopes ...string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.grants = append(x.grants, &oauthAppsGrant{appID: appID, userID: userID, orgID: orgID, scopes: scopes, at: x.tick()})
}

func (x *oauthAppsFake) liveGrants(appID string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, g := range x.grants {
		if g.appID == appID && !g.revoked {
			n++
		}
	}
	return n
}

func (x *oauthAppsFake) secretCount(appID string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.secrets[appID])
}

// manual returns the live published app of the organization, or ErrNotFound.
func (x *oauthAppsFake) manual(orgID int64, appID string) (*oauth.App, error) {
	a := x.apps[appID]
	if a == nil || a.DeletedAt != nil || a.RegistrationType != oauth.RegistrationManual || a.OrgID != orgID {
		return nil, oauth.ErrNotFound
	}
	return a, nil
}

func oauthAppsFakeValidate(name string, scopes, uris []string) error {
	switch {
	case strings.TrimSpace(name) == "" || len(name) > oauth.MaxClientNameLen:
		return fmt.Errorf("%w: name must be 1 to %d characters", oauth.ErrInvalid, oauth.MaxClientNameLen)
	case len(scopes) == 0:
		return fmt.Errorf("%w: scopes needs at least one scope", oauth.ErrInvalid)
	case len(uris) == 0 || len(uris) > oauth.MaxRedirectURIs:
		return fmt.Errorf("%w: redirect_uris needs 1 to %d URIs", oauth.ErrInvalid, oauth.MaxRedirectURIs)
	}
	for _, s := range scopes {
		if !oauth.ValidScope(s) {
			return fmt.Errorf("%w: %q is not a scope", oauth.ErrInvalid, s)
		}
	}
	for _, u := range uris {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://127.0.0.1") {
			return fmt.Errorf("%w: redirect URI %q is not https or loopback http", oauth.ErrInvalid, u)
		}
	}
	return nil
}

func (x *oauthAppsFake) ListPublishedApps(_ context.Context, orgID int64) ([]oauth.App, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("ListPublishedApps", orgID); err != nil {
		return nil, err
	}
	var out []oauth.App
	for _, a := range x.apps {
		if a.DeletedAt == nil && a.RegistrationType == oauth.RegistrationManual && a.OrgID == orgID {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (x *oauthAppsFake) ListAuthorizedApps(_ context.Context, orgID int64) ([]oauth.AuthorizedApp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("ListAuthorizedApps", orgID); err != nil {
		return nil, err
	}
	var out []oauth.AuthorizedApp
	for _, a := range x.apps {
		if a.DeletedAt != nil {
			continue
		}
		it := oauth.AuthorizedApp{App: *a}
		for _, g := range x.grants {
			if g.appID != a.ID || g.orgID != orgID || g.revoked {
				continue
			}
			if g.at.After(it.AuthorizedAt) {
				it.AuthorizedAt = g.at
			}
			it.Scopes = oauth.UnionScopes(it.Scopes, oauth.IntersectScopes(g.scopes, a.Scopes))
			if it.FirstApprover == "" {
				it.FirstApprover = g.userID
			}
		}
		if !it.AuthorizedAt.IsZero() {
			it.Scopes = oauth.NormalizeScopes(it.Scopes)
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthorizedAt.After(out[j].AuthorizedAt) })
	return out, nil
}

func (x *oauthAppsFake) CreateApp(_ context.Context, req oauth.CreateAppRequest) (*oauth.CreatedApp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("CreateApp", req); err != nil {
		return nil, err
	}
	if err := oauthAppsFakeValidate(req.Name, req.Scopes, req.RedirectURIs); err != nil {
		return nil, err
	}
	n := 0
	for _, a := range x.apps {
		if a.DeletedAt == nil && a.RegistrationType == oauth.RegistrationManual && a.OrgID == req.OrgID {
			n++
		}
	}
	if n >= oauth.MaxManualAppsPerOrg {
		return nil, fmt.Errorf("%w: an organization can publish at most %d apps", oauth.ErrLimit, oauth.MaxManualAppsPerOrg)
	}
	app := &oauth.App{
		ID: oauthAppsTestID(), RegistrationType: oauth.RegistrationManual, OrgID: req.OrgID, Name: req.Name,
		Website: req.Website, Icon: req.Icon, RedirectURIs: req.RedirectURIs, Scopes: req.Scopes,
		TokenEndpointAuthMethod: oauth.AuthMethodBasic, CreatedBy: req.CreatedBy, CreatedAt: x.tick(),
	}
	app.UpdatedAt = app.CreatedAt
	x.apps[app.ID] = app
	sec, plain := x.newSecret(app.ID, req.CreatedBy)
	x.secrets[app.ID] = []oauth.AppSecret{sec}
	sec.Hash = nil
	return &oauth.CreatedApp{App: *app, Secret: sec, ClientSecret: plain}, nil
}

func (x *oauthAppsFake) UpdateApp(_ context.Context, req oauth.UpdateAppRequest) (*oauth.App, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("UpdateApp", req); err != nil {
		return nil, err
	}
	app, err := x.manual(req.OrgID, req.AppID)
	if err != nil {
		return nil, err
	}
	if err := oauthAppsFakeValidate(req.Name, req.Scopes, req.RedirectURIs); err != nil {
		return nil, err
	}
	app.Name, app.Website, app.Icon, app.Scopes, app.RedirectURIs = req.Name, req.Website, req.Icon, req.Scopes, req.RedirectURIs
	app.UpdatedAt = x.tick()
	cp := *app
	return &cp, nil
}

func (x *oauthAppsFake) DeleteApp(_ context.Context, req oauth.DeleteAppRequest) (*oauth.App, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("DeleteApp", req); err != nil {
		return nil, err
	}
	app, err := x.manual(req.OrgID, req.AppID)
	if err != nil {
		return nil, err
	}
	was := *app
	at := x.tick()
	app.DeletedAt = &at
	for _, g := range x.grants {
		if g.appID == app.ID {
			g.revoked = true
		}
	}
	return &was, nil
}

func (x *oauthAppsFake) RevokeApp(_ context.Context, req oauth.RevokeAppRequest) (*oauth.RevokedApp, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("RevokeApp", req); err != nil {
		return nil, err
	}
	app := x.apps[req.AppID]
	if app == nil || app.DeletedAt != nil {
		return nil, oauth.ErrNotFound
	}
	res := &oauth.RevokedApp{App: *app}
	for _, g := range x.grants {
		if g.appID != app.ID || g.revoked || (req.OrgID != 0 && g.orgID != req.OrgID) {
			continue
		}
		if g.at.After(res.AuthorizedAt) {
			res.AuthorizedAt = g.at
		}
		g.revoked = true
		res.Revoked++
	}
	return res, nil
}

func (x *oauthAppsFake) ListClientSecrets(_ context.Context, orgID int64, appID string) ([]oauth.AppSecret, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("ListClientSecrets", orgID); err != nil {
		return nil, err
	}
	if _, err := x.manual(orgID, appID); err != nil {
		return nil, err
	}
	out := make([]oauth.AppSecret, 0, len(x.secrets[appID]))
	for _, s := range x.secrets[appID] {
		s.Hash = nil
		out = append(out, s)
	}
	return out, nil
}

func (x *oauthAppsFake) CreateClientSecret(_ context.Context, req oauth.CreateSecretRequest) (*oauth.CreatedSecret, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("CreateClientSecret", req); err != nil {
		return nil, err
	}
	if _, err := x.manual(req.OrgID, req.AppID); err != nil {
		return nil, err
	}
	sec, plain := x.newSecret(req.AppID, req.CreatedBy)
	x.secrets[req.AppID] = append(x.secrets[req.AppID], sec)
	sec.Hash = nil
	return &oauth.CreatedSecret{Secret: sec, ClientSecret: plain}, nil
}

func (x *oauthAppsFake) DeleteClientSecret(_ context.Context, req oauth.DeleteSecretRequest) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.enter("DeleteClientSecret", req); err != nil {
		return err
	}
	if _, err := x.manual(req.OrgID, req.AppID); err != nil {
		return err
	}
	list := x.secrets[req.AppID]
	for i, s := range list {
		if s.ID == req.SecretID {
			x.secrets[req.AppID] = append(list[:i:i], list[i+1:]...)
			return nil
		}
	}
	return oauth.ErrNotFound
}

// ---- helpers ----------------------------------------------------------------------------------

func oauthAppsFixture(t testing.TB) (*fixture, *oauthAppsFake) {
	t.Helper()
	f := newFixture(t)
	fake := newOAuthAppsFake()
	f.srv.oauth = fake
	return f, fake
}

func oauthAppsRows(t testing.TB, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a list of objects: %v: %s", err, rec.Body)
	}
	return out
}

func oauthAppsObject(t testing.TB, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not an object: %v: %s", err, rec.Body)
	}
	return out
}

func oauthAppsBody(name string) map[string]any {
	return map[string]any{
		"name": name, "website": "https://deploy.example.test",
		"scopes":        []string{"projects:read", "database:read"},
		"redirect_uris": []string{"https://deploy.example.test/callback"},
	}
}

// ---- conformance: the eight operations against the spec ----------------------------------------

func TestOAuthAppsConformance(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	dyn := fake.seedDynamic("Claude Code", "http://127.0.0.1:33418/callback")
	fake.addGrant(dyn.ID, f.org.ID, f.userID, oauth.ScopeProjectsRead, oauth.ScopeDatabaseRead)

	var appID, secondID string
	f.run(t, []step{
		{key: oauthAppsListKey, path: oauthAppsBase + "?type=published", check: want("", "[]")},
		{key: oauthAppsListKey, path: oauthAppsBase + "?type=authorized", check: want("0.id", dyn.ID)},
		{key: oauthAppsCreateKey, path: oauthAppsBase, body: oauthAppsBody("Deploy bot"), check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			appID, _ = oauthAppsObject(t, rec)["id"].(string)
		}},
	})
	app := oauthAppsBase + "/" + appID
	f.run(t, []step{
		{key: oauthAppsListKey, path: oauthAppsBase + "?type=published", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			rows := oauthAppsRows(t, rec)
			if len(rows) != 1 || rows[0]["id"] != appID || rows[0]["client_id"] != appID || rows[0]["registration_type"] != "manual" {
				t.Errorf("published = %v", rows)
			}
		}},
		{key: oauthSecretsListKey, path: app + "/client-secrets", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			list, _ := oauthAppsObject(t, rec)["client_secrets"].([]any)
			if len(list) != 1 {
				t.Fatalf("client_secrets = %v", list)
			}
			s := list[0].(map[string]any)
			if alias, _ := s["client_secret_alias"].(string); s["oauth_app_id"] != appID || !strings.HasPrefix(alias, "sba_") || !strings.HasSuffix(alias, "********") || s["last_used_at"] != nil {
				t.Errorf("secret = %v", s)
			}
		}},
		{key: oauthSecretsMakeKey, path: app + "/client-secrets", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			secondID, _ = oauthAppsObject(t, rec)["id"].(string)
		}},
		{key: oauthAppsUpdateKey, path: app, body: map[string]any{
			"name": "Deploy bot 2", "website": "https://deploy.example.test", "icon": "https://cdn.example.test/bot.png",
			"scopes": []string{"projects:read"}, "redirect_uris": []string{"https://deploy.example.test/cb2"},
		}, check: want("name", "Deploy bot 2")},
		{key: oauthAppsRevokeKey, path: oauthAppsBase + "/" + dyn.ID + "/revoke", check: want("name", "Claude Code")},
	})
	f.run(t, []step{
		{key: oauthSecretsDropKey, path: app + "/client-secrets/" + secondID},
		{key: oauthAppsDeleteKey, path: app, check: want("name", "Deploy bot 2")},
		{key: oauthAppsListKey, path: oauthAppsBase + "?type=published", check: want("", "[]")},
		{key: oauthAppsListKey, path: oauthAppsBase + "?type=authorized", check: want("", "[]")},
	})
}

// ---- who may call -----------------------------------------------------------------------------

// The eight operations, both tabs of the list included, are for Owners and Administrators of the
// organization. A refusal comes before the Service is called: from authorize for what changes
// something, from the handler for what only reads (the generic rule lets every role read
// oauth_apps, so the handlers ask for the right to update).
func TestOAuthAppsRoleMatrix(t *testing.T) {
	rf := newRolesFixture(t)
	ops := []struct {
		method, path string
		body         any
	}{
		{"GET", oauthAppsBase + "?type=published", nil},
		{"GET", oauthAppsBase + "?type=authorized", nil},
		{"POST", oauthAppsBase, oauthAppsBody("Role bot")},
		{"PUT", oauthAppsBase + "/{app}", oauthAppsBody("Renamed")},
		{"DELETE", oauthAppsBase + "/{app}", nil},
		{"POST", oauthAppsBase + "/{app}/revoke", nil},
		{"GET", oauthAppsBase + "/{app}/client-secrets", nil},
		{"POST", oauthAppsBase + "/{app}/client-secrets", nil},
		{"DELETE", oauthAppsBase + "/{app}/client-secrets/{secret}", nil},
	}
	for _, op := range ops {
		for _, role := range roleNames {
			fake := newOAuthAppsFake()
			rf.srv.oauth = fake
			appID, secretID := fake.seedManual(rf.org.ID, "Seeded")
			path := strings.NewReplacer("{app}", appID, "{secret}", secretID).Replace(op.path)
			t.Run(op.method+" "+op.path+" as "+role, func(t *testing.T) {
				rec := rf.as(role, op.method, path, op.body)
				if role == "owner" || role == "admin" {
					if rec.Code/100 != 2 || fake.calls() == 0 {
						t.Errorf("status %d, calls %d: %s", rec.Code, fake.calls(), truncate(rec.Body.String(), 300))
					}
					return
				}
				if rec.Code != http.StatusForbidden || fake.calls() != 0 {
					t.Errorf("status %d, calls %d, want 403 and none: %s", rec.Code, fake.calls(), truncate(rec.Body.String(), 300))
				}
			})
		}
	}
}

// Only a dashboard session reaches /platform. A personal access token and a missing token are
// refused before the handler, so neither can read or change an app or mint a secret.
func TestOAuthAppsNeedADashboardSession(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	appID, secretID := fake.seedManual(f.org.ID, "Seeded")
	pat := secrets.NewPAT()
	if err := f.reg.CreateAccessToken(context.Background(), &registry.AccessToken{UserID: f.userID, Name: "cli", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	for _, c := range oauthAppsAllRoutes(appID, secretID) {
		for name, token := range map[string]string{"pat": pat, "none": ""} {
			t.Run(c.method+" "+c.path+" with "+name, func(t *testing.T) {
				if rec := f.doAs(token, c.method, c.path, c.body); rec.Code != http.StatusUnauthorized {
					t.Errorf("status %d, want 401: %s", rec.Code, truncate(rec.Body.String(), 200))
				}
			})
		}
	}
	if fake.calls() != 0 {
		t.Errorf("the Service was called %d times", fake.calls())
	}
}

type oauthAppsRoute struct {
	method, path string
	body         any
}

// oauthAppsAllRoutes is each of the eight operations once (the list in both forms).
func oauthAppsAllRoutes(appID, secretID string) []oauthAppsRoute {
	app := oauthAppsBase + "/" + appID
	return []oauthAppsRoute{
		{"GET", oauthAppsBase + "?type=authorized", nil},
		{"GET", oauthAppsBase + "?type=published", nil},
		{"POST", oauthAppsBase, oauthAppsBody("Bot")},
		{"PUT", app, oauthAppsBody("Bot")},
		{"DELETE", app, nil},
		{"POST", app + "/revoke", nil},
		{"GET", app + "/client-secrets", nil},
		{"POST", app + "/client-secrets", nil},
		{"DELETE", app + "/client-secrets/" + secretID, nil},
	}
}

// [api] disable_oauth turns the page's API off with the rest of OAuth. The switch is read for each
// request.
func TestOAuthAppsDisabled(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	appID, secretID := fake.seedManual(f.org.ID, "Seeded")
	f.cfg.API.DisableOAuth = true
	for _, c := range oauthAppsAllRoutes(appID, secretID) {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if rec := f.do(c.method, c.path, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("status %d, want 404: %s", rec.Code, truncate(rec.Body.String(), 200))
			}
		})
	}
	if fake.calls() != 0 {
		t.Errorf("the Service was called %d times while OAuth is off", fake.calls())
	}
	f.cfg.API.DisableOAuth = false
	if rec := f.do("GET", oauthAppsBase+"?type=published", nil); rec.Code != http.StatusOK {
		t.Errorf("after the switch is off again: status %d", rec.Code)
	}
}

// ---- secrets ----------------------------------------------------------------------------------

// A client secret is in the answer that creates it and nowhere else: not in later answers, not as a
// hash, and the answer that carries it must not be cached. The listing shows the alias.
func TestOAuthAppsSecretsAreShownOnce(t *testing.T) {
	f, _ := oauthAppsFixture(t)
	rec := f.do("POST", oauthAppsBase, oauthAppsBody("Deploy bot"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("create app: Cache-Control = %q, want no-store", cc)
	}
	created := oauthAppsObject(t, rec)
	first, _ := created["client_secret"].(string)
	appID, _ := created["id"].(string)
	if !strings.HasPrefix(first, oauth.ClientSecretPrefix) || len(first) != len(oauth.ClientSecretPrefix)+oauth.ClientSecretHexLen {
		t.Fatalf("client_secret = %q", first)
	}
	if created["client_id"] != appID || created["client_secret_expires_at"] != float64(0) {
		t.Errorf("create answer = %v", created)
	}

	app := oauthAppsBase + "/" + appID
	rec = f.do("POST", app+"/client-secrets", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create secret: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("create secret: Cache-Control = %q, want no-store", cc)
	}
	second := oauthAppsObject(t, rec)
	plain, _ := second["client_secret"].(string)
	if !strings.HasPrefix(plain, oauth.ClientSecretPrefix) || plain == first {
		t.Fatalf("second client_secret = %q", plain)
	}
	if second["client_secret_alias"] != plain[:8]+"********" || second["created_by"] != f.userID || second["oauth_app_id"] != appID {
		t.Errorf("create secret answer = %v", second)
	}

	// Nothing else the API says holds either secret, or a hash of it, or a client_secret key.
	var forbidden []string
	for _, s := range []string{first, plain} {
		sum := sha256.Sum256([]byte(s))
		forbidden = append(forbidden, s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:]))
	}
	forbidden = append(forbidden, `"client_secret":`)
	for _, path := range []string{oauthAppsBase + "?type=published", oauthAppsBase + "?type=authorized", app + "/client-secrets"} {
		rec := f.do("GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		for _, bad := range forbidden {
			if strings.Contains(rec.Body.String(), bad) {
				t.Errorf("GET %s leaks %q", path, bad[:min(len(bad), 12)]+"...")
			}
		}
	}

	// The listing names the secrets by alias, oldest first, and last_used_at is null.
	rec = f.do("GET", app+"/client-secrets", nil)
	list, _ := oauthAppsObject(t, rec)["client_secrets"].([]any)
	if len(list) != 2 {
		t.Fatalf("client_secrets = %v", list)
	}
	for i, s := range []string{first, plain} {
		row := list[i].(map[string]any)
		if row["client_secret_alias"] != s[:8]+"********" || row["last_used_at"] != nil {
			t.Errorf("row %d = %v", i, row)
		}
	}

	// Deleting one leaves the other, and the answer has no body.
	rec = f.do("DELETE", app+"/client-secrets/"+second["id"].(string), nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("delete secret: %d %q", rec.Code, rec.Body)
	}
	rec = f.do("GET", app+"/client-secrets", nil)
	if list, _ := oauthAppsObject(t, rec)["client_secrets"].([]any); len(list) != 1 {
		t.Errorf("after delete: %v", list)
	}
}

// ---- organization binding ---------------------------------------------------------------------

func oauthAppsReqOrg(req any) int64 {
	switch r := req.(type) {
	case int64:
		return r
	case oauth.CreateAppRequest:
		return r.OrgID
	case oauth.UpdateAppRequest:
		return r.OrgID
	case oauth.DeleteAppRequest:
		return r.OrgID
	case oauth.RevokeAppRequest:
		return r.OrgID
	case oauth.CreateSecretRequest:
		return r.OrgID
	case oauth.DeleteSecretRequest:
		return r.OrgID
	}
	return -1
}

// Every call carries the organization of the path, not one the caller could pick another way: an
// owner of two organizations reaches an app of the second only through the second, and a dynamic
// app is not one that an organization publishes.
func TestOAuthAppsAreBoundToTheOrganizationOfThePath(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	ctx := context.Background()
	bravo, err := f.reg.CreateOrganization(ctx, "bravo", "Bravo")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.members.EnsureOwner(ctx, members.OrgRef{ID: bravo.ID, Slug: bravo.Slug}, f.userID); err != nil {
		t.Fatal(err)
	}
	appB, secretB := fake.seedManual(bravo.ID, "Bravo app")
	dyn := fake.seedDynamic("Some client", "https://client.example.test/cb")

	for _, c := range []oauthAppsRoute{
		{"PUT", oauthAppsBase + "/" + appB, oauthAppsBody("Taken")},
		{"DELETE", oauthAppsBase + "/" + appB, nil},
		{"GET", oauthAppsBase + "/" + appB + "/client-secrets", nil},
		{"POST", oauthAppsBase + "/" + appB + "/client-secrets", nil},
		{"DELETE", oauthAppsBase + "/" + appB + "/client-secrets/" + secretB, nil},
		{"PUT", oauthAppsBase + "/" + dyn.ID, oauthAppsBody("Taken")},
		{"DELETE", oauthAppsBase + "/" + dyn.ID, nil},
		{"GET", oauthAppsBase + "/" + dyn.ID + "/client-secrets", nil},
		{"POST", oauthAppsBase + "/" + dyn.ID + "/client-secrets", nil},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := f.do(c.method, c.path, c.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404: %s", rec.Code, truncate(rec.Body.String(), 200))
			}
			if got := oauthAppsReqOrg(fake.last()); got != f.org.ID {
				t.Errorf("the Service got organization %d, want %d (the path's)", got, f.org.ID)
			}
		})
	}
	if n := fake.secretCount(appB); n != 1 {
		t.Errorf("bravo's app has %d secrets after the refused calls, want 1", n)
	}

	rows := oauthAppsRows(t, f.do("GET", oauthAppsBase+"?type=published", nil))
	if len(rows) != 0 {
		t.Errorf("default publishes %v, want nothing", rows)
	}
	rows = oauthAppsRows(t, f.do("GET", "/platform/organizations/bravo/oauth/apps?type=published", nil))
	if len(rows) != 1 || rows[0]["id"] != appB || rows[0]["name"] != "Bravo app" {
		t.Errorf("bravo publishes %v", rows)
	}
	if rec := f.do("PUT", "/platform/organizations/bravo/oauth/apps/"+appB, oauthAppsBody("Renamed")); rec.Code != http.StatusOK {
		t.Fatalf("update through its own organization: %d %s", rec.Code, rec.Body)
	}
	if got := oauthAppsReqOrg(fake.last()); got != bravo.ID {
		t.Errorf("the Service got organization %d, want bravo's %d", got, bravo.ID)
	}

	// Who did it: the signed-in user is the actor and the creator.
	if rec := f.do("POST", oauthAppsBase, oauthAppsBody("Mine")); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if req := fake.last().(oauth.CreateAppRequest); req.CreatedBy != f.userID || req.OrgID != f.org.ID {
		t.Errorf("CreateApp got %+v", req)
	}
	appID := oauthAppsRows(t, f.do("GET", oauthAppsBase+"?type=published", nil))[0]["id"].(string)
	f.do("PUT", oauthAppsBase+"/"+appID, oauthAppsBody("Mine too"))
	if req := fake.last().(oauth.UpdateAppRequest); req.Actor != f.userID {
		t.Errorf("UpdateApp actor = %q", req.Actor)
	}
	f.do("POST", oauthAppsBase+"/"+appID+"/client-secrets", nil)
	if req := fake.last().(oauth.CreateSecretRequest); req.CreatedBy != f.userID {
		t.Errorf("CreateClientSecret created by %q", req.CreatedBy)
	}
	f.do("DELETE", oauthAppsBase+"/"+appID, nil)
	if req := fake.last().(oauth.DeleteAppRequest); req.Actor != f.userID || req.AppID != appID {
		t.Errorf("DeleteApp got %+v", req)
	}
}

// ---- the lists --------------------------------------------------------------------------------

// The Authorized tab lists every app that holds a live grant in the organization, published or
// dynamic. A dynamic app's own words (site, logo) are not returned, and its author cell says where
// it sends codes.
func TestOAuthAppsAuthorizedList(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	dyn := fake.seedDynamic("Claude Code", "http://127.0.0.1:33418/callback", "http://localhost:1/x")
	manualID, _ := fake.seedManual(f.org.ID, "Deploy bot")
	fake.addGrant(dyn.ID, f.org.ID, f.userID, oauth.ScopeProjectsRead, oauth.ScopeOrganizationsRead)
	fake.addGrant(dyn.ID, f.org.ID, "aaaaaaaa-0000-4000-8000-000000000001", oauth.ScopeDatabaseRead, oauth.ScopeProjectsRead)
	fake.addGrant(manualID, f.org.ID, f.userID, oauth.ScopeProjectsRead)
	elsewhere := fake.seedDynamic("Elsewhere", "https://elsewhere.example.test/cb")
	fake.addGrant(elsewhere.ID, f.org.ID+100, f.userID, oauth.ScopeProjectsRead)
	newest := fake.grants[1].at

	rec := f.do("GET", oauthAppsBase+"?type=authorized", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, oauthAppsListKey, rec.Body.Bytes())
	byID := map[string]map[string]any{}
	for _, row := range oauthAppsRows(t, rec) {
		byID[row["id"].(string)] = row
	}
	if len(byID) != 2 || byID[elsewhere.ID] != nil {
		t.Fatalf("listed %d apps, want the two with a grant here: %v", len(byID), byID)
	}

	d := byID[dyn.ID]
	if d["app_id"] != dyn.ID || d["client_id"] != dyn.ID || d["registration_type"] != "dynamic" || d["name"] != "Claude Code" {
		t.Errorf("dynamic row = %v", d)
	}
	if d["website"] != "" {
		t.Errorf("dynamic website = %q, want empty", d["website"])
	}
	if _, has := d["icon"]; has {
		t.Errorf("a dynamic app's logo is returned: %v", d["icon"])
	}
	if d["created_by"] != "127.0.0.1" {
		t.Errorf("dynamic created_by = %v, want the host of its first redirect URI", d["created_by"])
	}
	if d["authorized_at"] != ts(newest) {
		t.Errorf("authorized_at = %v, want the newest grant's %s", d["authorized_at"], ts(newest))
	}
	if got := fmt.Sprint(d["scopes"]); got != "[database:read organizations:read projects:read]" {
		t.Errorf("scopes = %s, want the union of the grants, sorted", got)
	}

	m := byID[manualID]
	if m["registration_type"] != "manual" || m["website"] != "https://deploy-bot.example.test" || m["icon"] != "https://cdn.example.test/icon.png" {
		t.Errorf("manual row = %v", m)
	}
	if m["created_by"] != "Default" {
		t.Errorf("manual created_by = %v, want the publishing organization's name", m["created_by"])
	}
}

// The Published tab lists the organization's manual apps and never a dynamic one; the website and
// icon are the publisher's own.
func TestOAuthAppsPublishedList(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	fake.seedDynamic("Claude Code", "http://127.0.0.1:33418/callback")
	appID, _ := fake.seedManual(f.org.ID, "Deploy bot")
	fake.seedManual(f.org.ID+100, "Someone else's")

	rec := f.do("GET", oauthAppsBase+"?type=published", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rows := oauthAppsRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("published = %v", rows)
	}
	r := rows[0]
	if r["id"] != appID || r["client_id"] != appID || r["name"] != "Deploy bot" || r["website"] != "https://deploy-bot.example.test" ||
		r["icon"] != "https://cdn.example.test/icon.png" || r["created_by"] != "99999999-0000-4000-8000-000000000001" {
		t.Errorf("row = %v", r)
	}
	if got := fmt.Sprint(r["redirect_uris"], r["scopes"]); got != "[https://app.example.test/callback] [projects:read database:read]" {
		t.Errorf("redirect_uris and scopes = %s", got)
	}
	if _, has := r["authorized_at"]; has {
		t.Error("the published tab carries authorized_at")
	}
}

// ---- revoke -----------------------------------------------------------------------------------

func TestOAuthAppsRevoke(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	dyn := fake.seedDynamic("Claude Code", "http://127.0.0.1:33418/callback")
	fake.addGrant(dyn.ID, f.org.ID, f.userID, oauth.ScopeProjectsRead)
	fake.addGrant(dyn.ID, f.org.ID, "aaaaaaaa-0000-4000-8000-000000000001", oauth.ScopeProjectsRead)
	fake.addGrant(dyn.ID, f.org.ID+100, f.userID, oauth.ScopeProjectsRead)
	newest := fake.grants[1].at

	rec := f.do("POST", oauthAppsBase+"/"+dyn.ID+"/revoke", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, oauthAppsRevokeKey, rec.Body.Bytes())
	body := oauthAppsObject(t, rec)
	if body["id"] != dyn.ID || body["name"] != "Claude Code" || body["website"] != "" || body["authorized_at"] != ts(newest) {
		t.Errorf("answer = %v", body)
	}
	if _, has := body["icon"]; has {
		t.Errorf("a dynamic app's logo is returned: %v", body["icon"])
	}
	req := fake.last().(oauth.RevokeAppRequest)
	if req.AppID != dyn.ID || req.OrgID != f.org.ID || req.Reason != oauth.ReasonAdmin || req.Actor != f.userID {
		t.Errorf("RevokeApp got %+v", req)
	}
	if n := fake.liveGrants(dyn.ID); n != 1 {
		t.Errorf("%d grants are live, want only the other organization's", n)
	}

	// Once nothing is left here the call still succeeds, without a time.
	rec = f.do("POST", oauthAppsBase+"/"+dyn.ID+"/revoke", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second revoke: %d %s", rec.Code, rec.Body)
	}
	if _, has := oauthAppsObject(t, rec)["authorized_at"]; has {
		t.Error("the second revoke names a time")
	}

	for _, id := range []string{oauthAppsTestID(), "not-a-uuid"} {
		if rec := f.do("POST", oauthAppsBase+"/"+id+"/revoke", nil); rec.Code != http.StatusNotFound {
			t.Errorf("revoke %s: %d, want 404", id, rec.Code)
		}
	}
}

// ---- input and errors -------------------------------------------------------------------------

func TestOAuthAppsRequestChecks(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	appID, secretID := fake.seedManual(f.org.ID, "Seeded")
	app := oauthAppsBase + "/" + appID
	full := func(drop string) map[string]any {
		m := oauthAppsBody("Bot")
		delete(m, drop)
		return m
	}
	notJSON := "{"

	// Refused by the handler: the Service is not called.
	for _, c := range []struct {
		name         string
		method, path string
		body         any
		status       int
		message      string
	}{
		{"create without name", "POST", oauthAppsBase, full("name"), 400, "name is required"},
		{"create without website", "POST", oauthAppsBase, full("website"), 400, "website is required"},
		{"create without scopes", "POST", oauthAppsBase, full("scopes"), 400, "scopes is required"},
		{"create without redirect_uris", "POST", oauthAppsBase, full("redirect_uris"), 400, "redirect_uris is required"},
		{"create with null scopes", "POST", oauthAppsBase, `{"name":"a","website":"https://a.example.test","scopes":null,"redirect_uris":["https://a.example.test/cb"]}`, 400, "scopes is required"},
		{"create with an empty body", "POST", oauthAppsBase, nil, 400, "name is required"},
		{"create with a number for a name", "POST", oauthAppsBase, `{"name":5}`, 400, "invalid request body"},
		{"create with text for a body", "POST", oauthAppsBase, notJSON, 400, "invalid request body"},
		{"update without name", "PUT", app, full("name"), 400, "name is required"},
		{"update without scopes", "PUT", app, full("scopes"), 400, "scopes is required"},
		{"a body over 64 KiB", "POST", oauthAppsBase, `{"name":"` + strings.Repeat("a", 70<<10) + `"}`, 413, "request body too large"},
		{"list without type", "GET", oauthAppsBase, nil, 400, "type must be published or authorized"},
		{"list with another type", "GET", oauthAppsBase + "?type=all", nil, 400, "type must be published or authorized"},
		{"update a malformed id", "PUT", oauthAppsBase + "/not-a-uuid", oauthAppsBody("Bot"), 404, "OAuth app not found"},
		{"delete a malformed id", "DELETE", oauthAppsBase + "/not-a-uuid", nil, 404, "OAuth app not found"},
		{"list secrets of a malformed id", "GET", oauthAppsBase + "/1/client-secrets", nil, 404, "OAuth app not found"},
		{"create a secret for a malformed id", "POST", oauthAppsBase + "/1/client-secrets", nil, 404, "OAuth app not found"},
		{"delete a malformed secret id", "DELETE", app + "/client-secrets/zzz", nil, 404, "Client secret not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := fake.calls()
			rec := f.do(c.method, c.path, c.body)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, truncate(rec.Body.String(), 200))
			}
			if c.message != "" && oauthAppsObject(t, rec)["message"] != c.message {
				t.Errorf("message = %v, want %q", oauthAppsObject(t, rec)["message"], c.message)
			}
			if fake.calls() != before {
				t.Errorf("the Service was called for a request the handler refuses")
			}
		})
	}

	// Judged by the Service: its text is shown, without its prefix.
	rec := f.do("POST", oauthAppsBase, map[string]any{
		"name": "Bot", "website": "https://a.example.test", "scopes": []string{"nope:read"}, "redirect_uris": []string{"https://a.example.test/cb"},
	})
	if rec.Code != http.StatusBadRequest || oauthAppsObject(t, rec)["message"] != `"nope:read" is not a scope` {
		t.Errorf("invalid scope: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("PUT", app, map[string]any{
		"name": "Bot", "website": "https://a.example.test", "scopes": []string{"projects:read"}, "redirect_uris": []string{"cursor://cb"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(oauthAppsObject(t, rec)["message"].(string), "cursor://cb") {
		t.Errorf("invalid redirect: %d %s", rec.Code, rec.Body)
	}

	// An uppercase id names the same app, and the Service gets it in lower case.
	if rec := f.do("PUT", oauthAppsBase+"/"+strings.ToUpper(appID), oauthAppsBody("Upper")); rec.Code != http.StatusOK {
		t.Fatalf("update by an uppercase id: %d %s", rec.Code, rec.Body)
	}
	if got := fake.last().(oauth.UpdateAppRequest).AppID; got != appID {
		t.Errorf("AppID = %q, want %q", got, appID)
	}
	// An id that is well formed and unknown is a 404, for the app and for the secret.
	if rec := f.do("DELETE", app+"/client-secrets/"+oauthAppsTestID(), nil); rec.Code != http.StatusNotFound || oauthAppsObject(t, rec)["message"] != "Client secret not found" {
		t.Errorf("unknown secret: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", oauthAppsBase+"/"+oauthAppsTestID(), nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app: %d", rec.Code)
	}
	if n := fake.secretCount(appID); n != 1 {
		t.Errorf("the app has %d secrets, want 1 (%s)", n, secretID)
	}
}

// A cap is 429 with Retry-After and the Service's words; a failure of the Service is a 500 whose
// detail stays in the log.
func TestOAuthAppsServiceErrors(t *testing.T) {
	f, fake := oauthAppsFixture(t)
	for i := 1; i < oauth.MaxManualAppsPerOrg; i++ {
		fake.seedManual(f.org.ID, fmt.Sprintf("App %d", i))
	}
	if rec := f.do("POST", oauthAppsBase, oauthAppsBody("Twentieth")); rec.Code != http.StatusCreated {
		t.Fatalf("the 20th app: %d %s", rec.Code, rec.Body)
	}
	rec := f.do("POST", oauthAppsBase, oauthAppsBody("Twenty-first"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("the 21st app: %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if msg := oauthAppsObject(t, rec)["message"]; msg != "an organization can publish at most 20 apps" {
		t.Errorf("message = %v", msg)
	}

	fake.fail["ListPublishedApps"] = errors.New("pq: connection refused by 10.0.0.7:5432")
	rec = f.do("GET", oauthAppsBase+"?type=published", nil)
	if rec.Code != http.StatusInternalServerError || oauthAppsObject(t, rec)["message"] != "Internal server error" || strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("a failing Service: %d %s", rec.Code, rec.Body)
	}
}

// ---- over the real Service --------------------------------------------------------------------

// The same page over oauth.Service and its memory store, with a grant made the way a client makes
// one.
func TestOAuthAppsWithService(t *testing.T) {
	f := newFixture(t)
	svc, ok := f.srv.oauth.(*oauth.Service)
	if !ok {
		t.Fatalf("Server.oauth is %T, want the *oauth.Service the server builds", f.srv.oauth)
	}
	ctx := context.Background()
	// Whether the user may hold grants is the server's own check (oauthAdmit, tested with it).
	svc.Admit = func(context.Context, string, int64) error { return nil }

	rec := f.do("POST", oauthAppsBase, map[string]any{
		"name": "CI deploy bot", "website": "https://ci.example.test",
		"scopes":        []string{"projects:read", "database:read"},
		"redirect_uris": []string{"https://ci.example.test/callback"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := oauthAppsObject(t, rec)
	appID, _ := created["id"].(string)
	firstSecret, _ := created["client_secret"].(string)
	if !strings.HasPrefix(firstSecret, oauth.ClientSecretPrefix) || created["client_id"] != appID {
		t.Fatalf("create answer = %v", created)
	}
	app := oauthAppsBase + "/" + appID

	rows := oauthAppsRows(t, f.do("GET", oauthAppsBase+"?type=published", nil))
	if len(rows) != 1 || rows[0]["id"] != appID {
		t.Fatalf("published = %v", rows)
	}
	rec = f.do("POST", app+"/client-secrets", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second secret: %d %s", rec.Code, rec.Body)
	}
	second := oauthAppsObject(t, rec)
	secretID, _ := second["id"].(string)
	clientSecret, _ := second["client_secret"].(string)
	list, _ := oauthAppsObject(t, f.do("GET", app+"/client-secrets", nil))["client_secrets"].([]any)
	if len(list) != 2 {
		t.Fatalf("client_secrets = %v", list)
	}
	firstID := list[0].(map[string]any)["id"].(string)
	if firstID == secretID {
		firstID = list[1].(map[string]any)["id"].(string)
	}
	if rec := f.do("DELETE", app+"/client-secrets/"+firstID, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete secret: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PUT", app, map[string]any{
		"name": "CI deploy bot 2", "website": "https://ci.example.test",
		"scopes": []string{"projects:read"}, "redirect_uris": []string{"https://ci.example.test/callback"},
	}); rec.Code != http.StatusOK || oauthAppsObject(t, rec)["name"] != "CI deploy bot 2" {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	// A user authorizes the app: authorize, approve, redeem the code with the client secret.
	res, err := svc.StartAuthorization(ctx, oauth.AuthorizeRequest{
		ClientID: appID, ResponseType: oauth.ResponseTypeCode, RedirectURI: "https://ci.example.test/callback",
		Scope: oauth.ScopeProjectsRead, State: "st",
	})
	if err != nil {
		t.Fatalf("StartAuthorization: %v", err)
	}
	approved, err := svc.Approve(ctx, oauth.ApproveRequest{AuthID: res.AuthID, UserID: f.userID, OrgID: f.org.ID, OrgSlug: f.org.Slug})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	u, err := url.Parse(approved.RedirectURL)
	if err != nil || u.Query().Get("code") == "" {
		t.Fatalf("Approve gave %q: %v", approved.RedirectURL, err)
	}
	tok, err := svc.Exchange(ctx, oauth.TokenRequest{
		GrantType: oauth.GrantTypeAuthorizationCode, ClientID: appID, ClientSecret: clientSecret,
		Code: u.Query().Get("code"), RedirectURI: "https://ci.example.test/callback",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	rec = f.do("GET", oauthAppsBase+"?type=authorized", nil)
	validateAgainstSpec(t, oauthAppsListKey, rec.Body.Bytes())
	rows = oauthAppsRows(t, rec)
	if len(rows) != 1 || rows[0]["app_id"] != appID || rows[0]["created_by"] != "Default" || fmt.Sprint(rows[0]["scopes"]) != "[projects:read]" {
		t.Fatalf("authorized = %v", rows)
	}
	if _, err := svc.LookupAccess(ctx, tok.AccessToken); err != nil {
		t.Fatalf("the token does not work before the revoke: %v", err)
	}
	if rec := f.do("POST", app+"/revoke", nil); rec.Code != http.StatusCreated {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if _, err := svc.LookupAccess(ctx, tok.AccessToken); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("the token works after the revoke: %v", err)
	}
	if rows := oauthAppsRows(t, f.do("GET", oauthAppsBase+"?type=authorized", nil)); len(rows) != 0 {
		t.Errorf("authorized after the revoke = %v", rows)
	}
	if rec := f.do("DELETE", app, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rows := oauthAppsRows(t, f.do("GET", oauthAppsBase+"?type=published", nil)); len(rows) != 0 {
		t.Errorf("published after the delete = %v", rows)
	}
}
