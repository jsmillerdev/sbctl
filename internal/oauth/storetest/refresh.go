package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
)

// rotate calls RotateRefresh with the grace window of the Service.
func (e *env) rotate(app oauth.App, token []byte, now time.Time, fn func(ctx context.Context, tx oauth.RotateTx) error) error {
	return e.st.RotateRefresh(e.ctx, oauth.RotateInput{AppID: app.ID, TokenHash: token, Now: now, Grace: oauth.RefreshGrace}, fn)
}

// pair returns a new access and a new refresh token, as the Service issues them at now.
func pair(now time.Time) (access, refresh oauth.Token) {
	access = oauth.Token{Hash: hash(), Prefix: "sbp_oaut", CreatedAt: now, ExpiresAt: now.Add(oauth.AccessTokenTTL)}
	refresh = oauth.Token{Hash: hash(), Prefix: "sbr_abcd", CreatedAt: now, ExpiresAt: now.Add(oauth.RefreshTokenTTL)}
	return access, refresh
}

// issue is the fn of a rotation that does what the Service does on success: it issues a new pair.
func issue(now time.Time) func(ctx context.Context, tx oauth.RotateTx) error {
	return func(ctx context.Context, tx oauth.RotateTx) error {
		access, refresh := pair(now)
		return tx.Issue(ctx, access, refresh)
	}
}

