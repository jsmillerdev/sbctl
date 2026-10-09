package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// mcpAuthority is the OAuth service as the gate sees it: it knows a few access tokens, and ends a
// grant by forgetting its tokens, as the real one does. Only the methods the gate calls are defined;
// any other call panics on the nil Authority, which is what the test wants.
type mcpAuthority struct {
	oauth.Authority
	mu      sync.Mutex
	tokens  map[string]*oauth.AccessInfo
	err     error // returned by LookupAccess when set
	lookups int
	revoked []mcpRevocation
}

type mcpRevocation struct {
	grant         int64
	reason, actor string
}

func (a *mcpAuthority) LookupAccess(_ context.Context, token string) (*oauth.AccessInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lookups++
	if a.err != nil {
		return nil, a.err
	}
	if info, ok := a.tokens[token]; ok {
		cp := *info
		return &cp, nil
	}
	return nil, oauth.ErrNotFound
}

func (a *mcpAuthority) RevokeGrant(_ context.Context, id int64, reason, actor string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked = append(a.revoked, mcpRevocation{id, reason, actor})
	for tok, info := range a.tokens {
		if info.GrantID == id {
			delete(a.tokens, tok)
		}
	}
	return nil
}

func (a *mcpAuthority) add(token string, info oauth.AccessInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = &info
}

func (a *mcpAuthority) lookupCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lookups
}

func (a *mcpAuthority) revocations() []mcpRevocation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]mcpRevocation(nil), a.revoked...)
}

const (
	mcpResourceMetadata = "https://api.example.test/.well-known/oauth-protected-resource/mcp"
	mcpResource         = "https://api.example.test/mcp"
	mcpBadChallenge     = `Bearer error="invalid_token", error_description="The access token is invalid, expired or revoked", resource_metadata="` + mcpResourceMetadata + `"`
)

// mcpFixture is a fixture whose OAuth service is an mcpAuthority, with one OAuth access token and
// one personal access token of the signed-in user, who owns the fixture's organization.
type mcpFixture struct {
	*fixture
	tt    *testing.T
	auth  *mcpAuthority
	oauth string // a live OAuth access token of f.userID in f.org, grant 7
	pat   string // a live personal access token of f.userID
	patID int64
	now   time.Time
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	f := &mcpFixture{fixture: newFixture(t), tt: t, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	f.auth = &mcpAuthority{tokens: map[string]*oauth.AccessInfo{}}
	f.srv.oauth = f.auth
	f.srv.now = func() time.Time { return f.now }
	f.oauth = "sbp_oauth_" + strings.Repeat("a1", 20)
	f.auth.add(f.oauth, f.info(7, f.userID))
	f.pat = secrets.NewPAT()
	f.patID = f.newPAT(f.userID, f.pat, nil)
	return f
}

// info is the AccessInfo of a live token of grant for user in the fixture's organization.
func (f *mcpFixture) info(grant int64, user string) oauth.AccessInfo {
	return oauth.AccessInfo{TokenID: grant * 10, GrantID: grant, AppID: "app", AppName: "Client", UserID: user, OrgID: f.org.ID, OrgSlug: f.org.Slug,
		ExpiresAt: f.now.Add(time.Hour), Resource: mcpResource, Scopes: []string{"projects:read"}}
}

func (f *mcpFixture) newPAT(user, token string, expires *time.Time) int64 {
	f.t.Helper()
	if err := f.reg.CreateAccessToken(context.Background(), &registry.AccessToken{UserID: user, Name: "pat", Hash: secrets.HashToken(token), Prefix: token[:8], ExpiresAt: expires}); err != nil {
		f.t.Fatal(err)
	}
	at, err := f.reg.GetAccessTokenByHash(context.Background(), secrets.HashToken(token))
	if err != nil {
		f.t.Fatal(err)
	}
	return at.ID
}

// settle waits until the gate has given back the slot of every request that is done: it does so in the
// goroutine that the request context's cancellation starts.
func (f *mcpFixture) settle() {
	f.tt.Helper()
	l := f.srv.mcpLimiter()
	idle := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, w := range l.callers {
			if w.inFlight > 0 {
				return false
			}
		}
		return true
	}
	for deadline := time.Now().Add(5 * time.Second); !idle(); runtime.Gosched() {
		if time.Now().After(deadline) {
			f.tt.Fatal("timed out waiting for the slots of finished requests to be given back")
		}
	}
}

