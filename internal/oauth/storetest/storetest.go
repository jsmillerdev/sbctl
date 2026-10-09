// Package storetest holds the contract tests that every oauth.Store implementation runs: the
// Postgres store (store_pg_test.go, when SUPAVISE_TEST_DATABASE_URL is set) and the memory store
// (store_mem_test.go). They are the definition of how a Store behaves; the Service relies on
// nothing else.
//
// The tests assert what store.go documents and nothing more, so a Store may differ in the places
// it leaves open (the error text, the order of rows no sort key covers, the counts a cascade adds
// to a PruneResult). The one thing a Store cannot do alone is know organizations: the Postgres
// store joins supavise.organizations for the slugs of OrgSlug. A store under test that keeps
// organizations therefore implements OrgSeeder; the tests then create two and check every slug. A
// store that does not is given the organization ids 1 and 2, and the slug checks are skipped.
package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/secrets"
)

// OrgSeeder is implemented by the Store a test builds when that store keeps organizations. SeedOrg
// creates an organization with this slug and returns its id; the id is what Grant.OrgID,
// Decision.OrgID and the other organization fields then carry, and slug is what OrgSlug must show.
type OrgSeeder interface {
	SeedOrg(ctx context.Context, slug string) (int64, error)
}

// Run runs the contract tests against stores that newStore makes. newStore returns a new, empty
// store on every call and cleans up with t.Cleanup; Run calls it once per test, so tests do not
// see each other's rows. The subtests are named for what they prove; the concurrent ones
// (CodeSingleUseConcurrent, DecideConcurrent, RefreshConcurrent) are the plan's C2 and T2 and are
// worth running with -race.
func Run(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(*testing.T, *env)
	}{
		{"AppLifecycle", testAppLifecycle},
		{"AppListsAndCounts", testAppListsAndCounts},
		{"DeleteApp", testDeleteApp},
		{"Secrets", testSecrets},
		{"AuthorizationDecide", testAuthorizationDecide},
		{"DecideConcurrent", testDecideConcurrent},
		{"WithCode", testWithCode},
		{"CompleteIssuesGrant", testCompleteIssuesGrant},
		{"CompleteSupersedes", testCompleteSupersedes},
		{"CodeSingleUseConcurrent", testCodeSingleUseConcurrent},
		{"LookupAccess", testLookupAccess},
		{"Grants", testGrants},
		{"RefreshRotation", testRefreshRotation},
		{"RefreshReuse", testRefreshReuse},
		{"RefreshRollback", testRefreshRollback},
		{"RefreshConcurrent", testRefreshConcurrent},
		{"PruneNothing", testPruneNothing},
		{"PruneAuthorizations", testPruneAuthorizations},
		{"PruneTokens", testPruneTokens},
		{"PruneGrants", testPruneGrants},
		{"PruneUnusedApps", testPruneUnusedApps},
		{"PruneIdleApps", testPruneIdleApps},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			c.fn(t, newEnv(t, newStore(t)))
		})
	}
}

// env is what one test works with: a store, a clock origin and the organizations.
type env struct {
	t   *testing.T
	st  oauth.Store
	ctx context.Context
	// t0 is the origin of the test's clock: every time in a test is t0 plus an offset, and has no
	// fraction of a microsecond, which Postgres would round away.
	t0   time.Time
	step int
	// orgA and orgB are two organizations. slugs maps their ids to their slugs when the store keeps
	// organizations (OrgSeeder); it is nil otherwise.
	orgA, orgB int64
	slugs      map[int64]string
}

func newEnv(t *testing.T, st oauth.Store) *env {
	t.Helper()
	e := &env{t: t, st: st, ctx: context.Background(), t0: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), orgA: 1, orgB: 2}
	if s, ok := st.(OrgSeeder); ok {
		e.slugs = map[int64]string{}
		var err error
		if e.orgA, err = s.SeedOrg(e.ctx, "acme"); err != nil {
			t.Fatalf("SeedOrg(acme): %v", err)
		}
		if e.orgB, err = s.SeedOrg(e.ctx, "beta"); err != nil {
			t.Fatalf("SeedOrg(beta): %v", err)
		}
		e.slugs[e.orgA], e.slugs[e.orgB] = "acme", "beta"
	}
	return e
}

// must fails the test now if err is not nil.
func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
}

// wantErr fails the test now unless err matches target (errors.Is).
func (e *env) wantErr(err, target error) {
	e.t.Helper()
	if !errors.Is(err, target) {
		e.t.Fatalf("error = %v, want %v", err, target)
	}
}

// next returns a new instant, one minute after the one before.
func (e *env) next() time.Time {
	e.step++
	return e.t0.Add(time.Duration(e.step) * time.Minute)
}

// checkSlug checks the slug a store returned for org, when the store knows organizations.
func (e *env) checkSlug(what, got string, org int64) {
	e.t.Helper()
	if e.slugs != nil && got != e.slugs[org] {
		e.t.Errorf("%s: org slug = %q, want %q", what, got, e.slugs[org])
	}
}

// hash returns a new SHA-256 digest of random bytes: what the Service stores for a secret.
func hash() []byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b[:])
	return sum[:]
}

// ---- fixtures

