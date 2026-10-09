package storetest

import (
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/secrets"
)

func testLookupAccess(t *testing.T, e *env) {
	app := e.createApp()
	g := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	expires := g.access.ExpiresAt

	// Found while expires_at is after now; at the instant itself, and after, it is not.
	if _, err := e.st.LookupAccess(e.ctx, g.access.Hash, expires.Add(-time.Second)); err != nil {
		t.Fatalf("LookupAccess just before the expiry: %v", err)
	}
	for _, now := range []time.Time{expires, expires.Add(time.Second)} {
		_, err := e.st.LookupAccess(e.ctx, g.access.Hash, now)
		e.wantErr(err, oauth.ErrNotFound)
	}

	// Only an access token is an access token.
	live := expires.Add(-time.Minute)
	for name, h := range map[string][]byte{"refresh token": g.refresh.Hash, "unknown": hash(), "empty": nil} {
		_, err := e.st.LookupAccess(e.ctx, h, live)
		if err == nil {
			t.Errorf("LookupAccess(%s) found a token", name)
			continue
		}
		e.wantErr(err, oauth.ErrNotFound)
	}

	// TouchToken stamps the token and its grant; unknown ids are not an error.
	at := e.t0.Add(2 * time.Hour)
	info, err := e.st.LookupAccess(e.ctx, g.access.Hash, live)
	e.must(err)
	e.must(e.st.TouchToken(e.ctx, info.TokenID, info.GrantID, at))
	e.must(e.st.TouchToken(e.ctx, 1<<40, 1<<41, at))
	tok, err := e.st.GetToken(e.ctx, g.access.Hash)
	e.must(err)
	eqTimePtr(t, "token.LastUsedAt", tok.LastUsedAt, at)
	grant, err := e.st.GetGrant(e.ctx, g.grant.ID)
	e.must(err)
	eqTimePtr(t, "grant.LastUsedAt", grant.LastUsedAt, at)
	other, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "refresh token LastUsedAt", other.LastUsedAt, time.Time{})

	// A revoked grant ends the token at once.
	if _, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: g.grant.ID}, oauth.ReasonAdmin, at); err != nil {
		t.Fatal(err)
	}
	_, err = e.st.LookupAccess(e.ctx, g.access.Hash, live)
	e.wantErr(err, oauth.ErrNotFound)

	// GetToken and GetGrant show a token and a grant in any state.
	if tok, err = e.st.GetToken(e.ctx, g.access.Hash); err != nil {
		t.Errorf("GetToken of a revoked grant's token: %v", err)
	}
	_, err = e.st.GetToken(e.ctx, hash())
	e.wantErr(err, oauth.ErrNotFound)
	_, err = e.st.GetGrant(e.ctx, 1<<40)
	e.wantErr(err, oauth.ErrNotFound)
}