// gated is what one call of the gate did.
type gated struct {
	rec   *httptest.ResponseRecorder
	req   *http.Request
	query string
	ok    bool
	done  context.CancelFunc // the proxy has served the request
}

func (g gated) body() string { return strings.TrimSpace(g.rec.Body.String()) }

// gate calls MCPGate for a request to target with the given headers (alternating name, value).
func (f *mcpFixture) gate(method, target, body string, headers ...string) gated {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	req := httptest.NewRequestWithContext(ctx, method, "https://api.example.test"+target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	q, ok := f.srv.MCPGate(rec, req)
	return gated{rec: rec, req: req, query: q, ok: ok, done: cancel}
}

// mcpAuthz is the headers of a request that carries token.
func mcpAuthz(token string) []string { return []string{"Authorization", "Bearer " + token} }

// M1: no token is the discovery challenge, a token that is not good is invalid_token, and neither
// goes on to Studio.
func TestMCPGateAuth(t *testing.T) {
	f := newMCPFixture(t)
	expired := f.now.Add(-time.Minute)
	f.newPAT(f.userID, "sbp_"+strings.Repeat("e5", 20), &expired)
	f.auth.add("sbp_oauth_"+strings.Repeat("e5", 20), func() oauth.AccessInfo { i := f.info(8, f.userID); i.ExpiresAt = expired; return i }())

	t.Run("no token", func(t *testing.T) {
		for _, hdr := range [][]string{nil, {"Authorization", ""}, {"Authorization", "Bearer"}, {"Authorization", "Bearer  "}, {"Authorization", "Basic dXNlcjpwYXNz"},
			{"Authorization", f.oauth}, {"X-Authorization", "Bearer " + f.oauth}, {"Cookie", "sb-access-token=" + f.jwt}} {
			g := f.gate("POST", "/mcp?access_token="+f.oauth, `{}`, hdr...)
			if g.ok || g.rec.Code != 401 || g.body() != `{"message":"No access token provided"}` {
				t.Errorf("%v: ok=%v %d %q", hdr, g.ok, g.rec.Code, g.body())
			}
			// Exactly hosted's answer.
			if got := g.rec.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Bearer resource_metadata="`+mcpResourceMetadata+`"` {
				t.Errorf("%v: WWW-Authenticate = %q", hdr, got)
			}
			if g.rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("content type %q", g.rec.Header().Get("Content-Type"))
			}
		}
		if f.auth.lookupCount() != 0 {
			t.Error("a request with no token reached the token lookup")
		}
	})

	t.Run("bad token", func(t *testing.T) {
		before := f.auth.lookupCount()
		for name, tok := range map[string]string{
			"dashboard session":       f.jwt,
			"random":                  "not-a-token",
			"unknown oauth token":     "sbp_oauth_" + strings.Repeat("b2", 20),
			"short oauth token":       "sbp_oauth_abc",
			"upper-case oauth token":  "sbp_oauth_" + strings.Repeat("A1", 20),
			"unknown personal token":  "sbp_" + strings.Repeat("c3", 20),
			"personal token, bad hex": "sbp_" + strings.Repeat("zz", 20),
			"refresh token":           "sbr_" + strings.Repeat("d4", 32),
			"authorization code":      "sbc_" + strings.Repeat("d4", 32),
			"client secret":           "sba_" + strings.Repeat("d4", 32),
			"expired oauth token":     "sbp_oauth_" + strings.Repeat("e5", 20),
			"expired personal token":  "sbp_" + strings.Repeat("e5", 20),
		} {
			g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...)
			if g.ok || g.rec.Code != 401 || g.body() != `{"message":"The access token is invalid, expired or revoked"}` {
				t.Errorf("%s: ok=%v %d %q", name, g.ok, g.rec.Code, g.body())
			}
			if got := g.rec.Header().Get("WWW-Authenticate"); got != mcpBadChallenge {
				t.Errorf("%s: WWW-Authenticate = %q", name, got)
			}
		}
		// Only the two OAuth tokens of the right shape are looked up: nothing else is worth a query.
		if n := f.auth.lookupCount() - before; n != 2 {
			t.Errorf("%d token lookups, want 2", n)
		}
	})

	t.Run("good tokens", func(t *testing.T) {
		for name, tok := range map[string]string{"oauth": f.oauth, "personal access token": f.pat} {
			g := f.gate("POST", "/mcp?project_ref=abcdefghijklmnopqrst", `{}`, mcpAuthz(tok)...)
			if !g.ok || g.rec.Code != 200 || g.query != "project_ref=abcdefghijklmnopqrst" {
				t.Errorf("%s: ok=%v %d %q query %q", name, g.ok, g.rec.Code, g.body(), g.query)
			}
			if g.rec.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("%s: a challenge on success", name)
			}
		}
		// The scheme is case-insensitive; the token is not trimmed of anything but spaces.
		if g := f.gate("GET", "/mcp", "", "Authorization", "bearer "+f.oauth); !g.ok {
			t.Errorf("lower-case scheme: %d %q", g.rec.Code, g.body())
		}
	})

	t.Run("the lookup failing refuses", func(t *testing.T) {
		f.auth.mu.Lock()
		f.auth.err = errors.New("registry unreachable")
		f.auth.mu.Unlock()
		defer func() { f.auth.mu.Lock(); f.auth.err = nil; f.auth.mu.Unlock() }()
		g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
		if g.ok || g.rec.Code != 500 || g.rec.Header().Get("WWW-Authenticate") != "" || strings.Contains(g.body(), "unreachable") {
			t.Errorf("ok=%v %d %q: the gate must fail closed and say nothing of the cause", g.ok, g.rec.Code, g.body())
		}
	})

	// The gate reads; it never writes (it also runs on followers, whose registry is read-only).
	if at, err := f.reg.GetAccessTokenByHash(context.Background(), secrets.HashToken(f.pat)); err != nil || at.LastUsedAt != nil {
		t.Errorf("the gate touched the token: %+v, %v", at, err)
	}
}

// The checks every principal passes at the API apply at the gate too, so that a token whose owner is
// gone is refused at the first request, before any tool makes a Management API call.
func TestMCPGateUserChecks(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	const dev = "aaaaaaaa-1111-4222-8333-444444444444"
	f.addMember(dev, members.RoleDeveloper)
	devOAuth := "sbp_oauth_" + strings.Repeat("d1", 20)
	f.auth.add(devOAuth, f.info(9, dev))
	devPAT := secrets.NewPAT()
	f.newPAT(dev, devPAT, nil)

	for _, tok := range []string{devOAuth, devPAT} {
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...); !g.ok {
			t.Fatalf("a Developer's token is good for the gate (the API limits what it does): %d %q", g.rec.Code, g.body())
		}
	}

	// SSO no longer admits the user.
	was := f.srv.auth.ssoUser
	f.srv.auth.ssoUser = func(context.Context, string) error { return errSSOPending }
	for _, tok := range []string{devOAuth, devPAT} {
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...); g.ok || g.rec.Code != 401 || g.rec.Header().Get("WWW-Authenticate") != mcpBadChallenge {
			t.Errorf("SSO denial: ok=%v %d %q", g.ok, g.rec.Code, g.body())
		}
	}
	// A failure to find out is not a refusal of the user, and not an acceptance either.
	f.srv.auth.ssoUser = func(context.Context, string) error { return errors.New("registry unreachable") }
	for _, tok := range []string{devOAuth, devPAT} {
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...); g.ok || g.rec.Code != 500 {
			t.Errorf("SSO check failing: ok=%v %d %q", g.ok, g.rec.Code, g.body())
		}
	}
	f.srv.auth.ssoUser = was
	if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(devOAuth)...); !g.ok {
		t.Fatalf("control: %d %q", g.rec.Code, g.body())
	}

	// Removed with `supavise users remove`.
	if err := f.srv.accounts.Store.MarkUserRemoved(ctx, dev, "dev@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{devOAuth, devPAT} {
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...); g.ok || g.rec.Code != 401 {
			t.Errorf("removed user: ok=%v %d %q", g.ok, g.rec.Code, g.body())
		}
	}
	if len(f.auth.revocations()) != 0 {
		t.Errorf("a removed user's grants are revoked by the removal, not by the gate: %+v", f.auth.revocations())
	}

	// Left the organization: 401, and the grant goes (reason membership), so that it stays dead.
	const stranger = "bbbbbbbb-1111-4222-8333-444444444444"
	strangerOAuth := "sbp_oauth_" + strings.Repeat("f6", 20)
	f.auth.add(strangerOAuth, f.info(10, stranger))
	g := f.gate("POST", "/mcp", `{}`, mcpAuthz(strangerOAuth)...)
	if g.ok || g.rec.Code != 401 || g.rec.Header().Get("WWW-Authenticate") != mcpBadChallenge {
		t.Errorf("non-member: ok=%v %d %q", g.ok, g.rec.Code, g.body())
	}
	if rev := f.auth.revocations(); len(rev) != 1 || rev[0] != (mcpRevocation{10, oauth.ReasonMembership, oauth.ActorSystem}) {
		t.Errorf("revocations = %+v, want grant 10 for membership by the system", rev)
	}
	if _, err := f.auth.LookupAccess(ctx, strangerOAuth); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("the grant of a user who left still works: %v", err)
	}

	// Revoked in Studio, by the client, by the operator: the token is unknown from then on.
	if err := f.auth.RevokeGrant(ctx, 7, oauth.ReasonAdmin, "someone"); err != nil {
		t.Fatal(err)
	}
	if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); g.ok || g.rec.Code != 401 || g.rec.Header().Get("WWW-Authenticate") != mcpBadChallenge {
		t.Errorf("revoked grant: ok=%v %d %q", g.ok, g.rec.Code, g.body())
	}
}

// M5: a grant bound to a resource is good for that resource only.
func TestMCPResourceBinding(t *testing.T) {
	f := newMCPFixture(t)
	for resource, ok := range map[string]bool{
		"":                              true, // the client sent none
		mcpResource:                     true,
		"https://api.example.test":      false, // the API root is a resource of its own
		"https://api.example.test/mcp/": false,
		"https://api.example.test/v1":   false,
		"https://evil.example/mcp":      false,
		"http://api.example.test/mcp":   false,
	} {
		info := f.info(20, f.userID)
		info.Resource = resource
		tok := "sbp_oauth_" + strings.Repeat("c7", 20)
		f.auth.add(tok, info)
		g := f.gate("POST", "/mcp", `{}`, mcpAuthz(tok)...)
		switch {
		case ok && !g.ok:
			t.Errorf("resource %q refused: %d %q", resource, g.rec.Code, g.body())
		case !ok && (g.ok || g.rec.Code != 401 ||
			g.rec.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token", error_description="The access token was issued for another resource", resource_metadata="`+mcpResourceMetadata+`"`):
			t.Errorf("resource %q: ok=%v %d %v", resource, g.ok, g.rec.Code, g.rec.Header())
		}
	}
	// A personal access token has no resource.
	if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.pat)...); !g.ok {
		t.Errorf("personal access token: %d", g.rec.Code)
	}
	// The issuer comes from the configuration: a public_url moves the resource with it.
	f.cfg.API.PublicURL = "https://mcp-host.example.test"
	if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); g.ok {
		t.Error("a grant for the old issuer's resource is good for the new one")
	}
}

