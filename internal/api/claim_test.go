package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// fakeGoTrue is the part of sb-gotrue@system's admin API the claim flow calls.
type fakeGoTrue struct {
	*httptest.Server
	mu      sync.Mutex
	users   map[string]map[string]any // by id
	created []map[string]any
	// failWith, when set, answers the next create with this status and error code.
	failWith int
	failCode string
	// failDelete, when set, answers the next delete with a 500.
	failDelete bool
	auth       []string
	// calls records the mail-related calls ("POST /invite?redirect_to=...").
	calls []string
	// mailFail makes /invite and /magiclink answer 500, as GoTrue does when SMTP is down.
	mailFail bool
}

func (g *fakeGoTrue) callList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// addUser puts an account in the fake, as GoTrue would have it.
func (g *fakeGoTrue) addUser(id, email string, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[id] = map[string]any{"id": id, "email": email, "created_at": created.UTC().Format(time.RFC3339), "app_metadata": map[string]any{AdminClaim: true}}
}

func newFakeGoTrue(t testing.TB) *fakeGoTrue {
	g := &fakeGoTrue{users: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/users", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if g.failWith != 0 {
			st := g.failWith
			g.failWith = 0
			w.WriteHeader(st)
			_ = json.NewEncoder(w).Encode(map[string]any{"msg": "refused: " + g.failCode, "error_code": g.failCode})
			return
		}
		for _, u := range g.users {
			if u["email"] == in["email"] {
				w.WriteHeader(422)
				_ = json.NewEncoder(w).Encode(map[string]any{"msg": "A user with this email address has already been registered", "error_code": "email_exists"})
				return
			}
		}
		id := "00000000-0000-4000-8000-00000000000" + string(rune('0'+len(g.users)))
		in["id"] = id
		in["created_at"] = time.Now().UTC().Format(time.RFC3339)
		g.users[id] = in
		g.created = append(g.created, in)
		_ = json.NewEncoder(w).Encode(in)
	})
	mux.HandleFunc("GET /admin/users", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var us []map[string]any
		for _, u := range g.users {
			us = append(us, u)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": us})
	})
	mux.HandleFunc("DELETE /admin/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.failDelete {
			g.failDelete = false
			w.WriteHeader(500)
			return
		}
		delete(g.users, r.PathValue("id"))
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("GET /admin/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		u, ok := g.users[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("PUT /admin/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, "PUT "+r.URL.Path)
		u, ok := g.users[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		for k, v := range in {
			u[k] = v
		}
		_ = json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("POST /invite", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, "POST /invite?"+r.URL.RawQuery)
		if g.mailFail {
			w.WriteHeader(500)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := fmt.Sprintf("00000000-0000-4000-8000-1%011d", len(g.users))
		u := map[string]any{"id": id, "email": in["email"], "created_at": time.Now().UTC().Format(time.RFC3339)}
		g.users[id] = u
		_ = json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("POST /magiclink", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, "POST /magiclink?"+r.URL.RawQuery)
		if g.mailFail {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

type claimFixture struct {
	t    *testing.T
	reg  *registry.Memory
	gt   *fakeGoTrue
	acc  *Accounts
	srv  *Server
	now  time.Time
	nowM sync.Mutex
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	reg := registry.NewMemory()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Domain = "example.test"
	cfg.API.PGMetaCryptoKey = "k"
	mgr := newFakeManager(reg, sec)
	mgr.addProject(t, config.SystemRef, "system", 0, registry.StatusActiveHealthy)
	gt := newFakeGoTrue(t)
	f := &claimFixture{t: t, reg: reg, gt: gt, now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	srv, err := NewServer(Deps{
		Registry: reg, Secrets: sec, Manager: mgr, Config: cfg, PGMetaURL: "http://127.0.0.1:1",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Upstream: func(p *registry.Project, svc string) string {
			if p.Ref == config.SystemRef && svc == upGoTrue {
				return gt.URL
			}
			return ""
		},
		Now: func() time.Time { f.nowM.Lock(); defer f.nowM.Unlock(); return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.acc = srv, srv.accounts
	return f
}

func (f *claimFixture) advance(d time.Duration) { f.nowM.Lock(); f.now = f.now.Add(d); f.nowM.Unlock() }

func (f *claimFixture) post(body any) *httptest.ResponseRecorder {
	return f.postFrom("", "", body)
}

// postFrom posts from a remote address (empty keeps httptest's 192.0.2.1) with an X-Forwarded-For.
func (f *claimFixture) postFrom(remote, xff string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/claim", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if remote != "" {
		req.RemoteAddr = remote
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

const goodPassword = "correct horse battery"

func TestClaimPageIsSelfContained(t *testing.T) {
	f := newClaimFixture(t)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest("GET", "/claim", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /claim: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, banned := range []string{"http://", "https://", "src=\"", "<link"} {
		if strings.Contains(body, banned) {
			t.Errorf("claim page references %q: it must not load anything", banned)
		}
	}
	// The CSP names the hash of the page's one inline script.
	h := sha256.Sum256([]byte(claimScript))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"; !strings.Contains(rec.Header().Get("Content-Security-Policy"), want) {
		t.Errorf("CSP %q lacks %s", rec.Header().Get("Content-Security-Policy"), want)
	}
	if !strings.Contains(body, "<script>"+claimScript+"</script>") {
		t.Error("the inline script in the page differs from the hashed one")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("claim page must not be cached")
	}
}

func TestClaimFlow(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, exp, err := f.acc.IssueClaimToken(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "sbc_") || len(token) != 4+48 {
		t.Fatalf("token shape %q", token)
	}
	if !exp.Equal(f.now.Add(DefaultClaimTTL)) {
		t.Fatalf("expiry %v", exp)
	}
	if ok, _ := f.acc.Claimed(ctx); ok {
		t.Fatal("claimed before the claim")
	}

	rec := f.post(RedeemRequest{Token: token, Email: "Admin@Example.Test", Password: goodPassword, Organization: "Acme Corp"})
	if rec.Code != 201 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	var res RedeemResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Email != "admin@example.test" || res.Organization != "acme-corp" || res.DashboardURL != "https://studio.example.test" || res.UserID == "" {
		t.Fatalf("result %+v", res)
	}
	if len(f.gt.created) != 1 {
		t.Fatalf("gotrue got %d users", len(f.gt.created))
	}
	u := f.gt.created[0]
	am, _ := u["app_metadata"].(map[string]any)
	if u["email"] != "admin@example.test" || u["email_confirm"] != true || am[AdminClaim] != true || u["password"] != goodPassword {
		t.Fatalf("user sent to gotrue: %v", u)
	}
	if len(f.gt.auth) != 1 || !strings.HasPrefix(f.gt.auth[0], "Bearer ey") {
		t.Fatalf("admin call must carry the system service_role JWT, got %q", f.gt.auth)
	}
	if o, err := f.reg.GetOrganization(ctx, "acme-corp"); err != nil || o.Name != "Acme Corp" {
		t.Fatalf("organization: %v %v", o, err)
	}
	if ok, _ := f.acc.Claimed(ctx); !ok {
		t.Fatal("not claimed after the claim")
	}

	// Single use.
	if rec := f.post(RedeemRequest{Token: token, Email: "second@example.test", Password: goodPassword}); rec.Code != 403 {
		t.Fatalf("second use: %d %s", rec.Code, rec.Body)
	}
	if len(f.gt.created) != 1 {
		t.Fatal("a second user was created with a spent token")
	}
	// Once claimed, no new claim token without force.
	if _, _, err := f.acc.IssueClaimToken(ctx, 0, false); !errors.Is(err, ErrClaimed) {
		t.Fatalf("IssueClaimToken after claim: %v", err)
	}
	if _, _, err := f.acc.IssueClaimToken(ctx, time.Hour, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
}

func TestClaimExistingOrganizationIsKept(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	_, _ = f.reg.CreateOrganization(ctx, "first", "First")
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword, Organization: "Other"})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body)
	}
	orgs, _ := f.reg.ListOrganizations(ctx)
	if len(orgs) != 1 || orgs[0].Slug != "first" {
		t.Fatalf("organizations %v", orgs)
	}
}

func TestClaimRejects(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, time.Hour, false)
	for name, tc := range map[string]struct {
		in   RedeemRequest
		want int
	}{
		"unknown token":  {RedeemRequest{Token: "sbc_" + strings.Repeat("0", 48), Email: "a@example.test", Password: goodPassword}, 403},
		"no prefix":      {RedeemRequest{Token: "hello", Email: "a@example.test", Password: goodPassword}, 403},
		"empty":          {RedeemRequest{}, 403},
		"short password": {RedeemRequest{Token: token, Email: "a@example.test", Password: "short"}, 400},
		"long password":  {RedeemRequest{Token: token, Email: "a@example.test", Password: strings.Repeat("x", 73)}, 400},
		"no email":       {RedeemRequest{Token: token, Password: goodPassword}, 400},
		"bad email":      {RedeemRequest{Token: token, Email: "not an email", Password: goodPassword}, 400},
		"display name":   {RedeemRequest{Token: token, Email: "Al <a@example.test>", Password: goodPassword}, 400},
	} {
		if rec := f.post(tc.in); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d: %s", name, rec.Code, tc.want, rec.Body)
		}
	}
	// None of those spent the real token.
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 201 {
		t.Fatalf("token was spent by a rejected request: %d %s", rec.Code, rec.Body)
	}
}

func TestClaimExpires(t *testing.T) {
	f := newClaimFixture(t)
	token, _, _ := f.acc.IssueClaimToken(context.Background(), time.Hour, false)
	f.advance(time.Hour + time.Second)
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 403 {
		t.Fatalf("expired token: %d %s", rec.Code, rec.Body)
	}
}

func TestClaimReleasedWhenUserCannotBeCreated(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)

	f.gt.failWith, f.gt.failCode = 422, "weak_password"
	rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "weak_password") {
		t.Fatalf("weak password: %d %s", rec.Code, rec.Body)
	}
	f.gt.failWith, f.gt.failCode = 500, "unexpected_failure"
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 500 {
		t.Fatalf("gotrue 500: %d %s", rec.Code, rec.Body)
	}
	if ok, _ := f.acc.Claimed(ctx); ok {
		t.Fatal("a failed claim counts as claimed")
	}
	// The token still works.
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 201 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
}

func TestInvite(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, err := f.acc.IssueInvite(ctx, "Dev@Example.Test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "sbi_") {
		t.Fatalf("invite token %q", token)
	}
	if _, _, err := f.acc.IssueInvite(ctx, "not an email", 0); err == nil {
		t.Fatal("invalid address accepted")
	}
	if rec := f.post(RedeemRequest{Token: token, Email: "someone-else@example.test", Password: goodPassword}); rec.Code != 400 {
		t.Fatalf("invite for another address: %d %s", rec.Code, rec.Body)
	}
	rec := f.post(RedeemRequest{Token: token, Password: goodPassword}) // the address comes from the invite
	if rec.Code != 201 {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	var res RedeemResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Email != "dev@example.test" || res.Organization != "" {
		t.Fatalf("result %+v", res)
	}
	// An invite does not make the node "claimed", and re-issuing revokes the older one.
	if ok, _ := f.acc.Claimed(ctx); ok {
		t.Fatal("an invite counted as the claim")
	}
	t1, _, _ := f.acc.IssueInvite(ctx, "x@example.test", 0)
	t2, _, _ := f.acc.IssueInvite(ctx, "x@example.test", 0)
	if rec := f.post(RedeemRequest{Token: t1, Password: goodPassword}); rec.Code != 403 {
		t.Fatalf("revoked invite: %d", rec.Code)
	}
	if rec := f.post(RedeemRequest{Token: t2, Password: goodPassword}); rec.Code != 201 {
		t.Fatalf("latest invite: %d %s", rec.Code, rec.Body)
	}
	// A claim token never works for an invite's address field being empty.
	ct, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	if rec := f.post(RedeemRequest{Token: ct, Password: goodPassword}); rec.Code != 400 {
		t.Fatalf("claim without email: %d", rec.Code)
	}
}

func TestClaimConcurrentRedeemCreatesOneUser(t *testing.T) {
	f := newClaimFixture(t)
	token, _, _ := f.acc.IssueClaimToken(context.Background(), 0, false)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}).Code
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c == 201 {
			ok++
		}
	}
	if ok != 1 || len(f.gt.created) != 1 {
		t.Fatalf("%d redemptions succeeded and %d users were created, want 1 and 1", ok, len(f.gt.created))
	}
}

