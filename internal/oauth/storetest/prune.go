package storetest

import (
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/secrets"
)

// The prune tests set one cutoff at a time: a cascade (an app takes its grants, a grant its tokens)
// may add to the counts of a PruneResult in one store and not in another, so a test asserts the
// count of the statement it exercises and looks at the rows for the rest.

func (e *env) prune(p oauth.PruneParams) oauth.PruneResult {
	e.t.Helper()
	res, err := e.st.Prune(e.ctx, p)
	e.must(err)
	return res
}

func (e *env) appExists(app oauth.App) bool {
	e.t.Helper()
	_, err := e.st.GetApp(e.ctx, app.ID)
	if err != nil {
		e.wantErr(err, oauth.ErrNotFound)
	}
	return err == nil
}

func testPruneNothing(t *testing.T, e *env) {
	app := e.createApp()
	g := e.grant(flow{app: app})
	old := oauth.Authorization{ID: secrets.NewUUID(), AppID: app.ID, RedirectURI: "http://127.0.0.1/cb", CreatedAt: e.t0, ExpiresAt: e.t0.Add(time.Minute), Status: oauth.StatusPending}
	e.must(e.st.CreateAuthorization(e.ctx, old))

	if res := e.prune(oauth.PruneParams{}); res != (oauth.PruneResult{}) {
		t.Errorf("Prune with no cutoff = %+v, want nothing", res)
	}
	_, err := e.st.GetAuthorization(e.ctx, old.ID)
	e.must(err)
	_, err = e.st.GetToken(e.ctx, g.access.Hash)
	e.must(err)
	if !e.appExists(app) {
		t.Error("Prune with no cutoff deleted the app")
	}
}

func testPruneAuthorizations(t *testing.T, e *env) {
	app := e.createApp()
	mk := func(expires time.Duration) string {
		a := oauth.Authorization{ID: secrets.NewUUID(), AppID: app.ID, RedirectURI: "http://127.0.0.1/cb", CreatedAt: e.t0, ExpiresAt: e.t0.Add(expires), Status: oauth.StatusPending}
		e.must(e.st.CreateAuthorization(e.ctx, a))
		return a.ID
	}
	old, edge, fresh := mk(time.Hour), mk(2*time.Hour), mk(3*time.Hour)

	// expires_at before the cutoff goes; at the cutoff it stays.
	if res := e.prune(oauth.PruneParams{AuthorizationsBefore: e.t0.Add(2 * time.Hour)}); res.Authorizations != 1 {
		t.Errorf("Prune deleted %d authorizations, want 1", res.Authorizations)
	}
	_, err := e.st.GetAuthorization(e.ctx, old)
	e.wantErr(err, oauth.ErrNotFound)
	for _, id := range []string{edge, fresh} {
		if _, err := e.st.GetAuthorization(e.ctx, id); err != nil {
			t.Errorf("authorization %s: %v", id, err)
		}
	}
	// Idempotent.
	if res := e.prune(oauth.PruneParams{AuthorizationsBefore: e.t0.Add(2 * time.Hour)}); res.Authorizations != 0 {
		t.Errorf("a second Prune deleted %d authorizations, want 0", res.Authorizations)
	}
}

func testPruneTokens(t *testing.T, e *env) {
	app := e.createApp()
	g := e.grant(flow{app: app, at: e.t0.Add(time.Hour)}) // the access token expires at t0+2h
	expires := g.access.ExpiresAt

	if res := e.prune(oauth.PruneParams{TokensBefore: expires}); res.Tokens != 0 {
		t.Errorf("Prune at the expiry deleted %d tokens, want 0", res.Tokens)
	}
	if _, err := e.st.GetToken(e.ctx, g.access.Hash); err != nil {
		t.Fatalf("the access token, pruned at its expiry: %v", err)
	}
	if res := e.prune(oauth.PruneParams{TokensBefore: expires.Add(time.Second)}); res.Tokens != 1 {
		t.Errorf("Prune deleted %d tokens, want the expired access token", res.Tokens)
	}
	_, err := e.st.GetToken(e.ctx, g.access.Hash)
	e.wantErr(err, oauth.ErrNotFound)
	if _, err := e.st.GetToken(e.ctx, g.refresh.Hash); err != nil {
		t.Errorf("the refresh token: %v", err)
	}
	if _, err := e.st.GetGrant(e.ctx, g.grant.ID); err != nil {
		t.Errorf("the grant: %v", err)
	}
	if res := e.prune(oauth.PruneParams{TokensBefore: expires.Add(time.Second)}); res.Tokens != 0 {
		t.Errorf("a second Prune deleted %d tokens, want 0", res.Tokens)
	}
}