// M6: 600 requests a minute and 8 at a time for each grant or personal access token.
func TestMCPLimits(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		f := newMCPFixture(t)
		other := "sbp_oauth_" + strings.Repeat("0f", 20)
		f.auth.add(other, f.info(8, f.userID))
		for i := 0; i < mcpRatePerMinute; i++ {
			g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
			if !g.ok {
				t.Fatalf("request %d: %d %q", i+1, g.rec.Code, g.body())
			}
			g.done() // served at once: the rate is what is tested
			f.settle()
			if i%100 == 99 {
				f.now = f.now.Add(time.Second)
			}
		}
		// 6 s have passed; the window opened at the first request, so 54 s remain.
		g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
		if g.ok || g.rec.Code != 429 || g.rec.Header().Get("Retry-After") != "54" || g.body() != `{"message":"Too many requests"}` {
			t.Errorf("601st: ok=%v %d %q Retry-After %q", g.ok, g.rec.Code, g.body(), g.rec.Header().Get("Retry-After"))
		}
		if g.rec.Header().Get("WWW-Authenticate") != "" || g.rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("a 429 is not a challenge and is readable by browsers: %v", g.rec.Header())
		}
		// Each grant and each personal access token has its own count.
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(other)...); !g.ok {
			t.Errorf("another grant: %d", g.rec.Code)
		}
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.pat)...); !g.ok {
			t.Errorf("a personal access token: %d", g.rec.Code)
		}
		// A refused request counts for nothing, and the window ends on time.
		f.now = f.now.Add(53*time.Second + 500*time.Millisecond)
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); g.ok || g.rec.Header().Get("Retry-After") != "1" {
			t.Errorf("half a second before the window ends: ok=%v Retry-After %q", g.ok, g.rec.Header().Get("Retry-After"))
		}
		f.now = f.now.Add(500 * time.Millisecond)
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); !g.ok {
			t.Errorf("a new window: %d %q", g.rec.Code, g.body())
		}
		// Requests that are not authenticated use no one's allowance.
		for i := 0; i < mcpRatePerMinute+10; i++ {
			if g := f.gate("POST", "/mcp", `{}`, mcpAuthz("sbp_oauth_"+strings.Repeat("99", 20))...); g.rec.Code != 401 {
				t.Fatalf("bad token %d: %d", i, g.rec.Code)
			}
		}
		if len(f.srv.mcpLimiter().callers) != 3 {
			t.Errorf("the limiter holds %d callers, want 3", len(f.srv.mcpLimiter().callers))
		}
	})

	t.Run("in flight", func(t *testing.T) {
		f := newMCPFixture(t)
		var open []gated
		for i := 0; i < mcpMaxInFlight; i++ {
			g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
			if !g.ok {
				t.Fatalf("request %d: %d %q", i+1, g.rec.Code, g.body())
			}
			open = append(open, g)
		}
		g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
		if g.ok || g.rec.Code != 429 || g.rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("9th at a time: ok=%v %d Retry-After %q", g.ok, g.rec.Code, g.rec.Header().Get("Retry-After"))
		}
		// Another principal is not held up.
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.pat)...); !g.ok {
			t.Errorf("another principal: %d", g.rec.Code)
		}
		// A request is done when the server cancels its context after the proxy has served it.
		open[3].done()
		var again gated
		waitFor(t, "a slot to be given back", func() bool {
			again = f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...)
			return again.ok
		})
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); g.ok {
			t.Error("one slot was given back twice")
		}
		for _, o := range open {
			o.done()
		}
		again.done()
		waitFor(t, "every slot to be given back", func() bool {
			l := f.srv.mcpLimiter()
			l.mu.Lock()
			defer l.mu.Unlock()
			n := 0
			for _, w := range l.callers {
				n += w.inFlight
			}
			return n == 1 // the other principal's request is still open
		})
	})

	t.Run("refused requests give the slot back", func(t *testing.T) {
		f := newMCPFixture(t)
		for i := 0; i < 3*mcpMaxInFlight; i++ {
			for _, target := range []string{"/mcp?read_only=true&read_only=false", "/mcp?x=%zz"} {
				if g := f.gate("POST", target, `{}`, mcpAuthz(f.oauth)...); g.ok || g.rec.Code != 400 {
					t.Fatalf("%s: ok=%v %d", target, g.ok, g.rec.Code)
				}
			}
		}
		if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); !g.ok {
			t.Errorf("the refusals kept their slots: %d %q", g.rec.Code, g.body())
		}
	})

	t.Run("memory", func(t *testing.T) {
		l := &mcpLimiter{}
		now := time.Unix(1000, 0)
		var rel []func()
		for i := 0; i < mcpMaxCallers; i++ {
			r, _, ok := l.acquire(fmt.Sprintf("t%d", i), now)
			if !ok {
				t.Fatal(i)
			}
			rel = append(rel, r)
		}
		// Every slot is a live caller: a new one is admitted and not stored.
		r, _, ok := l.acquire("new", now)
		if !ok || len(l.callers) != mcpMaxCallers {
			t.Fatalf("ok=%v callers=%d", ok, len(l.callers))
		}
		r()
		// Once their windows are over and the requests done, the old entries make room.
		for _, r := range rel[:10] {
			r()
		}
		later := now.Add(2 * mcpRateWindow)
		if _, _, ok := l.acquire("newer", later); !ok || len(l.callers) > mcpMaxCallers-10+1 {
			t.Errorf("ok=%v callers=%d", ok, len(l.callers))
		}
	})
}