func testRefreshRotation(t *testing.T, e *env) {
	app := e.createApp()
	g := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	now1 := e.t0.Add(2 * time.Hour)

	// The first exchange: fn sees the token stamped and its grant, and issues the new pair.
	acc2, ref2 := pair(now1)
	e.must(e.rotate(app, g.refresh.Hash, now1, func(ctx context.Context, tx oauth.RotateTx) error {
		tok := tx.Token()
		eq(t, "token kind", tok.Kind, oauth.KindRefresh)
		eq(t, "token grant", tok.GrantID, g.grant.ID)
		eqBytes(t, "token hash", tok.Hash, g.refresh.Hash)
		eqTimePtr(t, "token UsedAt in fn", tok.UsedAt, now1)
		grant := tx.Grant()
		checkGrant(t, &grant, g.grant)
		return tx.Issue(ctx, acc2, ref2)
	}))
	used, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt", used.UsedAt, now1)
	r2, err := e.st.GetToken(e.ctx, ref2.Hash)
	e.must(err)
	eq(t, "new refresh kind", r2.Kind, oauth.KindRefresh)
	eq(t, "new refresh grant", r2.GrantID, g.grant.ID)
	eq(t, "new refresh prefix", r2.Prefix, ref2.Prefix)
	eqTime(t, "new refresh ExpiresAt", r2.ExpiresAt, ref2.ExpiresAt)
	eq(t, "ReplacedBy", used.ReplacedBy, r2.ID)
	eq(t, "the new refresh token is unused", r2.UsedAt == nil, true)
	info, err := e.st.LookupAccess(e.ctx, acc2.Hash, now1)
	e.must(err)
	eq(t, "new access token grant", info.GrantID, g.grant.ID)

	// Inside the grace window the token works again and issues a fresh pair; the first stamp and the
	// first replacement stay.
	now2 := now1.Add(oauth.RefreshGrace - time.Second)
	acc3, ref3 := pair(now2)
	e.must(e.rotate(app, g.refresh.Hash, now2, func(ctx context.Context, tx oauth.RotateTx) error {
		eqTimePtr(t, "token UsedAt, second exchange", tx.Token().UsedAt, now1)
		return tx.Issue(ctx, acc3, ref3)
	}))
	again, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt after the second exchange", again.UsedAt, now1)
	eq(t, "ReplacedBy after the second exchange", again.ReplacedBy, r2.ID)
	if _, err = e.st.LookupAccess(e.ctx, acc3.Hash, now2); err != nil {
		t.Errorf("access token of the second exchange: %v", err)
	}
	if _, err = e.st.GetToken(e.ctx, ref3.Hash); err != nil {
		t.Errorf("refresh token of the second exchange: %v", err)
	}

	// The chain goes on: the new refresh token is exchanged in turn.
	now3 := now1.Add(time.Minute)
	e.must(e.rotate(app, ref2.Hash, now3, issue(now3)))
	r2, err = e.st.GetToken(e.ctx, ref2.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt of the second refresh token", r2.UsedAt, now3)
	if r2.ReplacedBy == 0 {
		t.Error("the second refresh token has no ReplacedBy")
	}
}

func testRefreshReuse(t *testing.T, e *env) {
	app, other := e.createApp(), e.createApp()
	g := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	now1 := e.t0.Add(2 * time.Hour)
	e.must(e.rotate(app, g.refresh.Hash, now1, issue(now1)))

	// Less than the grace window after the first use: fine. Exactly the window or more: a replay.
	for _, d := range []time.Duration{0, time.Second, oauth.RefreshGrace - time.Second} {
		e.must(e.rotate(app, g.refresh.Hash, now1.Add(d), issue(now1.Add(d))))
	}
	for _, d := range []time.Duration{oauth.RefreshGrace, oauth.RefreshGrace + time.Second, time.Hour} {
		ran := false
		err := e.rotate(app, g.refresh.Hash, now1.Add(d), func(context.Context, oauth.RotateTx) error { ran = true; return nil })
		var re *oauth.ReuseError
		if !errors.As(err, &re) || !errors.Is(err, oauth.ErrRefreshReused) {
			t.Fatalf("%v after the first use: error = %v, want *ReuseError", d, err)
		}
		eq(t, "ReuseError.GrantID", re.GrantID, g.grant.ID)
		eq(t, "ReuseError.AppID", re.AppID, app.ID)
		eq(t, "ReuseError.UserID", re.UserID, g.grant.UserID)
		eq(t, "ReuseError.OrgID", re.OrgID, g.grant.OrgID)
		if ran {
			t.Fatalf("%v after the first use: fn ran for a replay", d)
		}
	}
	// A replay writes nothing: the grant is live and the stamp is the first one.
	grant, err := e.st.GetGrant(e.ctx, g.grant.ID)
	e.must(err)
	checkGrant(t, grant, g.grant)
	tok, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt after the replays", tok.UsedAt, now1)

	// What is not a replay is "not found": the answer must not tell a guess from a stale token.
	notFound := func(what string, app oauth.App, hash []byte, now time.Time) {
		t.Helper()
		err := e.rotate(app, hash, now, func(context.Context, oauth.RotateTx) error {
			t.Errorf("%s: fn ran", what)
			return nil
		})
		if !errors.Is(err, oauth.ErrNotFound) || errors.Is(err, oauth.ErrRefreshReused) {
			t.Errorf("%s: error = %v, want ErrNotFound", what, err)
		}
	}
	late := now1.Add(time.Hour)
	notFound("used token, another app", other, g.refresh.Hash, late)
	notFound("unknown token", app, hash(), late)
	notFound("empty token", app, nil, late)
	notFound("access token", app, g.access.Hash, late)

	exp := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	notFound("expired and never used", app, exp.refresh.Hash, exp.refresh.ExpiresAt)
	e.must(e.rotate(app, exp.refresh.Hash, exp.at.Add(time.Hour), issue(exp.at.Add(time.Hour))))
	notFound("expired and used", app, exp.refresh.Hash, exp.refresh.ExpiresAt)
	notFound("expired and used, long after", app, exp.refresh.Hash, exp.refresh.ExpiresAt.Add(time.Hour))

	gone := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	e.must(e.rotate(app, gone.refresh.Hash, now1, issue(now1)))
	_, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: gone.grant.ID}, oauth.ReasonAdmin, now1.Add(time.Minute))
	e.must(err)
	notFound("used token of a revoked grant", app, gone.refresh.Hash, late)
	fresh := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	_, err = e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: fresh.grant.ID}, oauth.ReasonAdmin, now1)
	e.must(err)
	notFound("unused token of a revoked grant", app, fresh.refresh.Hash, now1)
}