func TestClaimFailuresAreRateLimited(t *testing.T) {
	f := newClaimFixture(t)
	bad := RedeemRequest{Token: "sbc_" + strings.Repeat("1", 48), Email: "a@example.test", Password: goodPassword}
	for i := 0; i < claimFailLimit; i++ {
		if rec := f.post(bad); rec.Code != 403 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := f.post(bad)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d failures: %d", claimFailLimit, rec.Code)
	}
	// Even the right token waits out the window.
	token, _, _ := f.acc.IssueClaimToken(context.Background(), 0, false)
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 429 {
		t.Fatalf("right token during lockout: %d", rec.Code)
	}
	f.advance(2 * time.Minute)
	if rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 201 {
		t.Fatalf("after the window: %d %s", rec.Code, rec.Body)
	}
}

func TestListAndRemoveUsers(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	var res RedeemResult
	rec := f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword})
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	us, err := f.acc.ListUsers(ctx)
	if err != nil || len(us) != 1 || us[0].Email != "a@example.test" || !us[0].Admin {
		t.Fatalf("users %+v %v", us, err)
	}
	for i := 0; i < 2; i++ {
		if err := f.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: res.UserID, Name: "t", Hash: secrets.HashToken(secrets.NewPAT()), Prefix: "sbp_x"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := f.acc.RemoveUser(ctx, "A@example.test", true)
	if err != nil || n != 2 {
		t.Fatalf("RemoveUser: %d %v", n, err)
	}
	if us, _ := f.acc.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("user still listed: %+v", us)
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, res.UserID); len(ts) != 0 {
		t.Fatalf("tokens left: %d", len(ts))
	}
	if _, err := f.acc.RemoveUser(ctx, "nobody@example.test", true); err == nil {
		t.Fatal("removing an unknown user succeeded")
	}
}