// The query that goes to Studio is rebuilt from the four parameters its route reads.
func TestMCPQuery(t *testing.T) {
	f := newMCPFixture(t)
	long := strings.Repeat("x", mcpMaxParam+1)
	for _, tc := range []struct {
		target, want string // want "" is no query; status 0 is accepted
		status       int
		msg          string
	}{
		{target: "/mcp", want: ""},
		{target: "/mcp?", want: ""},
		{target: "/mcp?project_ref=abcdefghijklmnopqrst", want: "project_ref=abcdefghijklmnopqrst"},
		{target: "/mcp?skip_elicitations=true&features=database,docs&read_only=true&project_ref=abc",
			want: "features=database%2Cdocs&project_ref=abc&read_only=true&skip_elicitations=true"},
		// Everything else is dropped, a token in the URL first.
		{target: "/mcp?access_token=sbp_oauth_x&token=t&code=c&state=s&foo=bar&project_ref=abc&Project_Ref=ABC", want: "project_ref=abc"},
		{target: "/mcp?access_token=sbp_oauth_x", want: ""},
		// Studio's route decides what a value means; the gate passes it unchanged and does not guess.
		{target: "/mcp?read_only=&features=", want: "features=&read_only="},
		{target: "/mcp?read_only=TRUE&project_ref=a%20b%26c", want: "project_ref=a+b%26c&read_only=TRUE"},
		{target: "/mcp?project_ref", want: "project_ref="},
		// A parameter that comes twice is not resolved by picking one.
		{target: "/mcp?read_only=false&read_only=true", status: 400, msg: "Query parameter read_only was given more than once"},
		{target: "/mcp?read_only=true&read_only=false", status: 400, msg: "Query parameter read_only was given more than once"},
		{target: "/mcp?features=a&features=b", status: 400, msg: "Query parameter features was given more than once"},
		{target: "/mcp?project_ref=" + long, status: 400, msg: "Query parameter project_ref is too long"},
		{target: "/mcp?project_ref=a;read_only=false", status: 400, msg: "Invalid query string"},
		{target: "/mcp?project_ref=%zz", status: 400, msg: "Invalid query string"},
	} {
		g := f.gate("POST", tc.target, `{}`, mcpAuthz(f.oauth)...)
		g.done()
		f.settle()
		if tc.status == 0 {
			if !g.ok || g.query != tc.want {
				t.Errorf("%.60s: ok=%v query %q, want %q", tc.target, g.ok, g.query, tc.want)
			}
			continue
		}
		if g.ok || g.rec.Code != tc.status || g.body() != `{"message":"`+tc.msg+`"}` {
			t.Errorf("%.60s: ok=%v %d %q", tc.target, g.ok, g.rec.Code, g.body())
		}
	}
}

