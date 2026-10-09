package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/secrets"
)

func testAuthorizationDecide(t *testing.T, e *env) {
	app, other := e.createApp(), e.createApp()
	mk := func(a oauth.App, ttl time.Duration) oauth.Authorization {
		au := oauth.Authorization{
			ID: secrets.NewUUID(), AppID: a.ID, RedirectURI: "http://127.0.0.1:8123/cb", Scopes: []string{"projects:read", "database:read"},
			State: "state-" + secrets.NewUUID(), CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", Resource: "https://api.example.com/mcp",
			OrgHint: "acme", CreatedAt: e.t0, ExpiresAt: e.t0.Add(ttl), Status: oauth.StatusPending,
		}
		e.must(e.st.CreateAuthorization(e.ctx, au))
		return au
	}
	a1, a2, a3, a4 := mk(app, 10*time.Minute), mk(app, 10*time.Minute), mk(other, 10*time.Minute), mk(app, time.Minute)

	// A pending request reads back whole; nothing is decided, no organization, no code.
	got, err := e.st.GetAuthorization(e.ctx, a1.ID)
	e.must(err)
	checkAuth(t, got, a1)
	eq(t, "pending OrgSlug", got.OrgSlug, "")
	e.wantErr(e.st.CreateAuthorization(e.ctx, a1), oauth.ErrConflict)
	for _, id := range []string{secrets.NewUUID(), "not-a-uuid"} {
		_, err = e.st.GetAuthorization(e.ctx, id)
		e.wantErr(err, oauth.ErrNotFound)
	}

	// CountPending counts the pending requests that have not expired: expires_at must be after now.
	count := func(appID string, now time.Time) int {
		t.Helper()
		n, err := e.st.CountPending(e.ctx, appID, now)
		e.must(err)
		return n
	}
	at := e.t0.Add(30 * time.Second)
	eq(t, "pending of app", count(app.ID, at), 3)
	eq(t, "pending of other app", count(other.ID, at), 1)
	eq(t, "pending of all", count("", at), 4)
	eq(t, "pending after a4 expired", count(app.ID, e.t0.Add(time.Minute)), 2)
	eq(t, "pending at the instant a1 expires", count(app.ID, e.t0.Add(10*time.Minute)), 0)
	eq(t, "pending of an unknown app", count(secrets.NewUUID(), at), 0)

	// Approval returns the updated row and changes nothing else.
	user, code := secrets.NewUUID(), hash()
	decided, codeExp := e.t0.Add(time.Minute-time.Second), e.t0.Add(2*time.Minute)
	won, err := e.st.DecideAuthorization(e.ctx, a1.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: user, At: decided, OrgID: e.orgA, CodeHash: code, CodeExpiresAt: codeExp,
	})
	e.must(err)
	want := a1
	want.Status, want.DecidedBy, want.DecidedAt, want.OrgID = oauth.StatusApproved, user, tp(decided), e.orgA
	want.CodeHash, want.CodeExpiresAt = code, tp(codeExp)
	checkAuth(t, won, want)
	got, err = e.st.GetAuthorization(e.ctx, a1.ID)
	e.must(err)
	checkAuth(t, got, want)
	e.checkSlug("approved, read back", got.OrgSlug, e.orgA)
	eq(t, "pending of app after a1 was decided", count(app.ID, at), 2)

	// A decided request cannot be decided again, either way, and keeps its first decision.
	_, err = e.st.DecideAuthorization(e.ctx, a1.ID, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: secrets.NewUUID(), At: decided})
	e.wantErr(err, oauth.ErrAlreadyDecided)
	_, err = e.st.DecideAuthorization(e.ctx, a1.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: secrets.NewUUID(), At: decided, OrgID: e.orgB, CodeHash: hash(), CodeExpiresAt: codeExp,
	})
	e.wantErr(err, oauth.ErrAlreadyDecided)
	got, err = e.st.GetAuthorization(e.ctx, a1.ID)
	e.must(err)
	checkAuth(t, got, want)

	// A refusal keeps no code and no organization.
	refused, err := e.st.DecideAuthorization(e.ctx, a2.ID, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: decided})
	e.must(err)
	want2 := a2
	want2.Status, want2.DecidedBy, want2.DecidedAt = oauth.StatusDeclined, user, tp(decided)
	checkAuth(t, refused, want2)
	_, err = e.st.DecideAuthorization(e.ctx, a2.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: user, At: decided, OrgID: e.orgA, CodeHash: hash(), CodeExpiresAt: codeExp,
	})
	e.wantErr(err, oauth.ErrAlreadyDecided)

	// A request that has expired (expires_at <= At) cannot be decided and stays pending.
	_, err = e.st.DecideAuthorization(e.ctx, a4.ID, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: a4.ExpiresAt})
	e.wantErr(err, oauth.ErrExpired)
	_, err = e.st.DecideAuthorization(e.ctx, a4.ID, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: a4.ExpiresAt.Add(time.Hour)})
	e.wantErr(err, oauth.ErrExpired)
	got, err = e.st.GetAuthorization(e.ctx, a4.ID)
	e.must(err)
	checkAuth(t, got, a4)
	// One second earlier it could still be decided.
	_, err = e.st.DecideAuthorization(e.ctx, a4.ID, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: a4.ExpiresAt.Add(-time.Second)})
	e.must(err)

	for _, id := range []string{secrets.NewUUID(), "not-a-uuid"} {
		_, err = e.st.DecideAuthorization(e.ctx, id, oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: decided})
		e.wantErr(err, oauth.ErrNotFound)
	}
	got, err = e.st.GetAuthorization(e.ctx, a3.ID)
	e.must(err)
	checkAuth(t, got, a3) // untouched
}