func testPruneGrants(t *testing.T, e *env) {
	app := e.createApp()
	gOld := e.grant(flow{app: app})
	gEdge := e.grant(flow{app: app})
	gLive := e.grant(flow{app: app})
	_, err := e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: gOld.grant.ID}, oauth.ReasonUser, e.t0.Add(time.Hour))
	e.must(err)
	_, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: gEdge.grant.ID}, oauth.ReasonUser, e.t0.Add(3*time.Hour))
	e.must(err)

	// revoked_at before the cutoff goes, with its tokens; a live grant never goes.
	if res := e.prune(oauth.PruneParams{RevokedGrantsBefore: e.t0.Add(3 * time.Hour)}); res.Grants != 1 {
		t.Errorf("Prune deleted %d grants, want 1", res.Grants)
	}
	_, err = e.st.GetGrant(e.ctx, gOld.grant.ID)
	e.wantErr(err, oauth.ErrNotFound)
	for _, h := range [][]byte{gOld.access.Hash, gOld.refresh.Hash} {
		_, err = e.st.GetToken(e.ctx, h)
		e.wantErr(err, oauth.ErrNotFound)
	}
	for _, g := range []granted{gEdge, gLive} {
		if _, err := e.st.GetGrant(e.ctx, g.grant.ID); err != nil {
			t.Errorf("grant %d: %v", g.grant.ID, err)
		}
		if _, err := e.st.GetToken(e.ctx, g.access.Hash); err != nil {
			t.Errorf("token of grant %d: %v", g.grant.ID, err)
		}
	}
	if res := e.prune(oauth.PruneParams{RevokedGrantsBefore: e.t0.Add(3*time.Hour + time.Second)}); res.Grants != 1 {
		t.Errorf("Prune deleted %d grants, want the one revoked at the old cutoff", res.Grants)
	}
	if _, err := e.st.GetGrant(e.ctx, gLive.grant.ID); err != nil {
		t.Errorf("the live grant: %v", err)
	}
}

func testPruneUnusedApps(t *testing.T, e *env) {
	mkDynamic := func(created time.Duration) oauth.App {
		a := e.dynamicApp()
		a.CreatedAt, a.UpdatedAt = e.t0.Add(created), e.t0.Add(created)
		e.must(e.st.CreateApp(e.ctx, a, nil))
		return a
	}
	old, fresh, used := mkDynamic(time.Hour), mkDynamic(3*time.Hour), mkDynamic(time.Hour)
	manual := e.manualApp(e.orgA)
	manual.CreatedAt = e.t0.Add(time.Hour)
	e.must(e.st.CreateApp(e.ctx, manual, nil))
	e.grant(flow{app: used}) // authorized once

	// A dynamic app nobody authorized and older than the cutoff goes. A manual app is not pruned.
	if res := e.prune(oauth.PruneParams{UnusedAppsBefore: e.t0.Add(2 * time.Hour)}); res.Apps != 1 {
		t.Errorf("Prune deleted %d apps, want 1", res.Apps)
	}
	if e.appExists(old) {
		t.Error("the unused old app was kept")
	}
	for name, a := range map[string]oauth.App{"recent": fresh, "authorized": used, "manual": manual} {
		if !e.appExists(a) {
			t.Errorf("the %s app was deleted", name)
		}
	}
	if res := e.prune(oauth.PruneParams{UnusedAppsBefore: e.t0.Add(2 * time.Hour)}); res.Apps != 0 {
		t.Errorf("a second Prune deleted %d apps, want 0", res.Apps)
	}
}

func testPruneIdleApps(t *testing.T, e *env) {
	mkDynamic := func(created time.Duration) oauth.App {
		a := e.dynamicApp()
		a.CreatedAt, a.UpdatedAt = e.t0.Add(created), e.t0.Add(created)
		e.must(e.st.CreateApp(e.ctx, a, nil))
		return a
	}
	revokeAll := func(g granted) {
		_, err := e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: g.grant.ID}, oauth.ReasonUser, g.at.Add(time.Minute))
		e.must(err)
	}
	idle, live, recent := mkDynamic(0), mkDynamic(0), mkDynamic(0)
	never, neverNew := mkDynamic(time.Hour), mkDynamic(3*time.Hour)
	manual := e.manualApp(e.orgA)
	e.must(e.st.CreateApp(e.ctx, manual, nil))
	revokeAll(e.grant(flow{app: idle, at: e.t0.Add(time.Hour)}))
	e.grant(flow{app: live, at: e.t0.Add(time.Hour)})
	revokeAll(e.grant(flow{app: recent, at: e.t0.Add(3 * time.Hour)}))

	// A dynamic app with no live grant whose last authorization (or creation, if it never had one) is
	// before the cutoff goes.
	if res := e.prune(oauth.PruneParams{IdleAppsBefore: e.t0.Add(2 * time.Hour)}); res.Apps != 2 {
		t.Errorf("Prune deleted %d apps, want 2", res.Apps)
	}
	for name, a := range map[string]oauth.App{"idle": idle, "never authorized": never} {
		if e.appExists(a) {
			t.Errorf("the %s app was kept", name)
		}
	}
	for name, a := range map[string]oauth.App{"with a live grant": live, "authorized recently": recent, "created recently": neverNew, "manual": manual} {
		if !e.appExists(a) {
			t.Errorf("the app %s was deleted", name)
		}
	}
}
