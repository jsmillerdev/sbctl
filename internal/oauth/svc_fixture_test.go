package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture of the Service tests: a Service over a MemoryStore with a fake clock, recorders for the
// audit events, the alerts and the log, and helpers that walk the protocol.

const (
	testIssuer    = "https://api.example.test"
	testDashboard = "https://studio.example.test"
	testUserA     = "11111111-1111-4111-8111-111111111111"
	testUserB     = "22222222-2222-4222-8222-222222222222"
	testOrgAcme   = int64(1)
	testOrgOther  = int64(2)
	// testVerifier and testRedirect are the PKCE verifier of RFC 7636 appendix B and a loopback redirect
	// with a port; the registered form has none.
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testRedirect = "http://127.0.0.1:53123/callback"
	testState    = "state-abc 123/&=?"
)

type svcClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *svcClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *svcClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type svcEvent struct {
	Kind    string
	Payload map[string]any
}

// svcBuffer is a bytes.Buffer that is safe to write from the handler and read from the test.
type svcBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *svcBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *svcBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type svcFixture struct {
	t      *testing.T
	svc    *Service
	store  *MemoryStore
	clock  *svcClock
	logbuf *svcBuffer

	mu     sync.Mutex
	events []svcEvent
	alerts []AlertEvent
	// admit, when set, decides who is admitted; the default admits everybody.
	admit      func(userID string, orgID int64) error
	admitCalls int
}

func newSvc(t *testing.T) *svcFixture {
	t.Helper()
	fx := &svcFixture{
		t: t, store: NewMemoryStore(), logbuf: &svcBuffer{},
		clock: &svcClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
	}
	fx.store.SetOrgSlugs(func(id int64) string {
		switch id {
		case testOrgAcme:
			return "acme"
		case testOrgOther:
			return "other"
		}
		return fmt.Sprintf("org-%d", id)
	})
	fx.svc = &Service{
		Store: fx.store, Issuer: testIssuer, DashboardURL: testDashboard, Now: fx.clock.Now,
		Log: slog.New(slog.NewTextHandler(fx.logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Admit: func(_ context.Context, userID string, orgID int64) error {
			fx.mu.Lock()
			fx.admitCalls++
			f := fx.admit
			fx.mu.Unlock()
			if f != nil {
				return f(userID, orgID)
			}
			return nil
		},
		Audit: func(_ context.Context, kind string, payload map[string]any) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.events = append(fx.events, svcEvent{kind, payload})
		},
		Alert: func(_ context.Context, e AlertEvent) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.alerts = append(fx.alerts, e)
		},
	}
	// The lazy prune would delete rows of tests that move the clock by months; TestLazyPrune turns it on.
	fx.svc.lastPrune = fx.clock.t.Add(100 * 365 * 24 * time.Hour)
	return fx
}

// enableLazyPrune makes the next Register or Exchange prune, as on a node that has just started.
func (fx *svcFixture) enableLazyPrune() { fx.svc.lastPrune = time.Time{} }

func (fx *svcFixture) ctx() context.Context { return context.Background() }

func (fx *svcFixture) setAdmit(f func(userID string, orgID int64) error) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.admit = f
}

// eventsOf returns the audit events of one kind, in order.
func (fx *svcFixture) eventsOf(kind string) []svcEvent {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	var out []svcEvent
	for _, e := range fx.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (fx *svcFixture) alertList() []AlertEvent {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]AlertEvent(nil), fx.alerts...)
}

// register registers a dynamic app with the given redirect URIs (default: a loopback one without a port).
func (fx *svcFixture) register(redirects ...string) *RegisteredApp {
	fx.t.Helper()
	if len(redirects) == 0 {
		redirects = []string{"http://127.0.0.1/callback"}
	}
	ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{ClientName: "Test Client", RedirectURIs: redirects})
	if err != nil {
		fx.t.Fatalf("Register: %v", err)
	}
	return ra
}