// testDecideConcurrent: two decisions on one request never both succeed.
func testDecideConcurrent(t *testing.T, e *env) {
	app := e.createApp()
	au := oauth.Authorization{
		ID: secrets.NewUUID(), AppID: app.ID, RedirectURI: "http://127.0.0.1:8123/cb", Scopes: []string{"projects:read"},
		CreatedAt: e.t0, ExpiresAt: e.t0.Add(time.Hour), Status: oauth.StatusPending,
	}
	e.must(e.st.CreateAuthorization(e.ctx, au))

	const n = 8
	user, at := secrets.NewUUID(), e.t0.Add(time.Minute)
	codes := make([][]byte, n)
	for i := range codes {
		codes[i] = hash()
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d := oauth.Decision{Status: oauth.StatusDeclined, DecidedBy: user, At: at}
			if i%2 == 0 {
				d = oauth.Decision{Status: oauth.StatusApproved, DecidedBy: user, At: at, OrgID: e.orgA, CodeHash: codes[i], CodeExpiresAt: at.Add(time.Minute)}
			}
			_, errs[i] = e.st.DecideAuthorization(e.ctx, au.ID, d)
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("decisions %d and %d both succeeded", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, oauth.ErrAlreadyDecided):
			t.Errorf("decision %d: %v, want nil or ErrAlreadyDecided", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no decision succeeded")
	}
	got, err := e.st.GetAuthorization(e.ctx, au.ID)
	e.must(err)
	if winner%2 == 0 {
		eq(t, "status", got.Status, oauth.StatusApproved)
		eqBytes(t, "code hash of the winner", got.CodeHash, codes[winner])
	} else {
		eq(t, "status", got.Status, oauth.StatusDeclined)
		eqBytes(t, "code hash of a refusal", got.CodeHash, nil)
	}
}

func testWithCode(t *testing.T, e *env) {
	app, other := e.createApp(), e.createApp()
	user, at := secrets.NewUUID(), e.t0.Add(time.Minute)
	approved, code := e.approve(app, user, e.orgA, at)

	// A code that is unknown, or belongs to another app, finds nothing and runs nothing.
	ran := false
	run := func(context.Context, oauth.CodeTx) error { ran = true; return nil }
	e.wantErr(e.st.WithCode(e.ctx, other.ID, code, run), oauth.ErrNotFound)
	e.wantErr(e.st.WithCode(e.ctx, app.ID, hash(), run), oauth.ErrNotFound)
	e.wantErr(e.st.WithCode(e.ctx, "not-a-uuid", code, run), oauth.ErrNotFound)
	e.wantErr(e.st.WithCode(e.ctx, app.ID, nil, run), oauth.ErrNotFound)
	if ran {
		t.Fatal("WithCode ran fn although it found no row")
	}

	// fn sees the row as it is; an error from fn undoes what fn wrote and is returned as it is.
	boom := errors.New("boom")
	burnAt := e.t0.Add(2 * time.Minute)
	err := e.st.WithCode(e.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		a := tx.Authorization()
		checkAuth(t, &a, approved)
		e.checkSlug("WithCode row", a.OrgSlug, e.orgA)
		a.Scopes[0] = "tampered" // a copy: it must not reach the store
		if err := tx.Burn(ctx, burnAt); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("WithCode returned %v, want the error of fn unchanged", err)
	}
	got, err := e.st.GetAuthorization(e.ctx, approved.ID)
	e.must(err)
	checkAuth(t, got, approved)
	eqStrings(t, "scopes after the copy was changed", got.Scopes, []string{"projects:read", "database:read"})

	// Burn, committed, spends the code: exchanged, used at burnAt, no grant, the hash kept.
	e.must(e.st.WithCode(e.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error { return tx.Burn(ctx, burnAt) }))
	spent := approved
	spent.Status, spent.CodeUsedAt = oauth.StatusExchanged, tp(burnAt)
	got, err = e.st.GetAuthorization(e.ctx, approved.ID)
	e.must(err)
	checkAuth(t, got, spent)

	// A spent code is still found, shows its state, and cannot be turned into a grant.
	var completeErr error
	e.must(e.st.WithCode(e.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		a := tx.Authorization()
		eq(t, "state of a spent code", a.Status, oauth.StatusExchanged)
		_, completeErr = tx.Complete(ctx, oauth.NewGrant{
			Grant:   oauth.Grant{AppID: app.ID, UserID: user, OrgID: e.orgA, Scopes: []string{"projects:read"}, Resource: approved.Resource, CreatedAt: burnAt},
			Access:  oauth.Token{Hash: hash(), Prefix: "sbp_oaut", CreatedAt: burnAt, ExpiresAt: burnAt.Add(time.Hour)},
			Refresh: oauth.Token{Hash: hash(), Prefix: "sbr_abcd", CreatedAt: burnAt, ExpiresAt: burnAt.Add(24 * time.Hour)},
			At:      burnAt,
		})
		return nil
	}))
	e.wantErr(completeErr, oauth.ErrAlreadyDecided)
	infos, err := e.st.ListGrants(e.ctx, oauth.GrantFilter{AppID: app.ID})
	e.must(err)
	if len(infos) != 0 {
		t.Errorf("a refused Complete left %d grants", len(infos))
	}
}