func TestMCPCORS(t *testing.T) {
	f := newMCPFixture(t)
	exposed := "WWW-Authenticate, Mcp-Session-Id"

	// A preflight needs no token and carries no credentials mode.
	g := f.gate("OPTIONS", "/mcp", "", "Origin", "https://claude.ai", "Access-Control-Request-Method", "POST",
		"Access-Control-Request-Headers", "authorization,content-type,mcp-protocol-version")
	h := g.rec.Header()
	if g.ok || g.rec.Code != 204 || g.rec.Body.Len() != 0 {
		t.Fatalf("preflight: ok=%v %d %q", g.ok, g.rec.Code, g.body())
	}
	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "GET, POST, DELETE, OPTIONS",
		"Access-Control-Allow-Headers":  "authorization, content-type, mcp-protocol-version, mcp-session-id, last-event-id",
		"Access-Control-Expose-Headers": exposed,
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("Access-Control-Allow-Credentials") != "" || h.Get("WWW-Authenticate") != "" {
		t.Errorf("preflight headers: %v", h)
	}

	// The answers are readable by a page of another origin, the challenge included.
	for name, g := range map[string]gated{
		"no token":   f.gate("POST", "/mcp", `{}`, "Origin", "https://claude.ai"),
		"bad token":  f.gate("POST", "/mcp", `{}`, "Origin", "https://claude.ai", "Authorization", "Bearer x"),
		"good token": f.gate("POST", "/mcp", `{}`, "Origin", "https://claude.ai", "Authorization", "Bearer "+f.oauth),
		"method":     f.gate("PUT", "/mcp", `{}`, "Origin", "https://claude.ai", "Authorization", "Bearer "+f.oauth),
	} {
		h := g.rec.Header()
		if h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Expose-Headers") != exposed || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("%s: %v", name, h)
		}
	}

	// Other methods are the gate's to refuse, before anyone looks at the token.
	before := f.auth.lookupCount()
	for _, method := range []string{"PUT", "PATCH", "HEAD", "TRACE", "CONNECT", "PROPFIND"} {
		g := f.gate(method, "/mcp", "", mcpAuthz(f.oauth)...)
		if g.ok || g.rec.Code != 405 || g.rec.Header().Get("Allow") != "GET, POST, DELETE, OPTIONS" {
			t.Errorf("%s: ok=%v %d %v", method, g.ok, g.rec.Code, g.rec.Header())
		}
	}
	if f.auth.lookupCount() != before {
		t.Error("a method the endpoint does not serve cost a token lookup")
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if g := f.gate(method, "/mcp", "", mcpAuthz(f.oauth)...); !g.ok {
			t.Errorf("%s: %d", method, g.rec.Code)
		}
	}
}

