package oauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// TestPrune is R2: Prune deletes by the ages of section 2.16 and nothing younger.
func TestPrune(t *testing.T) {
	t.Run("authorizations", func(t *testing.T) {
		fx := newSvc(t)
		ra := fx.register()
		used := fx.grantFor(ra, testUserA, testOrgAcme) // keeps the app from being pruned; its own authorization is old
		old := fx.start(authorizeReq(&ra.App))
		fx.clock.Advance(48 * time.Hour)
		mid := fx.start(authorizeReq(&ra.App)) // expires 48h10m, so it expired 23h50m before the prune
		fx.clock.Advance(24*time.Hour - 5*time.Minute)
		pending := fx.start(authorizeReq(&ra.App))
		fx.clock.Advance(5 * time.Minute)

		res, err := fx.svc.Prune(fx.ctx())
		mustNoErr(t, err)
		if res.Authorizations != 2 || res.Tokens != 0 || res.Grants != 0 || res.Apps != 0 {
			t.Errorf("result: %+v", res)
		}
		for name, tc := range map[string]struct {
			id   string
			want bool
		}{"expired three days ago": {old, false}, "redeemed three days ago": {used.AuthID, false}, "expired 23h50m ago": {mid, true}, "still pending": {pending, true}} {
			_, err := fx.store.GetAuthorization(fx.ctx(), tc.id)
			if (err == nil) != tc.want {
				t.Errorf("authorization %s present=%v, want %v", name, err == nil, tc.want)
			}
		}
		// Idempotent: a second run, as another node might do at the same time, finds nothing.
		if res, err := fx.svc.Prune(fx.ctx()); err != nil || res != (PruneResult{}) {
			t.Errorf("second run: %+v, %v", res, err)
		}
	})

	t.Run("tokens", func(t *testing.T) {
		fx := newSvc(t)
		f := fx.grant(testUserA, testOrgAcme)
		first := f.Tokens
		fx.clock.Advance(10 * time.Hour)
		second, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(first.RefreshToken))
		mustNoErr(t, err)
		fx.clock.Advance(8*24*time.Hour - 12*time.Hour)
		third, err := fx.svc.Exchange(fx.ctx(), f.refreshRequest(second.RefreshToken))
		mustNoErr(t, err)
		fx.clock.Advance(2 * 24 * time.Hour)

		// Two access tokens expired over a week ago; the third expired two days ago.
		res, err := fx.svc.Prune(fx.ctx())
		mustNoErr(t, err)
		if res.Tokens != 2 || res.Grants != 0 || res.Apps != 0 {
			t.Errorf("result: %+v", res)
		}
		for tok, want := range map[string]bool{first.AccessToken: false, second.AccessToken: false, third.AccessToken: true, first.RefreshToken: true, third.RefreshToken: true} {
			_, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(tok))
			if (err == nil) != want {
				t.Errorf("token %.12s present=%v, want %v", tok, err == nil, want)
			}
		}
	})

	t.Run("revoked grants", func(t *testing.T) {
		fx := newSvc(t)
		old := fx.grant(testUserA, testOrgAcme)
		recent := fx.grant(testUserB, testOrgAcme)
		live := fx.grant(testUserA, testOrgOther)
		id := func(f *grantFlow) int64 {
			tok, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(f.Tokens.RefreshToken))
			mustNoErr(t, err)
			return tok.GrantID
		}
		oldID, recentID, liveID := id(old), id(recent), id(live)
		mustNoErr(t, fx.svc.RevokeGrant(fx.ctx(), oldID, ReasonUser, testUserA))
		fx.clock.Advance(2 * 24 * time.Hour)
		mustNoErr(t, fx.svc.RevokeGrant(fx.ctx(), recentID, ReasonUser, testUserB))
		fx.clock.Advance(RevokedGrantRetention - 24*time.Hour) // old: 31 days, recent: 29

		res, err := fx.svc.Prune(fx.ctx())
		mustNoErr(t, err)
		if res.Grants != 1 {
			t.Errorf("result: %+v", res)
		}
		for gid, want := range map[int64]bool{oldID: false, recentID: true, liveID: true} {
			_, err := fx.store.GetGrant(fx.ctx(), gid)
			if (err == nil) != want {
				t.Errorf("grant %d present=%v, want %v", gid, err == nil, want)
			}
		}
		// A pruned grant takes its tokens with it.
		if _, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(old.Tokens.RefreshToken)); !errors.Is(err, ErrNotFound) {
			t.Errorf("a token of a pruned grant: %v", err)
		}
	})

	t.Run("apps", func(t *testing.T) {
		fx := newSvc(t)
		unusedOld := fx.register()
		idleOld := fx.register()
		idleRecent := fx.register()
		inUse := fx.register()
		manual, _ := fx.manual(testOrgAcme)

		idleOldFlow := fx.grantFor(idleOld, testUserA, testOrgAcme)
		inUseFlow := fx.grantFor(inUse, testUserA, testOrgAcme)
		fx.clock.Advance(30 * 24 * time.Hour)
		idleRecentFlow := fx.grantFor(idleRecent, testUserA, testOrgAcme)
		mustNoErr(t, fx.svc.RevokeGrant(fx.ctx(), grantOf(t, fx, idleOldFlow), ReasonUser, testUserA))
		mustNoErr(t, fx.svc.RevokeGrant(fx.ctx(), grantOf(t, fx, idleRecentFlow), ReasonUser, testUserA))
		unusedNew := fx.register() // registered 30 days in
		fx.clock.Advance(IdleAppRetention - 30*24*time.Hour + time.Hour)
		// Now: idleOld authorized 90 days and an hour ago; idleRecent 60 days ago; unusedNew 60 days old
		// too and never used; inUse authorized long ago but its grant is live. unusedNew is pruned as well
		// (unused for over a day), so register a truly young one right before the prune.
		young := fx.register()

		res, err := fx.svc.Prune(fx.ctx())
		mustNoErr(t, err)
		if res.Apps != 3 { // unusedOld, unusedNew, idleOld
			t.Errorf("result: %+v", res)
		}
		for name, tc := range map[string]struct {
			id   string
			want bool
		}{
			"unused for over a day (from the start)": {unusedOld.App.ID, false},
			"unused for over a day (later)":          {unusedNew.App.ID, false},
			"idle for over 90 days":                  {idleOld.App.ID, false},
			"idle for 60 days":                       {idleRecent.App.ID, true},
			"a live grant":                           {inUse.App.ID, true},
			"registered a moment ago":                {young.App.ID, true},
			"a manual app is never pruned":           {manual.ID, true},
		} {
			_, err := fx.store.GetApp(fx.ctx(), tc.id)
			if (err == nil) != tc.want {
				t.Errorf("%s: present=%v, want %v", name, err == nil, tc.want)
			}
		}
		// The pruned app took its grants and secrets with it.
		if ss, _ := fx.store.ListSecrets(fx.ctx(), idleOld.App.ID); len(ss) != 0 {
			t.Errorf("secrets of a pruned app: %+v", ss)
		}
		if _, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(idleOldFlow.Tokens.RefreshToken)); !errors.Is(err, ErrNotFound) {
			t.Errorf("a token of a pruned app: %v", err)
		}
		// The app in use still serves its token (the access token is long expired, the grant is alive).
		if _, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(inUseFlow.Tokens.RefreshToken)); err != nil {
			t.Errorf("the refresh token of the live grant: %v", err)
		}
	})
}