func testCompleteIssuesGrant(t *testing.T, e *env) {
	app := e.createApp()
	user, at := secrets.NewUUID(), e.t0.Add(time.Hour)
	approved, code := e.approve(app, user, e.orgA, at.Add(-time.Minute))
	ng := oauth.NewGrant{
		Grant:   oauth.Grant{AppID: app.ID, UserID: user, OrgID: e.orgA, Scopes: []string{"projects:read", "database:read"}, Resource: "https://api.example.com/mcp", CreatedAt: at},
		Access:  oauth.Token{Hash: hash(), Prefix: "sbp_oaut", CreatedAt: at, ExpiresAt: at.Add(oauth.AccessTokenTTL)},
		Refresh: oauth.Token{Hash: hash(), Prefix: "sbr_abcd", CreatedAt: at, ExpiresAt: at.Add(oauth.RefreshTokenTTL)},
		MaxLive: oauth.MaxLiveGrantsPerUserOrg, At: at,
	}
	var res *oauth.CompleteResult
	var second error
	extra := ng
	extra.Access.Hash, extra.Refresh.Hash = hash(), hash()
	e.must(e.st.WithCode(e.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		var err error
		if res, err = tx.Complete(ctx, ng); err != nil {
			return err
		}
		// A second Complete finds the row exchanged, and writes nothing.
		_, second = tx.Complete(ctx, extra)
		return nil
	}))
	e.wantErr(second, oauth.ErrAlreadyDecided)
	if res.Grant.ID == 0 || len(res.Superseded) != 0 {
		t.Fatalf("Complete = grant %d, superseded %v", res.Grant.ID, ids(res.Superseded))
	}
	want := ng.Grant
	want.ID = res.Grant.ID
	checkGrant(t, &res.Grant, want)

	grant, err := e.st.GetGrant(e.ctx, res.Grant.ID)
	e.must(err)
	checkGrant(t, grant, want)
	infos, err := e.st.ListGrants(e.ctx, oauth.GrantFilter{AppID: app.ID})
	e.must(err)
	if len(infos) != 1 {
		t.Errorf("the app has %d grants, want 1 (the second Complete must not write)", len(infos))
	}

	// The two tokens: the kinds are set by the store.
	acc, err := e.st.GetToken(e.ctx, ng.Access.Hash)
	e.must(err)
	ref, err := e.st.GetToken(e.ctx, ng.Refresh.Hash)
	e.must(err)
	for _, c := range []struct {
		got  *oauth.Token
		kind string
		want oauth.Token
	}{{acc, oauth.KindAccess, ng.Access}, {ref, oauth.KindRefresh, ng.Refresh}} {
		eq(t, c.kind+" id is set", c.got.ID != 0, true)
		eq(t, c.kind+" kind", c.got.Kind, c.kind)
		eq(t, c.kind+" grant", c.got.GrantID, res.Grant.ID)
		eq(t, c.kind+" prefix", c.got.Prefix, c.want.Prefix)
		eqBytes(t, c.kind+" hash", c.got.Hash, c.want.Hash)
		eqTime(t, c.kind+" CreatedAt", c.got.CreatedAt, c.want.CreatedAt)
		eqTime(t, c.kind+" ExpiresAt", c.got.ExpiresAt, c.want.ExpiresAt)
		eqTimePtr(t, c.kind+" UsedAt", c.got.UsedAt, time.Time{})
		eqTimePtr(t, c.kind+" LastUsedAt", c.got.LastUsedAt, time.Time{})
		eq(t, c.kind+" ReplacedBy", c.got.ReplacedBy, 0)
	}
	if acc.ID == ref.ID {
		t.Error("the access and the refresh token have one id")
	}
	for _, h := range [][]byte{extra.Access.Hash, extra.Refresh.Hash} {
		_, err = e.st.GetToken(e.ctx, h)
		e.wantErr(err, oauth.ErrNotFound)
	}

	// The access token resolves to everything the API needs.
	info, err := e.st.LookupAccess(e.ctx, ng.Access.Hash, at.Add(time.Minute))
	e.must(err)
	eq(t, "info.TokenID", info.TokenID, acc.ID)
	eqTime(t, "info.ExpiresAt", info.ExpiresAt, ng.Access.ExpiresAt)
	eq(t, "info.GrantID", info.GrantID, res.Grant.ID)
	eq(t, "info.AppID", info.AppID, app.ID)
	eq(t, "info.AppName", info.AppName, app.Name)
	eq(t, "info.UserID", info.UserID, user)
	eq(t, "info.OrgID", info.OrgID, e.orgA)
	e.checkSlug("info", info.OrgSlug, e.orgA)
	eq(t, "info.Resource", info.Resource, ng.Grant.Resource)
	eqStrings(t, "info.GrantScopes", info.GrantScopes, ng.Grant.Scopes)
	eqStrings(t, "info.AppScopes", info.AppScopes, app.Scopes)
	eqStrings(t, "info.Scopes (the Service fills it)", info.Scopes, nil)

	// The request is exchanged and points at the grant; the app was authorized.
	au, err := e.st.GetAuthorization(e.ctx, approved.ID)
	e.must(err)
	spent := approved
	spent.Status, spent.GrantID, spent.CodeUsedAt = oauth.StatusExchanged, res.Grant.ID, tp(at)
	checkAuth(t, au, spent)
	stored, err := e.st.GetApp(e.ctx, app.ID)
	e.must(err)
	eqTimePtr(t, "app.LastAuthorizedAt", stored.LastAuthorizedAt, at)
}