func TestMCPDisabled(t *testing.T) {
	f := newMCPFixture(t)
	f.cfg.API.DisableOAuth = true
	for _, method := range []string{"POST", "GET", "DELETE", "OPTIONS", "PUT"} {
		g := f.gate(method, "/mcp", `{}`, mcpAuthz(f.oauth)...)
		if g.ok || g.rec.Code != 404 || g.body() != `{"message":"Not Found"}` || g.rec.Header().Get("Access-Control-Allow-Origin") != "" || g.rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: ok=%v %d %q %v", method, g.ok, g.rec.Code, g.body(), g.rec.Header())
		}
	}
	if f.auth.lookupCount() != 0 {
		t.Error("a disabled endpoint looked a token up")
	}
	f.cfg.API.DisableOAuth = false
	if g := f.gate("POST", "/mcp", `{}`, mcpAuthz(f.oauth)...); !g.ok {
		t.Errorf("enabled again: %d", g.rec.Code)
	}
}

func TestMCPBodyCap(t *testing.T) {
	f := newMCPFixture(t)
	// A declared length over the cap is refused before anything is forwarded, and gives the slot back.
	for i := 0; i < 2*mcpMaxInFlight; i++ {
		req := httptest.NewRequest("POST", "https://api.example.test/mcp", strings.NewReader("x"))
		req.ContentLength = mcpMaxBody + 1
		req.Header.Set("Authorization", "Bearer "+f.oauth)
		rec := httptest.NewRecorder()
		if _, ok := f.srv.MCPGate(rec, req); ok || rec.Code != 413 {
			t.Fatalf("declared %d bytes: ok=%v %d %q", req.ContentLength, ok, rec.Code, rec.Body)
		}
	}
	// One of an unknown length is cut off at the cap.
	req := httptest.NewRequest("POST", "https://api.example.test/mcp", strings.NewReader(strings.Repeat("x", mcpMaxBody+1)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+f.oauth)
	if _, ok := f.srv.MCPGate(httptest.NewRecorder(), req); !ok {
		t.Fatal("refused")
	}
	var tooLarge *http.MaxBytesError
	buf := make([]byte, 1<<20)
	var err error
	n := 0
	for err == nil {
		var m int
		m, err = req.Body.Read(buf)
		n += m
	}
	if !errors.As(err, &tooLarge) || n != mcpMaxBody {
		t.Errorf("read %d bytes, then %v; want the cap of %d and a MaxBytesError", n, err, mcpMaxBody)
	}
}

// T4: nothing secret reaches the logs.
func TestMCPGateLogsNoSecrets(t *testing.T) {
	f := newMCPFixture(t)
	var logs bytes.Buffer
	f.srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.srv.auth.ssoUser = func(context.Context, string) error { return errors.New("sso store down") }
	f.auth.mu.Lock()
	f.auth.err = errors.New("lookup failed")
	f.auth.mu.Unlock()
	for _, tok := range []string{f.oauth, f.pat, f.jwt, "garbage"} {
		f.gate("POST", "/mcp?access_token="+tok+"&code=sbc_leak&state=leakstate", `{"token":"`+tok+`"}`, mcpAuthz(tok)...)
	}
	if logs.Len() == 0 {
		t.Fatal("control: the failures were not logged")
	}
	for _, leak := range []string{f.oauth, f.pat, f.jwt, "garbage", "sbc_leak", "leakstate", "access_token"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the log has %q:\n%s", leak, logs.String())
		}
	}
}