// testClaimStore is the conformance check of every ClaimStore.
func testClaimStore(t *testing.T, s ClaimStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	h1, h2, h3 := secrets.HashToken("one"), secrets.HashToken("two"), secrets.HashToken("three")

	if ok, err := s.Claimed(ctx); err != nil || ok {
		t.Fatalf("Claimed on empty: %v %v", ok, err)
	}
	t1, err := s.CreateClaimToken(ctx, KindClaim, h1, "", now.Add(time.Hour))
	if err != nil || t1.Kind != KindClaim || t1.Email != "" {
		t.Fatalf("create: %+v %v", t1, err)
	}
	// A second claim token revokes the first.
	if _, err := s.CreateClaimToken(ctx, KindClaim, h2, "", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupClaimToken(ctx, h1, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token still found: %v", err)
	}
	// Expired tokens are not live.
	if _, err := s.LookupClaimToken(ctx, h2, now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token found: %v", err)
	}
	got, err := s.ConsumeClaimToken(ctx, h2, now, "a@example.test")
	if err != nil || got.UsedBy != "a@example.test" {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if _, err := s.ConsumeClaimToken(ctx, h2, now, "b@example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: %v", err)
	}
	if ok, _ := s.Claimed(ctx); !ok {
		t.Fatal("not claimed after consume")
	}
	if err := s.ReleaseClaimToken(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Claimed(ctx); ok {
		t.Fatal("claimed after release")
	}
	if _, err := s.LookupClaimToken(ctx, h2, now); err != nil {
		t.Fatalf("released token not live: %v", err)
	}
	// Invites are per address.
	if _, err := s.CreateClaimToken(ctx, KindInvite, h3, "x@example.test", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateClaimToken(ctx, KindInvite, secrets.HashToken("four"), "y@example.test", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	inv, err := s.LookupClaimToken(ctx, h3, now)
	if err != nil || inv.Kind != KindInvite || inv.Email != "x@example.test" {
		t.Fatalf("invite for x was revoked by the one for y: %+v %v", inv, err)
	}
}

func TestMemoryClaimStore(t *testing.T) { testClaimStore(t, NewMemoryClaimStore()) }

// TestPGClaimStore runs the same checks against a real database (CI provides one).
func TestPGClaimStore(t *testing.T) {
	dsn := os.Getenv("SBCTL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SBCTL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := registry.Migrate(ctx, r.Pool()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pool().Exec(ctx, `truncate sbctl.claim_tokens`); err != nil {
		t.Fatal(err)
	}
	testClaimStore(t, NewPGClaimStore(r.Pool()))
}

func TestClaimLimiterIsPerClient(t *testing.T) {
	f := newClaimFixture(t)
	junk := RedeemRequest{Token: "not-a-token", Password: goodPassword}
	// One anonymous client sends junk until it is limited.
	for i := 0; i < claimFailLimit; i++ {
		if rec := f.postFrom("198.51.100.9:4000", "", junk); rec.Code != 403 {
			t.Fatalf("junk %d: %d", i, rec.Code)
		}
	}
	if rec := f.postFrom("198.51.100.9:4001", "", junk); rec.Code != 429 {
		t.Fatalf("the noisy client was not limited: %d", rec.Code)
	}
	// The administrator, from another address, is not.
	token, _, _ := f.acc.IssueClaimToken(context.Background(), 0, false)
	if rec := f.postFrom("203.0.113.5:5000", "", RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}); rec.Code != 201 {
		t.Fatalf("a different client was locked out: %d %s", rec.Code, rec.Body)
	}
}

func TestClaimClientKey(t *testing.T) {
	for _, tc := range []struct{ remote, xff, want string }{
		{"203.0.113.5:1", "", "203.0.113.5"},
		{"203.0.113.5:1", "10.9.9.9", "203.0.113.5"}, // not from our proxy: the header is the caller's
		{"127.0.0.1:1", "198.51.100.7", "198.51.100.7"},
		{"127.0.0.1:1", "1.2.3.4, 198.51.100.7", "198.51.100.7"}, // the last entry is the proxy's
		{"[::1]:1", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::"},
		{"127.0.0.1:1", "", "127.0.0.1"},
	} {
		r := httptest.NewRequest("POST", "/claim", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := claimClient(r); got != tc.want {
			t.Errorf("%s xff %q: %q, want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}

func TestClaimLimiterMemoryIsBoundedAndFailsOpenWhenFull(t *testing.T) {
	var l claimLimiter
	now := time.Now()
	for i := 0; i < claimMaxClients+500; i++ {
		l.fail(fmt.Sprintf("client-%d", i), now)
	}
	if len(l.clients) > claimMaxClients {
		t.Fatalf("%d tracked clients", len(l.clients))
	}
	// A client the full table cannot track is never blocked, however often it fails: a flood of
	// junk clients must not lock out the first administrator.
	for i := 0; i < 3*claimFailLimit; i++ {
		l.fail("fresh-admin", now)
	}
	if l.blocked("fresh-admin", now) {
		t.Fatal("an unknown client was blocked while the table was full")
	}
	if len(l.clients) > claimMaxClients {
		t.Fatalf("%d tracked clients after the untracked failures", len(l.clients))
	}
	// Tracked clients are still limited, and once the windows expire the table takes new clients.
	for i := 0; i < claimFailLimit; i++ {
		l.fail("client-0", now)
	}
	if !l.blocked("client-0", now) {
		t.Fatal("a tracked client was not blocked")
	}
	later := now.Add(2 * claimFailWindow)
	for i := 0; i < claimFailLimit; i++ {
		l.fail("fresh-admin", later)
	}
	if !l.blocked("fresh-admin", later) {
		t.Fatal("a client is not tracked after the old windows expired")
	}
}

func TestRemoveUserDeletesTokensBeforeTheAccount(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	var res RedeemResult
	_ = json.Unmarshal(f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}).Body.Bytes(), &res)
	if err := f.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: res.UserID, Name: "t", Hash: secrets.HashToken(secrets.NewPAT()), Prefix: "sbp_x"}); err != nil {
		t.Fatal(err)
	}
	f.gt.failDelete = true
	if _, err := f.acc.RemoveUser(ctx, "a@example.test", true); err == nil {
		t.Fatal("a failed GoTrue delete was reported as success")
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, res.UserID); len(ts) != 0 {
		t.Fatalf("tokens of the account survived the attempt: %d", len(ts))
	}
	if us, _ := f.acc.ListUsers(ctx); len(us) != 1 {
		t.Fatalf("the account must still be findable to retry: %+v", us)
	}
	if _, err := f.acc.RemoveUser(ctx, "a@example.test", true); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if us, _ := f.acc.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("user left after the retry: %+v", us)
	}
}

// The claimed first user owns the organization; an organization keeps its Owner when the
// account is removed; removing an account removes its memberships.
func TestClaimedUserIsOwnerAndTheLastOwnerStays(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	rec := f.post(RedeemRequest{Token: token, Email: "first@example.test", Password: goodPassword, Organization: "Acme"})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body)
	}
	res := body[RedeemResult](t, rec)
	org, _ := f.reg.GetOrganization(ctx, res.Organization)
	a, err := f.srv.members.Access(ctx, res.UserID)
	if err != nil || a.OrgRole(org.ID) != members.RoleOwner {
		t.Fatalf("the first user is Owner of %s: %+v %v", res.Organization, a, err)
	}
	if _, err := f.srv.store.GetUser(ctx, res.UserID); err != nil {
		t.Fatalf("the user is recorded at creation: %v", err)
	}
	// Invited with a role, the account joins on creation.
	inv, err := f.acc.InviteToOrganization(ctx, nil, members.OrgRef{ID: org.ID, Slug: org.Slug}, members.InviteInput{Email: "dev@example.test", RoleID: members.RoleDeveloper})
	if err != nil || inv.ClaimURL == "" {
		t.Fatalf("invite: %+v %v", inv, err)
	}
	u, _ := url.Parse(inv.ClaimURL)
	frag, _ := url.ParseQuery(u.Fragment)
	rec = f.post(RedeemRequest{Token: frag.Get("token"), Email: "dev@example.test", Password: goodPassword})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body)
	}
	dev := body[RedeemResult](t, rec)
	if a, _ := f.srv.members.Access(ctx, dev.UserID); a.OrgRole(org.ID) != members.RoleDeveloper {
		t.Fatalf("the invited user: %+v", a.Memberships)
	}
	// An invite token for an address nobody invited to an organization creates an account without access.
	tok, _, _ := f.acc.IssueInvite(ctx, "bare@example.test", 0)
	rec = f.post(RedeemRequest{Token: tok, Email: "bare@example.test", Password: goodPassword})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body)
	}
	bare := body[RedeemResult](t, rec)
	if a, _ := f.srv.members.Access(ctx, bare.UserID); len(a.Memberships) != 0 {
		t.Fatalf("a bare account has no memberships: %+v", a.Memberships)
	}
	// users remove: the only Owner stays unless forced; memberships go with the account.
	if _, err := f.acc.RemoveUser(ctx, "first@example.test", false); !errors.Is(err, members.ErrLastOwner) {
		t.Fatalf("removing the only owner: %v", err)
	}
	if _, ok := f.gt.users[res.UserID]; !ok {
		t.Fatal("the account was deleted although the removal was refused")
	}
	if _, err := f.acc.RemoveUser(ctx, "dev@example.test", false); err != nil {
		t.Fatal(err)
	}
	if ms, _ := f.srv.members.Store.MembershipsOf(ctx, dev.UserID); len(ms) != 0 {
		t.Fatalf("memberships after removing the account: %+v", ms)
	}
	if _, err := f.acc.RemoveUser(ctx, "first@example.test", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.srv.members.Store.CountOwners(ctx, org.ID); n != 0 {
		t.Fatalf("--force removes the last owner: %d", n)
	}
}