func testCompleteSupersedes(t *testing.T, e *env) {
	a, b := e.createApp(), e.createApp()
	u, u2 := secrets.NewUUID(), secrets.NewUUID()

	// The same app, user and organization: the newer grant replaces the older.
	at1, at2 := e.next(), e.next()
	g1 := e.grant(flow{app: a, user: u, org: e.orgA, at: at1})
	g2 := e.grant(flow{app: a, user: u, org: e.orgA, at: at2})
	if len(g1.superseded) != 0 || len(g2.superseded) != 1 {
		t.Fatalf("superseded = %v then %v, want none then the first grant", ids(g1.superseded), ids(g2.superseded))
	}
	checkGrant(t, &g2.superseded[0], revoked(g1.grant, oauth.ReasonSuperseded, at2))
	old, err := e.st.GetGrant(e.ctx, g1.grant.ID)
	e.must(err)
	checkGrant(t, old, revoked(g1.grant, oauth.ReasonSuperseded, at2))
	_, err = e.st.LookupAccess(e.ctx, g1.access.Hash, at2)
	e.wantErr(err, oauth.ErrNotFound)
	if _, err = e.st.LookupAccess(e.ctx, g2.access.Hash, at2); err != nil {
		t.Errorf("the newer grant's access token: %v", err)
	}

	// Another user, another organization or another app does not replace it.
	for _, f := range []flow{{app: a, user: u2, org: e.orgA}, {app: a, user: u, org: e.orgB}, {app: b, user: u, org: e.orgA}} {
		if g := e.grant(f); len(g.superseded) != 0 {
			t.Errorf("a grant for %+v superseded %v", f.user, ids(g.superseded))
		}
	}
	live, err := e.st.GetGrant(e.ctx, g2.grant.ID)
	e.must(err)
	checkGrant(t, live, g2.grant)

	// MaxLive keeps the newest grants of a user in an organization: the oldest beyond it are revoked.
	c := make([]oauth.App, 4)
	for i := range c {
		c[i] = e.createApp()
	}
	uc := secrets.NewUUID()
	var made []granted
	for i := 0; i < 3; i++ {
		g := e.grant(flow{app: c[i], user: uc, org: e.orgA, maxLive: 3})
		if len(g.superseded) != 0 {
			t.Fatalf("grant %d superseded %v, want none", i, ids(g.superseded))
		}
		made = append(made, g)
	}
	g4 := e.grant(flow{app: c[3], user: uc, org: e.orgA, maxLive: 3})
	if len(g4.superseded) != 1 {
		t.Fatalf("the fourth grant superseded %v, want the first", ids(g4.superseded))
	}
	checkGrant(t, &g4.superseded[0], revoked(made[0].grant, oauth.ReasonSuperseded, g4.at))
	stored, err := e.st.GetGrant(e.ctx, g4.grant.ID)
	e.must(err)
	checkGrant(t, stored, g4.grant) // the new grant is never the one trimmed

	// Replacing a grant of the same app and trimming happen in one Complete: the replaced one is listed first.
	g5 := e.grant(flow{app: c[0], user: uc, org: e.orgA, maxLive: 3}) // c[0] was revoked; live: c1, c2, c3 -> trims c1
	if len(g5.superseded) != 1 || g5.superseded[0].ID != made[1].grant.ID {
		t.Fatalf("the fifth grant superseded %v, want %d", ids(g5.superseded), made[1].grant.ID)
	}
	g6 := e.grant(flow{app: c[2], user: uc, org: e.orgA, maxLive: 2}) // live: c2, c3, c0(g5); replaces c2, then trims c3
	got := ids(g6.superseded)
	eqIDs(t, "superseded by the sixth grant (same app first, then the trimmed)", got, []int64{made[2].grant.ID, g4.grant.ID})
}