func testGrants(t *testing.T, e *env) {
	a, b := e.createApp(), e.createApp()
	u1, u2 := secrets.NewUUID(), secrets.NewUUID()
	at := func(m int) time.Time { return e.t0.Add(time.Duration(m) * time.Minute) }
	g1 := e.grant(flow{app: a, user: u1, org: e.orgA, at: at(1)})
	g2 := e.grant(flow{app: a, user: u2, org: e.orgA, at: at(2)})
	g3 := e.grant(flow{app: b, user: u1, org: e.orgB, at: at(3)})
	g4 := e.grant(flow{app: a, user: u1, org: e.orgB, at: at(4)})
	g5 := e.grant(flow{app: b, user: u2, org: e.orgA, at: at(4)}) // the same CreatedAt as g4: the id decides

	list := func(f oauth.GrantFilter) []int64 {
		t.Helper()
		infos, err := e.st.ListGrants(e.ctx, f)
		e.must(err)
		return infoIDs(infos)
	}
	// Newest first, ties by id descending.
	eqIDs(t, "all", list(oauth.GrantFilter{}), []int64{g5.grant.ID, g4.grant.ID, g3.grant.ID, g2.grant.ID, g1.grant.ID})
	eqIDs(t, "app a", list(oauth.GrantFilter{AppID: a.ID}), []int64{g4.grant.ID, g2.grant.ID, g1.grant.ID})
	eqIDs(t, "user u1", list(oauth.GrantFilter{UserID: u1}), []int64{g4.grant.ID, g3.grant.ID, g1.grant.ID})
	eqIDs(t, "org a", list(oauth.GrantFilter{OrgID: e.orgA}), []int64{g5.grant.ID, g2.grant.ID, g1.grant.ID})
	eqIDs(t, "id g3", list(oauth.GrantFilter{ID: g3.grant.ID}), []int64{g3.grant.ID})
	eqIDs(t, "app a and user u1", list(oauth.GrantFilter{AppID: a.ID, UserID: u1}), []int64{g4.grant.ID, g1.grant.ID})
	eqIDs(t, "limit 2", list(oauth.GrantFilter{Limit: 2}), []int64{g5.grant.ID, g4.grant.ID})
	eqIDs(t, "unknown app", list(oauth.GrantFilter{AppID: secrets.NewUUID()}), nil)
	eqIDs(t, "malformed app", list(oauth.GrantFilter{AppID: "not-a-uuid"}), nil)
	eqIDs(t, "malformed user", list(oauth.GrantFilter{UserID: "not-a-uuid"}), nil)

	// A listing carries the app and the organization slug.
	infos, err := e.st.ListGrants(e.ctx, oauth.GrantFilter{ID: g3.grant.ID})
	e.must(err)
	checkGrant(t, &infos[0].Grant, g3.grant)
	listedApp := b
	listedApp.LastAuthorizedAt = tp(at(4)) // set by every redemption: the last was g5's
	checkApp(t, &infos[0].App, listedApp)
	e.checkSlug("listed grant", infos[0].OrgSlug, e.orgB)

	// RevokeGrants returns the live grants it revoked, as updated, and keeps the first reason.
	revokedAt := at(10)
	got, err := e.st.RevokeGrants(e.ctx, oauth.GrantFilter{UserID: u2}, oauth.ReasonAdmin, revokedAt)
	e.must(err)
	sortGrants(got)
	if len(got) != 2 {
		t.Fatalf("RevokeGrants(u2) revoked %v, want g2 and g5", ids(got))
	}
	checkGrant(t, &got[0], revoked(g2.grant, oauth.ReasonAdmin, revokedAt))
	checkGrant(t, &got[1], revoked(g5.grant, oauth.ReasonAdmin, revokedAt))
	eqIDs(t, "live after revoking u2", list(oauth.GrantFilter{Live: true}), []int64{g4.grant.ID, g3.grant.ID, g1.grant.ID})

	later := at(11)
	got, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{AppID: a.ID}, oauth.ReasonOperator, later)
	e.must(err)
	eqIDs(t, "revoked for app a (g2 was revoked already)", sortedIDs(got), []int64{g1.grant.ID, g4.grant.ID})
	stored, err := e.st.GetGrant(e.ctx, g2.grant.ID)
	e.must(err)
	checkGrant(t, stored, revoked(g2.grant, oauth.ReasonAdmin, revokedAt))
	if got, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{AppID: a.ID}, oauth.ReasonOperator, later); err != nil || len(got) != 0 {
		t.Errorf("a second revocation = %v, %v; want nothing", ids(got), err)
	}
	if got, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{AppID: secrets.NewUUID()}, oauth.ReasonOperator, later); err != nil || len(got) != 0 {
		t.Errorf("revocation for an unknown app = %v, %v; want nothing", ids(got), err)
	}

	// An empty filter selects every live grant (the Service decides whether to ask for that).
	got, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{}, oauth.ReasonOperator, later)
	e.must(err)
	eqIDs(t, "revoked by the empty filter", ids(got), []int64{g3.grant.ID})
	eqIDs(t, "live after the empty filter", list(oauth.GrantFilter{Live: true}), nil)
}