// manual publishes a manual app of the organization with all the scopes of the advertised set and a
// redirect on example.test; it returns the app and its plaintext secret.
func (fx *svcFixture) manual(org int64) (*App, string) {
	fx.t.Helper()
	ca, err := fx.svc.CreateApp(fx.ctx(), CreateAppRequest{
		OrgID: org, CreatedBy: testUserA, Name: "Manual App", Website: "https://app.example.test",
		Scopes: AdvertisedScopes, RedirectURIs: []string{"https://app.example.test/callback"},
	})
	if err != nil {
		fx.t.Fatalf("CreateApp: %v", err)
	}
	return &ca.App, ca.ClientSecret
}

// authorizeReq is a valid dynamic-app request for the app: PKCE with testVerifier, a state, the loopback
// redirect with a port.
func authorizeReq(app *App) AuthorizeRequest {
	redirect := app.RedirectURIs[0]
	if strings.HasPrefix(redirect, "http://127.0.0.1") {
		redirect = testRedirect
	}
	return AuthorizeRequest{
		ClientID: app.ID, ResponseType: ResponseTypeCode, RedirectURI: redirect, State: testState,
		CodeChallenge: pkceChallengeS256(testVerifier), CodeChallengeMethod: PKCEMethodS256,
	}
}

func (fx *svcFixture) start(req AuthorizeRequest) string {
	fx.t.Helper()
	res, err := fx.svc.StartAuthorization(fx.ctx(), req)
	if err != nil {
		fx.t.Fatalf("StartAuthorization: %v", err)
	}
	return res.AuthID
}

// approve approves the request as the user in the organization and returns the redirect URL's parts.
func (fx *svcFixture) approve(authID, user string, org int64) (redirect *url.URL, code string) {
	fx.t.Helper()
	slug := fx.store.slug(org)
	res, err := fx.svc.Approve(fx.ctx(), ApproveRequest{AuthID: authID, UserID: user, OrgID: org, OrgSlug: slug})
	if err != nil {
		fx.t.Fatalf("Approve: %v", err)
	}
	u, err := url.Parse(res.RedirectURL)
	if err != nil {
		fx.t.Fatal(err)
	}
	return u, u.Query().Get("code")
}

// codeRequest is the token request that redeems the code of req for the dynamic app (no secret, PKCE).
func codeRequest(ra *RegisteredApp, req AuthorizeRequest, code string) TokenRequest {
	return TokenRequest{
		GrantType: GrantTypeAuthorizationCode, ClientID: ra.App.ID, Code: code,
		RedirectURI: req.RedirectURI, CodeVerifier: testVerifier,
	}
}

// grantFlow walks authorize, approve and exchange for a dynamic app.
type grantFlow struct {
	App    *RegisteredApp
	Req    AuthorizeRequest
	AuthID string
	Code   string
	Tokens *TokenResponse
}

func (fx *svcFixture) grant(user string, org int64) *grantFlow {
	fx.t.Helper()
	return fx.grantFor(fx.register(), user, org)
}

func (fx *svcFixture) grantFor(ra *RegisteredApp, user string, org int64) *grantFlow {
	fx.t.Helper()
	req := authorizeReq(&ra.App)
	f := &grantFlow{App: ra, Req: req, AuthID: fx.start(req)}
	_, f.Code = fx.approve(f.AuthID, user, org)
	tr, err := fx.svc.Exchange(fx.ctx(), codeRequest(ra, req, f.Code))
	if err != nil {
		fx.t.Fatalf("Exchange: %v", err)
	}
	f.Tokens = tr
	return f
}

// refreshRequest refreshes the flow's current refresh token as the dynamic app (no secret needed when
// the app registered with auth method none; the fixture's default method needs the secret).
func (f *grantFlow) refreshRequest(refresh string) TokenRequest {
	return TokenRequest{GrantType: GrantTypeRefreshToken, ClientID: f.App.App.ID, ClientSecret: f.App.ClientSecret, RefreshToken: refresh}
}

// oauthErr returns the *Error inside err, or fails the test.
func oauthErr(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want an *oauth.Error, got %T %v", err, err)
	}
	return e
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got success", code)
	}
	if e := oauthErr(t, err); e.Code != code {
		t.Fatalf("want error code %s, got %s (%s)", code, e.Code, e.Description)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