// testCodeSingleUseConcurrent is C2: many redemptions of one code at once, one winner.
func testCodeSingleUseConcurrent(t *testing.T, e *env) {
	app := e.createApp()
	user, at := secrets.NewUUID(), e.t0.Add(time.Hour)
	approved, code := e.approve(app, user, e.orgA, at.Add(-time.Minute))

	const n = 16
	type attempt struct {
		access, refresh []byte
		won             bool
		sawState        string
		res             *oauth.CompleteResult
		err             error
	}
	tries := make([]attempt, n)
	for i := range tries {
		tries[i].access, tries[i].refresh = hash(), hash()
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := &tries[i]
			<-start
			a.err = e.st.WithCode(e.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
				a.sawState = tx.Authorization().Status
				if a.sawState != oauth.StatusApproved {
					return nil // a spent code: the Service answers invalid_grant
				}
				res, err := tx.Complete(ctx, oauth.NewGrant{
					Grant:   oauth.Grant{AppID: app.ID, UserID: user, OrgID: e.orgA, Scopes: []string{"projects:read"}, Resource: approved.Resource, CreatedAt: at},
					Access:  oauth.Token{Hash: a.access, Prefix: "sbp_oaut", CreatedAt: at, ExpiresAt: at.Add(time.Hour)},
					Refresh: oauth.Token{Hash: a.refresh, Prefix: "sbr_abcd", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)},
					At:      at,
				})
				a.res, a.won = res, err == nil
				return err
			})
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, a := range tries {
		if a.err != nil {
			t.Errorf("redemption %d failed: %v", i, a.err)
		}
		switch {
		case a.won && winner >= 0:
			t.Fatalf("redemptions %d and %d both completed", winner, i)
		case a.won:
			winner = i
		case a.sawState != oauth.StatusExchanged:
			t.Errorf("redemption %d lost but saw state %q, want exchanged", i, a.sawState)
		}
	}
	if winner < 0 {
		t.Fatal("no redemption completed")
	}
	infos, err := e.st.ListGrants(e.ctx, oauth.GrantFilter{AppID: app.ID})
	e.must(err)
	if len(infos) != 1 || infos[0].Grant.ID != tries[winner].res.Grant.ID {
		t.Fatalf("grants after the race = %v, want only the winner's %d", infoIDs(infos), tries[winner].res.Grant.ID)
	}
	au, err := e.st.GetAuthorization(e.ctx, approved.ID)
	e.must(err)
	eq(t, "status", au.Status, oauth.StatusExchanged)
	eq(t, "grant of the request", au.GrantID, tries[winner].res.Grant.ID)
	for i, a := range tries {
		_, err := e.st.GetToken(e.ctx, a.access)
		if i == winner && err != nil {
			t.Errorf("the winner's access token: %v", err)
		}
		if i != winner && !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("the access token of loser %d: %v, want ErrNotFound", i, err)
		}
	}
}
