package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// ---- views into the memory store for other tests of this package

func (m *MemoryStore) dumpForTest(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, tbl := range []struct {
		name string
		rows any
	}{{"apps", m.apps}, {"secrets", m.secrets}, {"authorizations", m.auths}, {"grants", m.grants}, {"tokens", m.tokens}} {
		j, err := json.Marshal(tbl.rows)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(tbl.name + ": " + string(j) + "\n")
	}
	return b.String()
}

func (m *MemoryStore) authsForTest() []Authorization {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Authorization
	for _, a := range m.auths {
		out = append(out, memAuth(a))
	}
	return out
}

func (m *MemoryStore) secretsForTest() []AppSecret {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AppSecret
	for _, s := range m.secrets {
		out = append(out, memSecret(s))
	}
	return out
}

// ---- rows

var memT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func memApp1(id string, kind string, org int64) App {
	a := App{ID: id, RegistrationType: kind, OrgID: org, Name: "App " + id[:4], RedirectURIs: []string{"https://x.example.test/cb"},
		Scopes: []string{ScopeProjectsRead, ScopeDatabaseRead}, TokenEndpointAuthMethod: AuthMethodBasic, CreatedAt: memT0, UpdatedAt: memT0}
	return a
}

type memEnv struct {
	t     *testing.T
	m     *MemoryStore
	ctx   context.Context
	app   App
	hashN byte
}