func (e *env) dynamicApp() oauth.App {
	return oauth.App{
		ID: secrets.NewUUID(), RegistrationType: oauth.RegistrationDynamic, Name: "Dynamic client", Website: "https://client.example",
		Icon:                    "https://client.example/logo.png",
		RedirectURIs:            []string{"http://127.0.0.1/callback", "https://client.example/cb"},
		Scopes:                  []string{"projects:read", "database:read", "organizations:read"},
		TokenEndpointAuthMethod: oauth.AuthMethodNone, CreatedAt: e.t0, UpdatedAt: e.t0,
	}
}

func (e *env) manualApp(org int64) oauth.App {
	a := e.dynamicApp()
	a.RegistrationType, a.OrgID, a.Name = oauth.RegistrationManual, org, "Published app"
	a.TokenEndpointAuthMethod, a.CreatedBy = oauth.AuthMethodBasic, secrets.NewUUID()
	return a
}

func (e *env) secret(appID string) oauth.AppSecret {
	return oauth.AppSecret{ID: secrets.NewUUID(), AppID: appID, Alias: "sba_1a2b********", Hash: hash(), CreatedBy: secrets.NewUUID(), CreatedAt: e.t0}
}

// createApp stores a new dynamic app and returns it.
func (e *env) createApp() oauth.App {
	e.t.Helper()
	a := e.dynamicApp()
	e.must(e.st.CreateApp(e.ctx, a, nil))
	return a
}

// flow describes one authorization, from the request to the redeemed code.
type flow struct {
	app oauth.App
	// user, org, at, scopes: defaults are a new user, org A, e.next() and two scopes.
	user     string
	org      int64
	at       time.Time
	scopes   []string
	resource string
	maxLive  int
}

// granted is what a flow made.
type granted struct {
	grant      oauth.Grant
	authID     string
	code       []byte
	access     oauth.Token // as given to Complete
	refresh    oauth.Token
	superseded []oauth.Grant
	at         time.Time
}

// grant runs a whole authorization through the store: create the request, approve it, redeem the
// code with Complete. at is the instant of the redemption.
func (e *env) grant(f flow) granted {
	e.t.Helper()
	if f.at.IsZero() {
		f.at = e.next()
	}
	if f.user == "" {
		f.user = secrets.NewUUID()
	}
	if f.org == 0 {
		f.org = e.orgA
	}
	if f.scopes == nil {
		f.scopes = []string{"projects:read", "database:read"}
	}
	g := granted{authID: secrets.NewUUID(), code: hash(), at: f.at}
	created := f.at.Add(-3 * time.Second)
	e.must(e.st.CreateAuthorization(e.ctx, oauth.Authorization{
		ID: g.authID, AppID: f.app.ID, RedirectURI: "http://127.0.0.1:5000/callback", Scopes: f.scopes, State: "st",
		CodeChallenge: "ch", Resource: f.resource, CreatedAt: created, ExpiresAt: created.Add(oauth.AuthorizationTTL),
	}))
	decided := f.at.Add(-time.Second)
	_, err := e.st.DecideAuthorization(e.ctx, g.authID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: f.user, At: decided, OrgID: f.org, CodeHash: g.code, CodeExpiresAt: decided.Add(oauth.AuthCodeTTL),
	})
	e.must(err)
	g.access = oauth.Token{Hash: hash(), Prefix: "sbp_oaut", CreatedAt: f.at, ExpiresAt: f.at.Add(oauth.AccessTokenTTL)}
	g.refresh = oauth.Token{Hash: hash(), Prefix: "sbr_abcd", CreatedAt: f.at, ExpiresAt: f.at.Add(oauth.RefreshTokenTTL)}
	e.must(e.st.WithCode(e.ctx, f.app.ID, g.code, func(ctx context.Context, tx oauth.CodeTx) error {
		res, err := tx.Complete(ctx, oauth.NewGrant{
			Grant:  oauth.Grant{AppID: f.app.ID, UserID: f.user, OrgID: f.org, Scopes: f.scopes, Resource: f.resource, CreatedAt: f.at},
			Access: g.access, Refresh: g.refresh, MaxLive: f.maxLive, At: f.at,
		})
		if err != nil {
			return err
		}
		g.grant, g.superseded = res.Grant, res.Superseded
		return nil
	}))
	return g
}

// approve makes a pending authorization of the app and approves it, without redeeming the code.
func (e *env) approve(app oauth.App, user string, org int64, at time.Time) (oauth.Authorization, []byte) {
	e.t.Helper()
	a := oauth.Authorization{
		ID: secrets.NewUUID(), AppID: app.ID, RedirectURI: "http://127.0.0.1:5000/callback", Scopes: []string{"projects:read", "database:read"},
		State: "st", CodeChallenge: "ch", Resource: "https://api.example.com/mcp", OrgHint: "acme",
		CreatedAt: at.Add(-time.Second), ExpiresAt: at.Add(-time.Second + oauth.AuthorizationTTL), Status: oauth.StatusPending,
	}
	e.must(e.st.CreateAuthorization(e.ctx, a))
	code := hash()
	got, err := e.st.DecideAuthorization(e.ctx, a.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: user, At: at, OrgID: org, CodeHash: code, CodeExpiresAt: at.Add(oauth.AuthCodeTTL),
	})
	e.must(err)
	return *got, code
}