func testRefreshRollback(t *testing.T, e *env) {
	app := e.createApp()
	scopes := []string{"projects:read", "database:read", "organizations:read"}
	g := e.grant(flow{app: app, scopes: scopes, at: e.t0.Add(time.Hour)})
	now := e.t0.Add(2 * time.Hour)

	// An error from fn undoes the stamp, the narrowing and the new tokens, and is returned as it is.
	boom := errors.New("boom")
	acc, ref := pair(now)
	err := e.rotate(app, g.refresh.Hash, now, func(ctx context.Context, tx oauth.RotateTx) error {
		if err := tx.NarrowScopes(ctx, []string{"projects:read"}); err != nil {
			return err
		}
		if err := tx.Issue(ctx, acc, ref); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("RotateRefresh returned %v, want the error of fn unchanged", err)
	}
	tok, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt after the rollback", tok.UsedAt, time.Time{})
	eq(t, "ReplacedBy after the rollback", tok.ReplacedBy, 0)
	for _, h := range [][]byte{acc.Hash, ref.Hash} {
		_, err = e.st.GetToken(e.ctx, h)
		e.wantErr(err, oauth.ErrNotFound)
	}
	grant, err := e.st.GetGrant(e.ctx, g.grant.ID)
	e.must(err)
	eqStrings(t, "scopes after the rollback", grant.Scopes, scopes)
	// The token is still unused, so it still works.
	e.must(e.rotate(app, g.refresh.Hash, now, func(ctx context.Context, tx oauth.RotateTx) error {
		eqTimePtr(t, "UsedAt of a token whose first exchange was undone", tx.Token().UsedAt, now)
		return nil
	}))

	// NarrowScopes, committed, narrows the grant for good.
	g2 := e.grant(flow{app: app, scopes: scopes, at: e.t0.Add(time.Hour)})
	e.must(e.rotate(app, g2.refresh.Hash, now, func(ctx context.Context, tx oauth.RotateTx) error {
		if err := tx.NarrowScopes(ctx, []string{"database:read", "projects:read"}); err != nil {
			return err
		}
		return issue(now)(ctx, tx)
	}))
	grant, err = e.st.GetGrant(e.ctx, g2.grant.ID)
	e.must(err)
	eqStrings(t, "scopes after narrowing", grant.Scopes, []string{"database:read", "projects:read"})

	// RevokeGrant from fn, with fn returning nil, commits the revocation (and the stamp).
	g3 := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	e.must(e.rotate(app, g3.refresh.Hash, now, func(ctx context.Context, tx oauth.RotateTx) error {
		return tx.RevokeGrant(ctx, oauth.ReasonMembership, now)
	}))
	grant, err = e.st.GetGrant(e.ctx, g3.grant.ID)
	e.must(err)
	checkGrant(t, grant, revoked(g3.grant, oauth.ReasonMembership, now))
	_, err = e.st.LookupAccess(e.ctx, g3.access.Hash, now)
	e.wantErr(err, oauth.ErrNotFound)
	tok, err = e.st.GetToken(e.ctx, g3.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt of the token whose grant was revoked", tok.UsedAt, now)
}

// testRefreshConcurrent is T2: processes of one client refreshing at the same instant all get a
// pair, and the token keeps its first stamp; a refresh after the window is a replay for all.
func testRefreshConcurrent(t *testing.T, e *env) {
	app := e.createApp()
	g := e.grant(flow{app: app, at: e.t0.Add(time.Hour)})
	now := e.t0.Add(2 * time.Hour)

	const n = 8
	access, refresh := make([]oauth.Token, n), make([]oauth.Token, n)
	for i := range access {
		access[i], refresh[i] = pair(now)
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = e.rotate(app, g.refresh.Hash, now, func(ctx context.Context, tx oauth.RotateTx) error {
				return tx.Issue(ctx, access[i], refresh[i])
			})
		}()
	}
	close(start)
	wg.Wait()

	replacements := map[int64]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("exchange %d: %v", i, errs[i])
			continue
		}
		if _, err := e.st.LookupAccess(e.ctx, access[i].Hash, now); err != nil {
			t.Errorf("access token of exchange %d: %v", i, err)
		}
		r, err := e.st.GetToken(e.ctx, refresh[i].Hash)
		e.must(err)
		replacements[r.ID] = true
	}
	tok, err := e.st.GetToken(e.ctx, g.refresh.Hash)
	e.must(err)
	eqTimePtr(t, "UsedAt", tok.UsedAt, now)
	if !replacements[tok.ReplacedBy] {
		t.Errorf("ReplacedBy = %d, want the id of one of the %d new refresh tokens", tok.ReplacedBy, n)
	}

	// After the window every caller is told it is a replay, and none gets a token.
	for i := range errs {
		errs[i] = nil
		access[i], refresh[i] = pair(now.Add(time.Minute))
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.rotate(app, g.refresh.Hash, now.Add(oauth.RefreshGrace), func(ctx context.Context, tx oauth.RotateTx) error {
				return tx.Issue(ctx, access[i], refresh[i])
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, oauth.ErrRefreshReused) {
			t.Errorf("late exchange %d: %v, want a replay", i, err)
		}
		if _, err := e.st.GetToken(e.ctx, access[i].Hash); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("late exchange %d issued a token: %v", i, err)
		}
	}
}