func newMemEnv(t *testing.T) *memEnv {
	t.Helper()
	e := &memEnv{t: t, m: NewMemoryStore(), ctx: context.Background(), app: memApp1(secrets.NewUUID(), RegistrationDynamic, 0)}
	e.m.SetOrgSlugs(func(id int64) string { return map[int64]string{1: "acme", 2: "other"}[id] })
	if err := e.m.CreateApp(e.ctx, e.app, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

// hash returns a distinct 32-byte value on every call.
func (e *memEnv) hash() []byte {
	e.hashN++
	h := make([]byte, 32)
	h[0] = e.hashN
	return h
}

// approved stores a pending authorization and approves it with a code hash; it returns the id and the hash.
func (e *memEnv) approved(user string, org int64) (string, []byte) {
	e.t.Helper()
	id := secrets.NewUUID()
	a := Authorization{ID: id, AppID: e.app.ID, RedirectURI: "https://x.example.test/cb", Scopes: []string{ScopeProjectsRead},
		CreatedAt: memT0, ExpiresAt: memT0.Add(time.Hour), Status: StatusPending}
	if err := e.m.CreateAuthorization(e.ctx, a); err != nil {
		e.t.Fatal(err)
	}
	h := e.hash()
	if _, err := e.m.DecideAuthorization(e.ctx, id, Decision{Status: StatusApproved, DecidedBy: user, At: memT0, OrgID: org, CodeHash: h, CodeExpiresAt: memT0.Add(time.Minute)}); err != nil {
		e.t.Fatal(err)
	}
	return id, h
}

func (e *memEnv) newGrant(user string, org int64, at time.Time) NewGrant {
	return NewGrant{
		Grant:   Grant{AppID: e.app.ID, UserID: user, OrgID: org, Scopes: []string{ScopeProjectsRead, ScopeDatabaseRead}, Resource: "r", CreatedAt: at},
		Access:  Token{Hash: e.hash(), Prefix: "sbp_oaut", CreatedAt: at, ExpiresAt: at.Add(time.Hour)},
		Refresh: Token{Hash: e.hash(), Prefix: "sbr_abcd", CreatedAt: at, ExpiresAt: at.Add(90 * 24 * time.Hour)},
		MaxLive: 3, At: at,
	}
}

// issue runs a whole redemption and returns the result.
func (e *memEnv) issue(user string, org int64, at time.Time) (*CompleteResult, NewGrant) {
	e.t.Helper()
	_, h := e.approved(user, org)
	ng := e.newGrant(user, org, at)
	var res *CompleteResult
	err := e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) (err error) {
		res, err = tx.Complete(ctx, ng)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return res, ng
}

// ---- the tests

func TestMemoryStoreZeroValueWorks(t *testing.T) {
	var m MemoryStore
	ctx := context.Background()
	if n, err := m.CountDynamicApps(ctx); err != nil || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if err := m.CreateApp(ctx, memApp1(secrets.NewUUID(), RegistrationDynamic, 0), nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.CountDynamicApps(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
}

func TestMemoryStoreMalformedIDsAreNotFound(t *testing.T) {
	e := newMemEnv(t)
	for _, id := range []string{"", "x", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz", e.app.ID + "0"} {
		if _, err := e.m.GetApp(e.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetApp(%q): %v", id, err)
		}
		if err := e.m.UpdateApp(e.ctx, App{ID: id}); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateApp(%q): %v", id, err)
		}
		if _, err := e.m.DeleteApp(e.ctx, id, memT0); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteApp(%q): %v", id, err)
		}
		if _, err := e.m.GetAuthorization(e.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetAuthorization(%q): %v", id, err)
		}
		if _, err := e.m.DecideAuthorization(e.ctx, id, Decision{Status: StatusDeclined, At: memT0}); !errors.Is(err, ErrNotFound) {
			t.Errorf("DecideAuthorization(%q): %v", id, err)
		}
		if err := e.m.DeleteSecret(e.ctx, id, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteSecret(%q): %v", id, err)
		}
		if err := e.m.WithCode(e.ctx, id, []byte{1}, func(context.Context, CodeTx) error { t.Error("fn ran"); return nil }); !errors.Is(err, ErrNotFound) {
			t.Errorf("WithCode(%q): %v", id, err)
		}
		if ss, err := e.m.ListSecrets(e.ctx, id); err != nil || len(ss) != 0 {
			t.Errorf("ListSecrets(%q): %v, %v", id, ss, err)
		}
		if n, err := e.m.CountPending(e.ctx, id, memT0); err != nil || n != 0 {
			t.Errorf("CountPending(%q): %d, %v", id, n, err)
		}
	}
	// An upper-case id finds the same row.
	if a, err := e.m.GetApp(e.ctx, strings.ToUpper(e.app.ID)); err != nil || a.ID != e.app.ID {
		t.Errorf("upper-case id: %v, %v", a, err)
	}
}

func TestMemoryStoreCopies(t *testing.T) {
	e := newMemEnv(t)
	a, _ := e.m.GetApp(e.ctx, e.app.ID)
	a.Scopes[0], a.RedirectURIs[0], a.Name = "mutated", "mutated", "mutated"
	again, _ := e.m.GetApp(e.ctx, e.app.ID)
	if again.Scopes[0] == "mutated" || again.RedirectURIs[0] == "mutated" || again.Name == "mutated" {
		t.Error("GetApp hands out the stored row")
	}
	// What the caller passes in is copied too.
	in := memApp1(secrets.NewUUID(), RegistrationDynamic, 0)
	if err := e.m.CreateApp(e.ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	in.Scopes[0] = "mutated"
	if got, _ := e.m.GetApp(e.ctx, in.ID); got.Scopes[0] == "mutated" {
		t.Error("CreateApp keeps the caller's slice")
	}
}

func TestMemoryStoreConflicts(t *testing.T) {
	e := newMemEnv(t)
	if err := e.m.CreateApp(e.ctx, e.app, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("app id twice: %v", err)
	}
	sec := AppSecret{ID: secrets.NewUUID(), AppID: e.app.ID, Alias: "sba_1234********", Hash: e.hash(), CreatedAt: memT0}
	if err := e.m.CreateSecret(e.ctx, sec); err != nil {
		t.Fatal(err)
	}
	dup := sec
	dup.ID = secrets.NewUUID()
	if err := e.m.CreateSecret(e.ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("secret hash twice: %v", err)
	}
	dup = AppSecret{ID: sec.ID, AppID: e.app.ID, Hash: e.hash()}
	if err := e.m.CreateSecret(e.ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("secret id twice: %v", err)
	}
	if err := e.m.CreateSecret(e.ctx, AppSecret{ID: secrets.NewUUID(), AppID: secrets.NewUUID(), Hash: e.hash()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("secret of an unknown app: %v", err)
	}
	// CreateApp with a first secret is atomic: a conflicting hash leaves no app behind.
	second := memApp1(secrets.NewUUID(), RegistrationDynamic, 0)
	if err := e.m.CreateApp(e.ctx, second, &AppSecret{ID: secrets.NewUUID(), Hash: sec.Hash}); !errors.Is(err, ErrConflict) {
		t.Errorf("app with a conflicting secret: %v", err)
	}
	if _, err := e.m.GetApp(e.ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the app was created though its secret conflicted: %v", err)
	}
	// A duplicate authorization id and a duplicate code hash.
	id, h := e.approved("u", 1)
	if err := e.m.CreateAuthorization(e.ctx, Authorization{ID: id, AppID: e.app.ID, ExpiresAt: memT0}); !errors.Is(err, ErrConflict) {
		t.Errorf("authorization id twice: %v", err)
	}
	if err := e.m.CreateAuthorization(e.ctx, Authorization{ID: secrets.NewUUID(), AppID: e.app.ID, ExpiresAt: memT0, CodeHash: h}); !errors.Is(err, ErrConflict) {
		t.Errorf("code hash twice at creation: %v", err)
	}
	pending := secrets.NewUUID()
	if err := e.m.CreateAuthorization(e.ctx, Authorization{ID: pending, AppID: e.app.ID, ExpiresAt: memT0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.DecideAuthorization(e.ctx, pending, Decision{Status: StatusApproved, At: memT0, OrgID: 1, CodeHash: h, CodeExpiresAt: memT0})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("code hash twice at approval: %v", err)
	}
	if a, _ := e.m.GetAuthorization(e.ctx, pending); a.Status != StatusPending {
		t.Errorf("a refused approval changed the row: %+v", a)
	}
	if err := e.m.CreateAuthorization(e.ctx, Authorization{ID: secrets.NewUUID(), AppID: secrets.NewUUID(), ExpiresAt: memT0}); !errors.Is(err, ErrNotFound) {
		t.Errorf("authorization of an unknown app: %v", err)
	}
}

func TestMemoryStoreDecide(t *testing.T) {
	e := newMemEnv(t)
	mk := func(expires time.Time) string {
		id := secrets.NewUUID()
		if err := e.m.CreateAuthorization(e.ctx, Authorization{ID: id, AppID: e.app.ID, ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	approve := func(id string, at time.Time) (*Authorization, error) {
		return e.m.DecideAuthorization(e.ctx, id, Decision{Status: StatusApproved, DecidedBy: "u", At: at, OrgID: 1, CodeHash: e.hash(), CodeExpiresAt: at.Add(time.Minute)})
	}
	id := mk(memT0.Add(time.Hour))
	got, err := approve(id, memT0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusApproved || got.DecidedBy != "u" || got.DecidedAt == nil || !got.DecidedAt.Equal(memT0) || got.OrgID != 1 || got.OrgSlug != "acme" ||
		got.CodeExpiresAt == nil || !got.CodeExpiresAt.Equal(memT0.Add(time.Minute)) || len(got.CodeHash) != 32 {
		t.Errorf("approved row: %+v", got)
	}
	// The state decides what a second call says; "decided" wins over "expired".
	if _, err := approve(id, memT0); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("second approval: %v", err)
	}
	if _, err := approve(id, memT0.Add(2*time.Hour)); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("approval of a decided and expired row: %v", err)
	}
	if _, err := approve(secrets.NewUUID(), memT0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// Expiry is expires_at <= at.
	id = mk(memT0.Add(time.Hour))
	if _, err := approve(id, memT0.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("at the expiry instant: %v", err)
	}
	if _, err := approve(id, memT0.Add(time.Hour-time.Nanosecond)); err != nil {
		t.Errorf("a nanosecond before: %v", err)
	}
	// Declining stores no code.
	id = mk(memT0.Add(time.Hour))
	got, err = e.m.DecideAuthorization(e.ctx, id, Decision{Status: StatusDeclined, DecidedBy: "u", At: memT0})
	if err != nil || got.Status != StatusDeclined || got.CodeHash != nil || got.OrgID != 0 || got.OrgSlug != "" {
		t.Errorf("declined: %+v, %v", got, err)
	}
	// Only approve and decline are decisions, and an approval needs a code.
	id = mk(memT0.Add(time.Hour))
	for _, d := range []Decision{{Status: StatusExchanged, At: memT0}, {Status: StatusPending, At: memT0}, {Status: StatusApproved, At: memT0, OrgID: 1}} {
		if _, err := e.m.DecideAuthorization(e.ctx, id, d); err == nil {
			t.Errorf("decision %+v accepted", d)
		}
	}
	// CountPending: pending and unexpired.
	if n, _ := e.m.CountPending(e.ctx, e.app.ID, memT0); n != 1 {
		t.Errorf("pending = %d", n)
	}
	if n, _ := e.m.CountPending(e.ctx, "", memT0.Add(2*time.Hour)); n != 0 {
		t.Errorf("pending after everything expired = %d", n)
	}
	// Concurrent approvals: one wins.
	race := mk(memT0.Add(time.Hour))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		h := e.hash()
		go func() {
			defer wg.Done()
			_, err := e.m.DecideAuthorization(e.ctx, race, Decision{Status: StatusApproved, At: memT0, OrgID: 1, CodeHash: h, CodeExpiresAt: memT0})
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrAlreadyDecided) {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d winners", wins.Load())
	}
}

func TestMemoryStoreWithCode(t *testing.T) {
	e := newMemEnv(t)
	id, h := e.approved("user-1", 1)

	// A code of another app is not found and fn does not run.
	other := memApp1(secrets.NewUUID(), RegistrationDynamic, 0)
	if err := e.m.CreateApp(e.ctx, other, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.WithCode(e.ctx, other.ID, h, func(context.Context, CodeTx) error { t.Error("fn ran for another app"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("another app: %v", err)
	}
	if err := e.m.WithCode(e.ctx, e.app.ID, e.hash(), func(context.Context, CodeTx) error { t.Error("fn ran"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown code: %v", err)
	}

	// fn sees the row with the organization slug; the handle's copy is its own.
	err := e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
		a := tx.Authorization()
		if a.ID != id || a.Status != StatusApproved || a.OrgSlug != "acme" || a.DecidedBy != "user-1" {
			t.Errorf("row: %+v", a)
		}
		a.Status = "mutated"
		if tx.Authorization().Status != StatusApproved {
			t.Error("Authorization() hands out its own state")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// An error from fn is returned unchanged and undoes every write.
	boom := errors.New("boom")
	ng := e.newGrant("user-1", 1, memT0)
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
		if _, err := tx.Complete(ctx, ng); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Errorf("error = %v", err)
	}
	if a, _ := e.m.GetAuthorization(e.ctx, id); a.Status != StatusApproved || a.GrantID != 0 || a.CodeUsedAt != nil {
		t.Errorf("a failed redemption changed the authorization: %+v", a)
	}
	if gs, _ := e.m.ListGrants(e.ctx, GrantFilter{}); len(gs) != 0 {
		t.Errorf("a failed redemption left grants: %+v", gs)
	}
	if _, err := e.m.GetToken(e.ctx, ng.Access.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed redemption left a token: %v", err)
	}
	if app, _ := e.m.GetApp(e.ctx, e.app.ID); app.LastAuthorizedAt != nil {
		t.Errorf("last_authorized_at = %v", app.LastAuthorizedAt)
	}
	// The same for Burn, and for a panic.
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
		if err := tx.Burn(ctx, memT0); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Errorf("error = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
			_ = tx.Burn(ctx, memT0)
			panic("kaboom")
		})
	}()
	if a, _ := e.m.GetAuthorization(e.ctx, id); a.Status != StatusApproved || a.CodeUsedAt != nil {
		t.Errorf("a rolled back Burn is visible: %+v", a)
	}
	// And the lock is free afterwards.
	done := make(chan struct{})
	go func() { _, _ = e.m.CountDynamicApps(e.ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the store stayed locked after a panic in fn")
	}

	// A failure recorded in fn and nil returned commits (the Service burns a code that way).
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error { return tx.Burn(ctx, memT0.Add(time.Second)) })
	if err != nil {
		t.Fatal(err)
	}
	a, _ := e.m.GetAuthorization(e.ctx, id)
	if a.Status != StatusExchanged || a.CodeUsedAt == nil || !a.CodeUsedAt.Equal(memT0.Add(time.Second)) || a.GrantID != 0 {
		t.Errorf("burned row: %+v", a)
	}
	// A burned row can be looked up again (the replay check needs it) but cannot be completed.
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
		if tx.Authorization().Status != StatusExchanged {
			t.Error("the replay does not see the row as exchanged")
		}
		if _, err := tx.Complete(ctx, ng); !errors.Is(err, ErrAlreadyDecided) {
			t.Errorf("Complete of an exchanged row: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gs, _ := e.m.ListGrants(e.ctx, GrantFilter{}); len(gs) != 0 {
		t.Errorf("Complete wrote through an error: %+v", gs)
	}
}

func TestMemoryStoreComplete(t *testing.T) {
	e := newMemEnv(t)
	at := memT0.Add(time.Hour)
	res, ng := e.issue("user-1", 1, at)

	g := res.Grant
	if g.ID == 0 || g.AppID != e.app.ID || g.UserID != "user-1" || g.OrgID != 1 || g.Resource != "r" || !g.CreatedAt.Equal(at) || g.RevokedAt != nil ||
		!slices.Equal(g.Scopes, []string{ScopeProjectsRead, ScopeDatabaseRead}) {
		t.Errorf("grant: %+v", g)
	}
	if len(res.Superseded) != 0 {
		t.Errorf("superseded: %+v", res.Superseded)
	}
	acc, err := e.m.GetToken(e.ctx, ng.Access.Hash)
	if err != nil || acc.Kind != KindAccess || acc.GrantID != g.ID || acc.Prefix != "sbp_oaut" || !acc.ExpiresAt.Equal(at.Add(time.Hour)) || acc.ID == 0 {
		t.Errorf("access token: %+v, %v", acc, err)
	}
	ref, err := e.m.GetToken(e.ctx, ng.Refresh.Hash)
	if err != nil || ref.Kind != KindRefresh || ref.GrantID != g.ID || ref.UsedAt != nil || ref.ID == acc.ID {
		t.Errorf("refresh token: %+v, %v", ref, err)
	}
	if app, _ := e.m.GetApp(e.ctx, e.app.ID); app.LastAuthorizedAt == nil || !app.LastAuthorizedAt.Equal(at) {
		t.Errorf("last_authorized_at = %v", app.LastAuthorizedAt)
	}

	// The same (app, user, organization) again supersedes; the first grant keeps its tokens but is dead.
	res2, _ := e.issue("user-1", 1, at.Add(time.Minute))
	if len(res2.Superseded) != 1 || res2.Superseded[0].ID != g.ID || res2.Superseded[0].RevokedReason != ReasonSuperseded || res2.Superseded[0].RevokedAt == nil {
		t.Errorf("superseded: %+v", res2.Superseded)
	}
	if _, err := e.m.LookupAccess(e.ctx, ng.Access.Hash, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("the superseded grant still resolves: %v", err)
	}
	// Another user or organization is not touched.
	e.issue("user-2", 1, at.Add(2*time.Minute))
	e.issue("user-1", 2, at.Add(3*time.Minute))
	if live, _ := e.m.ListGrants(e.ctx, GrantFilter{Live: true}); len(live) != 3 {
		t.Errorf("%d live grants", len(live))
	}
	// MaxLive trims the oldest live grants of (user, organization): fresh apps would be needed to get
	// more than one live grant of a user in an organization, so make them directly.
	for i := 0; i < 4; i++ {
		app := memApp1(secrets.NewUUID(), RegistrationDynamic, 0)
		if err := e.m.CreateApp(e.ctx, app, nil); err != nil {
			t.Fatal(err)
		}
		e.app = app
		e.issue("user-3", 1, at.Add(time.Hour+time.Duration(i)*time.Minute))
	}
	live, _ := e.m.ListGrants(e.ctx, GrantFilter{UserID: "user-3", OrgID: 1, Live: true})
	if len(live) != 3 {
		t.Fatalf("MaxLive 3 left %d live grants", len(live))
	}
	all, _ := e.m.ListGrants(e.ctx, GrantFilter{UserID: "user-3", OrgID: 1})
	if len(all) != 4 || all[3].Grant.RevokedReason != ReasonSuperseded || all[3].Grant.RevokedAt == nil {
		t.Errorf("the oldest was not trimmed: %+v", all)
	}
	// Duplicate token hashes are refused before anything is written.
	_, h := e.approved("user-4", 1)
	dup := e.newGrant("user-4", 1, at)
	dup.Refresh.Hash = dup.Access.Hash
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error { _, err := tx.Complete(ctx, dup); return err })
	if !errors.Is(err, ErrConflict) {
		t.Errorf("equal token hashes: %v", err)
	}
	dup.Refresh.Hash = ng.Access.Hash // already stored
	err = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error { _, err := tx.Complete(ctx, dup); return err })
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a stored token hash: %v", err)
	}
	if gs, _ := e.m.ListGrants(e.ctx, GrantFilter{UserID: "user-4"}); len(gs) != 0 {
		t.Errorf("a refused Complete left grants: %+v", gs)
	}
}

// TestMemoryStoreWithCodeSerializes: concurrent callers for one code run one after the other.
func TestMemoryStoreWithCodeSerializes(t *testing.T) {
	e := newMemEnv(t)
	_, h := e.approved("user-1", 1)
	var inside, maxInside, completed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		ng := e.newGrant("user-1", 1, memT0)
		go func() {
			defer wg.Done()
			_ = e.m.WithCode(e.ctx, e.app.ID, h, func(ctx context.Context, tx CodeTx) error {
				n := inside.Add(1)
				if n > maxInside.Load() {
					maxInside.Store(n)
				}
				time.Sleep(time.Millisecond)
				defer inside.Add(-1)
				if _, err := tx.Complete(ctx, ng); err == nil {
					completed.Add(1)
				} else if !errors.Is(err, ErrAlreadyDecided) {
					t.Errorf("unexpected: %v", err)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 || completed.Load() != 1 {
		t.Errorf("%d callers inside at once, %d completions", maxInside.Load(), completed.Load())
	}
}

func TestMemoryStoreRotateRefresh(t *testing.T) {
	e := newMemEnv(t)
	at := memT0
	res, ng := e.issue("user-1", 1, at)
	grace := 10 * time.Second
	in := func(now time.Time) RotateInput {
		return RotateInput{AppID: e.app.ID, TokenHash: ng.Refresh.Hash, Now: now, Grace: grace}
	}
	issue := func() (Token, Token) {
		return Token{Hash: e.hash(), Prefix: "sbp_oaut", CreatedAt: at, ExpiresAt: at.Add(time.Hour)},
			Token{Hash: e.hash(), Prefix: "sbr_abcd", CreatedAt: at, ExpiresAt: at.Add(90 * 24 * time.Hour)}
	}

	// Not found: wrong app, an access token, unknown hash, expired token, malformed app id.
	noFn := func(context.Context, RotateTx) error { t.Error("fn ran"); return nil }
	for name, bad := range map[string]RotateInput{
		"another app":  {AppID: secrets.NewUUID(), TokenHash: ng.Refresh.Hash, Now: at, Grace: grace},
		"malformed":    {AppID: "x", TokenHash: ng.Refresh.Hash, Now: at, Grace: grace},
		"access token": {AppID: e.app.ID, TokenHash: ng.Access.Hash, Now: at, Grace: grace},
		"unknown":      {AppID: e.app.ID, TokenHash: e.hash(), Now: at, Grace: grace},
		"expired":      {AppID: e.app.ID, TokenHash: ng.Refresh.Hash, Now: at.Add(90 * 24 * time.Hour), Grace: grace},
	} {
		if err := e.m.RotateRefresh(e.ctx, bad, noFn); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// An error from fn rolls the stamp back.
	boom := errors.New("boom")
	err := e.m.RotateRefresh(e.ctx, in(at.Add(time.Minute)), func(ctx context.Context, tx RotateTx) error {
		if tx.Token().UsedAt == nil {
			t.Error("the handle's token is not stamped")
		}
		a, r := issue()
		if err := tx.Issue(ctx, a, r); err != nil {
			return err
		}
		if err := tx.NarrowScopes(ctx, []string{ScopeProjectsRead}); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Errorf("error = %v", err)
	}
	if tok, _ := e.m.GetToken(e.ctx, ng.Refresh.Hash); tok.UsedAt != nil || tok.ReplacedBy != 0 {
		t.Errorf("the stamp survives a failed rotation: %+v", tok)
	}
	if gr, _ := e.m.GetGrant(e.ctx, res.Grant.ID); !slices.Equal(gr.Scopes, res.Grant.Scopes) {
		t.Errorf("NarrowScopes survives a failed rotation: %v", gr.Scopes)
	}
	if n := len(e.m.tokens); n != 2 {
		t.Errorf("%d tokens after a failed rotation", n)
	}

	// A good rotation: stamped with in.Now, successor recorded, grant narrowed.
	now := at.Add(time.Minute)
	var na, nr Token
	err = e.m.RotateRefresh(e.ctx, in(now), func(ctx context.Context, tx RotateTx) error {
		if g := tx.Grant(); g.ID != res.Grant.ID || g.UserID != "user-1" {
			t.Errorf("handle grant: %+v", g)
		}
		na, nr = issue()
		if err := tx.Issue(ctx, na, nr); err != nil {
			return err
		}
		if err := tx.NarrowScopes(ctx, []string{ScopeDatabaseRead}); err != nil {
			return err
		}
		if err := tx.NarrowScopes(ctx, []string{ScopeProjectsRead, "billing:write"}); err == nil {
			t.Error("NarrowScopes widened the grant")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := e.m.GetToken(e.ctx, ng.Refresh.Hash)
	nrow, _ := e.m.GetToken(e.ctx, nr.Hash)
	if old.UsedAt == nil || !old.UsedAt.Equal(now) || old.ReplacedBy != nrow.ID || nrow.Kind != KindRefresh || nrow.GrantID != res.Grant.ID {
		t.Errorf("old %+v new %+v", old, nrow)
	}
	if gr, _ := e.m.GetGrant(e.ctx, res.Grant.ID); !slices.Equal(gr.Scopes, []string{ScopeDatabaseRead}) {
		t.Errorf("grant scopes %v", gr.Scopes)
	}

	// Inside the grace window the old token rotates again and keeps its first stamp and successor.
	later := now.Add(grace - time.Nanosecond)
	err = e.m.RotateRefresh(e.ctx, in(later), func(ctx context.Context, tx RotateTx) error {
		a, r := issue()
		return tx.Issue(ctx, a, r)
	})
	if err != nil {
		t.Fatalf("inside the grace window: %v", err)
	}
	if again, _ := e.m.GetToken(e.ctx, ng.Refresh.Hash); !again.UsedAt.Equal(now) || again.ReplacedBy != nrow.ID {
		t.Errorf("grace rotation changed the stamp or the successor: %+v", again)
	}
	// At grace it is a replay, with the grant named and nothing written.
	before := len(e.m.tokens)
	err = e.m.RotateRefresh(e.ctx, in(now.Add(grace)), noFn)
	var re *ReuseError
	if !errors.As(err, &re) || !errors.Is(err, ErrRefreshReused) || re.GrantID != res.Grant.ID || re.AppID != e.app.ID || re.UserID != "user-1" || re.OrgID != 1 {
		t.Fatalf("at the grace boundary: %v", err)
	}
	if len(e.m.tokens) != before {
		t.Error("a replay wrote tokens")
	}
	// A replay of a token of another app is plain not found, and of a revoked grant too.
	if err := e.m.RotateRefresh(e.ctx, RotateInput{AppID: secrets.NewUUID(), TokenHash: ng.Refresh.Hash, Now: now.Add(time.Hour), Grace: grace}, noFn); !errors.Is(err, ErrNotFound) {
		t.Errorf("replay by another app: %v", err)
	}
	if _, err := e.m.RevokeGrants(e.ctx, GrantFilter{ID: res.Grant.ID}, ReasonRefreshReuse, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RotateRefresh(e.ctx, in(now.Add(time.Hour)), noFn); !errors.Is(err, ErrNotFound) {
		t.Errorf("replay on a revoked grant: %v", err)
	}
}

func TestMemoryStoreRotateRevokeGrant(t *testing.T) {
	e := newMemEnv(t)
	res, ng := e.issue("user-1", 1, memT0)
	err := e.m.RotateRefresh(e.ctx, RotateInput{AppID: e.app.ID, TokenHash: ng.Refresh.Hash, Now: memT0, Grace: time.Second}, func(ctx context.Context, tx RotateTx) error {
		if err := tx.RevokeGrant(ctx, "bogus", memT0); err == nil {
			t.Error("a bad reason is accepted")
		}
		return tx.RevokeGrant(ctx, ReasonMembership, memT0.Add(time.Second))
	})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := e.m.GetGrant(e.ctx, res.Grant.ID)
	if g.RevokedAt == nil || !g.RevokedAt.Equal(memT0.Add(time.Second)) || g.RevokedReason != ReasonMembership {
		t.Errorf("grant: %+v", g)
	}
	if _, err := e.m.LookupAccess(e.ctx, ng.Access.Hash, memT0); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup after revoke: %v", err)
	}
}

func TestMemoryStoreLookupAccess(t *testing.T) {
	e := newMemEnv(t)
	res, ng := e.issue("user-1", 2, memT0)
	got, err := e.m.LookupAccess(e.ctx, ng.Access.Hash, memT0.Add(time.Hour-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenID == 0 || got.GrantID != res.Grant.ID || got.AppID != e.app.ID || got.AppName != e.app.Name || got.UserID != "user-1" || got.OrgID != 2 ||
		got.OrgSlug != "other" || got.Resource != "r" || got.Scopes != nil || !got.ExpiresAt.Equal(memT0.Add(time.Hour)) ||
		!slices.Equal(got.GrantScopes, []string{ScopeProjectsRead, ScopeDatabaseRead}) || !slices.Equal(got.AppScopes, e.app.Scopes) {
		t.Errorf("info: %+v", got)
	}
	for name, f := range map[string]func() error{
		"at expiry":     func() error { _, err := e.m.LookupAccess(e.ctx, ng.Access.Hash, memT0.Add(time.Hour)); return err },
		"refresh token": func() error { _, err := e.m.LookupAccess(e.ctx, ng.Refresh.Hash, memT0); return err },
		"unknown":       func() error { _, err := e.m.LookupAccess(e.ctx, e.hash(), memT0); return err },
	} {
		if err := f(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Deleting the app ends the lookup and revokes its grants.
	revoked, err := e.m.DeleteApp(e.ctx, e.app.ID, memT0.Add(time.Minute))
	if err != nil || len(revoked) != 1 || revoked[0].ID != res.Grant.ID || revoked[0].RevokedReason != ReasonAppDeleted {
		t.Errorf("DeleteApp: %+v, %v", revoked, err)
	}
	if _, err := e.m.LookupAccess(e.ctx, ng.Access.Hash, memT0); !errors.Is(err, ErrNotFound) {
		t.Errorf("after DeleteApp: %v", err)
	}
	if _, err := e.m.GetApp(e.ctx, e.app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetApp of a deleted app: %v", err)
	}
	if _, err := e.m.DeleteApp(e.ctx, e.app.ID, memT0); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteApp: %v", err)
	}
	if n, _ := e.m.CountDynamicApps(e.ctx); n != 0 {
		t.Errorf("a deleted app is counted: %d", n)
	}
	// A deleted app's grants still list, with the app.
	gs, _ := e.m.ListGrants(e.ctx, GrantFilter{ID: res.Grant.ID})
	if len(gs) != 1 || gs[0].App.ID != e.app.ID || gs[0].App.DeletedAt == nil || gs[0].OrgSlug != "other" {
		t.Errorf("listing after delete: %+v", gs)
	}
}

func TestMemoryStoreListAndRevokeGrants(t *testing.T) {
	e := newMemEnv(t)
	r1, _ := e.issue("user-1", 1, memT0)
	r2, _ := e.issue("user-2", 1, memT0.Add(time.Minute))
	r3, _ := e.issue("user-1", 2, memT0.Add(time.Minute)) // same time as r2: the higher id comes first

	ids := func(f GrantFilter) []int64 {
		gs, err := e.m.ListGrants(e.ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, g := range gs {
			out = append(out, g.Grant.ID)
		}
		return out
	}
	if got := ids(GrantFilter{}); !slices.Equal(got, []int64{r3.Grant.ID, r2.Grant.ID, r1.Grant.ID}) {
		t.Errorf("order: %v", got)
	}
	if got := ids(GrantFilter{UserID: "user-1"}); !slices.Equal(got, []int64{r3.Grant.ID, r1.Grant.ID}) {
		t.Errorf("by user: %v", got)
	}
	if got := ids(GrantFilter{UserID: "user-1", OrgID: 1}); !slices.Equal(got, []int64{r1.Grant.ID}) {
		t.Errorf("by user and org: %v", got)
	}
	if got := ids(GrantFilter{Limit: 2}); len(got) != 2 {
		t.Errorf("limit: %v", got)
	}
	if got := ids(GrantFilter{AppID: strings.ToUpper(e.app.ID)}); len(got) != 3 {
		t.Errorf("by app, upper case: %v", got)
	}
	if got := ids(GrantFilter{AppID: "garbage"}); len(got) != 0 {
		t.Errorf("by a malformed app: %v", got)
	}
	if got := ids(GrantFilter{ID: r2.Grant.ID}); !slices.Equal(got, []int64{r2.Grant.ID}) {
		t.Errorf("by id: %v", got)
	}

	// Revocation touches live grants only and keeps the first reason.
	at := memT0.Add(time.Hour)
	revoked, err := e.m.RevokeGrants(e.ctx, GrantFilter{UserID: "user-1"}, ReasonAdmin, at)
	if err != nil || len(revoked) != 2 || revoked[0].RevokedReason != ReasonAdmin || !revoked[0].RevokedAt.Equal(at) {
		t.Fatalf("revoked: %+v, %v", revoked, err)
	}
	again, err := e.m.RevokeGrants(e.ctx, GrantFilter{UserID: "user-1"}, ReasonOperator, at.Add(time.Hour))
	if err != nil || len(again) != 0 {
		t.Errorf("revoking twice: %+v, %v", again, err)
	}
	if g, _ := e.m.GetGrant(e.ctx, r1.Grant.ID); g.RevokedReason != ReasonAdmin || !g.RevokedAt.Equal(at) {
		t.Errorf("the first reason was overwritten: %+v", g)
	}
	if _, err := e.m.RevokeGrants(e.ctx, GrantFilter{}, "bogus", at); err == nil {
		t.Error("a bad reason is accepted")
	}
	if got := ids(GrantFilter{Live: true}); !slices.Equal(got, []int64{r2.Grant.ID}) {
		t.Errorf("live: %v", got)
	}
	// An empty filter selects every live grant (the Service guards it).
	all, err := e.m.RevokeGrants(e.ctx, GrantFilter{}, ReasonOperator, at)
	if err != nil || len(all) != 1 {
		t.Errorf("empty filter: %+v, %v", all, err)
	}
	if _, err := e.m.GetGrant(e.ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown grant: %v", err)
	}
}

func TestMemoryStoreTouch(t *testing.T) {
	e := newMemEnv(t)
	res, ng := e.issue("user-1", 1, memT0)
	acc, _ := e.m.GetToken(e.ctx, ng.Access.Hash)
	at := memT0.Add(time.Minute)
	if err := e.m.TouchToken(e.ctx, acc.ID, res.Grant.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := e.m.TouchToken(e.ctx, 9999, 9999, at); err != nil {
		t.Errorf("unknown ids: %v", err)
	}
	tok, _ := e.m.GetToken(e.ctx, ng.Access.Hash)
	g, _ := e.m.GetGrant(e.ctx, res.Grant.ID)
	if tok.LastUsedAt == nil || !tok.LastUsedAt.Equal(at) || g.LastUsedAt == nil || !g.LastUsedAt.Equal(at) {
		t.Errorf("token %v grant %v", tok.LastUsedAt, g.LastUsedAt)
	}
	sec := AppSecret{ID: secrets.NewUUID(), AppID: e.app.ID, Hash: e.hash(), CreatedAt: memT0}
	if err := e.m.CreateSecret(e.ctx, sec); err != nil {
		t.Fatal(err)
	}
	if err := e.m.TouchSecret(e.ctx, sec.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := e.m.TouchSecret(e.ctx, "nope", at); err != nil {
		t.Errorf("unknown secret: %v", err)
	}
	if ss, _ := e.m.ListSecrets(e.ctx, e.app.ID); len(ss) != 1 || ss[0].LastUsedAt == nil || !ss[0].LastUsedAt.Equal(at) {
		t.Errorf("secrets: %+v", ss)
	}
}

func TestMemoryStoreManualApps(t *testing.T) {
	e := newMemEnv(t)
	mk := func(org int64, at time.Time) App {
		a := memApp1(secrets.NewUUID(), RegistrationManual, org)
		a.CreatedAt = at
		if err := e.m.CreateApp(e.ctx, a, &AppSecret{ID: secrets.NewUUID(), Hash: e.hash(), CreatedAt: at, Alias: "sba_0000********"}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	b := mk(1, memT0.Add(time.Minute))
	a := mk(1, memT0)
	mk(2, memT0)
	list, _ := e.m.ListManualApps(e.ctx, 1)
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Errorf("order: %+v", list)
	}
	if n, _ := e.m.CountManualApps(e.ctx, 1); n != 2 {
		t.Errorf("count = %d", n)
	}
	// The first secret was stored with the app.
	if ss, _ := e.m.ListSecrets(e.ctx, a.ID); len(ss) != 1 || ss[0].AppID != a.ID || len(ss[0].Hash) != 32 {
		t.Errorf("secrets: %+v", ss)
	}
	// UpdateApp overwrites the editable fields only.
	up := a
	up.Name, up.Website, up.Icon, up.Scopes, up.RedirectURIs, up.UpdatedAt = "New", "https://w", "https://i", []string{ScopeAuthRead}, []string{"https://n/cb"}, memT0.Add(time.Hour)
	up.OrgID, up.CreatedBy, up.RegistrationType = 99, "someone", RegistrationDynamic
	if err := e.m.UpdateApp(e.ctx, up); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.GetApp(e.ctx, a.ID)
	if got.Name != "New" || got.Website != "https://w" || got.Icon != "https://i" || !slices.Equal(got.Scopes, []string{ScopeAuthRead}) ||
		!slices.Equal(got.RedirectURIs, []string{"https://n/cb"}) || !got.UpdatedAt.Equal(memT0.Add(time.Hour)) {
		t.Errorf("updated: %+v", got)
	}
	if got.OrgID != 1 || got.CreatedBy != "" || got.RegistrationType != RegistrationManual || !got.CreatedAt.Equal(memT0) {
		t.Errorf("UpdateApp changed what it must keep: %+v", got)
	}
	// A deleted app is invisible to the listing and the count; its secrets stay (the row exists).
	if _, err := e.m.DeleteApp(e.ctx, a.ID, memT0); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.m.ListManualApps(e.ctx, 1); len(list) != 1 || list[0].ID != b.ID {
		t.Errorf("after delete: %+v", list)
	}
	if err := e.m.UpdateApp(e.ctx, up); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a deleted app: %v", err)
	}
}

func TestMemoryStorePruneCascades(t *testing.T) {
	e := newMemEnv(t)
	// An app that is idle and old, with a revoked grant, tokens, an authorization and a secret.
	res, ng := e.issue("user-1", 1, memT0)
	if _, err := e.m.RevokeGrants(e.ctx, GrantFilter{ID: res.Grant.ID}, ReasonUser, memT0); err != nil {
		t.Fatal(err)
	}
	sec := AppSecret{ID: secrets.NewUUID(), AppID: e.app.ID, Hash: e.hash(), CreatedAt: memT0}
	if err := e.m.CreateSecret(e.ctx, sec); err != nil {
		t.Fatal(err)
	}
	far := memT0.Add(1000 * 24 * time.Hour)
	// Cutoffs at the zero time delete nothing.
	if r, err := e.m.Prune(e.ctx, PruneParams{}); err != nil || r != (PruneResult{}) {
		t.Fatalf("zero cutoffs: %+v, %v", r, err)
	}
	// Only the apps cutoff: the app goes, with everything under it, and counts only itself.
	r, err := e.m.Prune(e.ctx, PruneParams{IdleAppsBefore: far})
	if err != nil || r != (PruneResult{Apps: 1}) {
		t.Fatalf("result: %+v, %v", r, err)
	}
	if len(e.m.apps)+len(e.m.secrets)+len(e.m.auths)+len(e.m.grants)+len(e.m.tokens)+len(e.m.byHash) != 0 {
		t.Errorf("rows remain: %s", e.m.dumpForTest(t))
	}
	if _, err := e.m.GetToken(e.ctx, ng.Refresh.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("token after cascade: %v", err)
	}
}