func grantOf(t *testing.T, fx *svcFixture, f *grantFlow) int64 {
	t.Helper()
	tok, err := fx.store.GetToken(fx.ctx(), secrets.HashToken(f.Tokens.RefreshToken))
	mustNoErr(t, err)
	return tok.GrantID
}

// TestLazyPrune: Register and Exchange prune, at most once per PruneEvery, and a failing prune never
// fails the request.
func TestLazyPrune(t *testing.T) {
	fx := newSvc(t)
	fx.enableLazyPrune()
	stale := func() string {
		id := newUUID()
		app := fx.register()
		a := Authorization{ID: id, AppID: app.App.ID, RedirectURI: testRedirect, Scopes: []string{ScopeProjectsRead},
			CreatedAt: fx.clock.Now().Add(-48 * time.Hour), ExpiresAt: fx.clock.Now().Add(-47 * time.Hour), Status: StatusPending}
		mustNoErr(t, fx.store.CreateAuthorization(fx.ctx(), a))
		return id
	}
	present := func(id string) bool { _, err := fx.store.GetAuthorization(fx.ctx(), id); return err == nil }

	first := stale() // fx.register inside stale() ran the first lazy prune, before this row existed
	if !present(first) {
		t.Fatal("setup")
	}
	fx.clock.Advance(PruneEvery - time.Second)
	fx.register()
	if !present(first) {
		t.Error("pruned again before PruneEvery")
	}
	fx.clock.Advance(time.Second)
	fx.register()
	if present(first) {
		t.Error("not pruned after PruneEvery")
	}

	// Exchange prunes as well, even when it fails.
	second := stale()
	fx.clock.Advance(PruneEvery)
	_, err := fx.svc.Exchange(fx.ctx(), TokenRequest{GrantType: "password"})
	wantCode(t, err, CodeUnsupportedGrantType)
	if present(second) {
		t.Error("Exchange did not prune")
	}

	// A store that cannot prune does not fail the request; the failure is logged.
	fx.clock.Advance(PruneEvery)
	broken := &pruneFails{Store: fx.store}
	fx.svc.Store = broken
	if _, err := fx.svc.Register(fx.ctx(), RegisterRequest{ClientName: "C", RedirectURIs: []string{"http://localhost/cb"}}); err != nil {
		t.Fatalf("Register failed because pruning failed: %v", err)
	}
	if broken.calls != 1 || !strings.Contains(fx.logbuf.String(), "pruning failed") {
		t.Errorf("prune calls %d, log %q", broken.calls, fx.logbuf.String())
	}
}

type pruneFails struct {
	Store
	calls int
}

func (p *pruneFails) Prune(context.Context, PruneParams) (PruneResult, error) {
	p.calls++
	return PruneResult{}, errors.New("disk on fire")
}
