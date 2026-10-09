package storetest

import (
	"sort"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
)

func testAppLifecycle(t *testing.T, e *env) {
	app := e.dynamicApp()
	sec := e.secret(app.ID)
	e.must(e.st.CreateApp(e.ctx, app, &sec))

	got, err := e.st.GetApp(e.ctx, app.ID)
	e.must(err)
	checkApp(t, got, app)
	secrets, err := e.st.ListSecrets(e.ctx, app.ID)
	e.must(err)
	if len(secrets) != 1 {
		t.Fatalf("ListSecrets = %d secrets, want the first one", len(secrets))
	}
	checkSecret(t, secrets[0], sec)

	// An app with an organization and no secret.
	manual := e.manualApp(e.orgA)
	e.must(e.st.CreateApp(e.ctx, manual, nil))
	got, err = e.st.GetApp(e.ctx, manual.ID)
	e.must(err)
	checkApp(t, got, manual)
	if secrets, err = e.st.ListSecrets(e.ctx, manual.ID); err != nil || len(secrets) != 0 {
		t.Errorf("ListSecrets(app without secret) = %v, %v; want none", secrets, err)
	}

	// A taken id or a taken secret hash is ErrConflict, and the pair is written whole or not at all.
	e.wantErr(e.st.CreateApp(e.ctx, app, nil), oauth.ErrConflict)
	taken := e.dynamicApp()
	dup := e.secret(taken.ID)
	dup.Hash = sec.Hash
	e.wantErr(e.st.CreateApp(e.ctx, taken, &dup), oauth.ErrConflict)
	_, err = e.st.GetApp(e.ctx, taken.ID)
	e.wantErr(err, oauth.ErrNotFound)

	// Unknown and malformed ids are "not found".
	for _, id := range []string{uuid(), "not-a-uuid", ""} {
		_, err = e.st.GetApp(e.ctx, id)
		e.wantErr(err, oauth.ErrNotFound)
	}

	// UpdateApp overwrites Name, Website, Icon, RedirectURIs, Scopes and UpdatedAt; the rest is kept.
	later := e.t0.Add(time.Hour)
	upd := app
	upd.Name, upd.Website, upd.Icon = "Renamed", "https://other.example", "https://other.example/i.png"
	upd.RedirectURIs = []string{"https://other.example/cb", "http://localhost/cb"}
	upd.Scopes = []string{"projects:read"}
	upd.UpdatedAt = later
	upd.RegistrationType, upd.OrgID = oauth.RegistrationManual, e.orgB
	upd.TokenEndpointAuthMethod, upd.CreatedBy, upd.CreatedAt = oauth.AuthMethodPost, uuid(), later
	e.must(e.st.UpdateApp(e.ctx, upd))
	want := app
	want.Name, want.Website, want.Icon = upd.Name, upd.Website, upd.Icon
	want.RedirectURIs, want.Scopes, want.UpdatedAt = upd.RedirectURIs, upd.Scopes, later
	got, err = e.st.GetApp(e.ctx, app.ID)
	e.must(err)
	checkApp(t, got, want)

	missing := app
	missing.ID = uuid()
	e.wantErr(e.st.UpdateApp(e.ctx, missing), oauth.ErrNotFound)
	missing.ID = "not-a-uuid"
	e.wantErr(e.st.UpdateApp(e.ctx, missing), oauth.ErrNotFound)
}

func testAppListsAndCounts(t *testing.T, e *env) {
	for i := 0; i < 2; i++ {
		a := e.dynamicApp()
		a.CreatedAt = e.t0.Add(time.Duration(i+1) * time.Second)
		e.must(e.st.CreateApp(e.ctx, a, nil))
	}
	// Two manual apps of one organization created at the same instant: the id breaks the tie.
	m1, m2 := e.manualApp(e.orgA), e.manualApp(e.orgA)
	m3, m4 := e.manualApp(e.orgB), e.manualApp(e.orgA)
	m1.CreatedAt, m2.CreatedAt, m3.CreatedAt, m4.CreatedAt = e.t0.Add(5*time.Second), e.t0.Add(5*time.Second), e.t0.Add(time.Second), e.t0.Add(time.Second)
	for _, a := range []oauth.App{m1, m2, m3, m4} {
		e.must(e.st.CreateApp(e.ctx, a, nil))
	}
	_, err := e.st.DeleteApp(e.ctx, m4.ID, e.t0.Add(time.Minute))
	e.must(err)

	if n, err := e.st.CountDynamicApps(e.ctx); err != nil || n != 2 {
		t.Errorf("CountDynamicApps = %d, %v; want 2", n, err)
	}
	for org, want := range map[int64]int{e.orgA: 2, e.orgB: 1, 999: 0} {
		if n, err := e.st.CountManualApps(e.ctx, org); err != nil || n != want {
			t.Errorf("CountManualApps(%d) = %d, %v; want %d", org, n, err, want)
		}
	}

	list, err := e.st.ListManualApps(e.ctx, e.orgA)
	e.must(err)
	tied := []oauth.App{m1, m2}
	sort.Slice(tied, func(i, j int) bool { return tied[i].ID < tied[j].ID })
	if len(list) != 2 {
		t.Fatalf("ListManualApps(A) has %d apps, want 2 (not the dynamic ones, not the deleted one)", len(list))
	}
	for i := range list {
		checkApp(t, &list[i], tied[i])
	}
	list, err = e.st.ListManualApps(e.ctx, e.orgB)
	e.must(err)
	if len(list) != 1 {
		t.Fatalf("ListManualApps(B) has %d apps, want 1", len(list))
	}
	checkApp(t, &list[0], m3)
	if list, err = e.st.ListManualApps(e.ctx, 999); err != nil || len(list) != 0 {
		t.Errorf("ListManualApps(unknown) = %v, %v; want none", list, err)
	}
}

func testDeleteApp(t *testing.T, e *env) {
	app := e.createApp()
	at := e.t0.Add(3 * time.Hour)

	g1 := e.grant(flow{app: app})
	g2 := e.grant(flow{app: app, org: e.orgB})
	g3 := e.grant(flow{app: app})
	// g3 was revoked before: DeleteApp neither returns it nor changes its reason.
	_, err := e.st.RevokeGrants(e.ctx, oauth.GrantFilter{ID: g3.grant.ID}, oauth.ReasonUser, e.t0.Add(2*time.Hour))
	e.must(err)

	got, err := e.st.DeleteApp(e.ctx, app.ID, at)
	e.must(err)
	sortGrants(got)
	if len(got) != 2 {
		t.Fatalf("DeleteApp revoked %d grants (%v), want 2", len(got), ids(got))
	}
	checkGrant(t, &got[0], revoked(g1.grant, oauth.ReasonAppDeleted, at))
	checkGrant(t, &got[1], revoked(g2.grant, oauth.ReasonAppDeleted, at))

	_, err = e.st.GetApp(e.ctx, app.ID)
	e.wantErr(err, oauth.ErrNotFound)
	if n, err := e.st.CountDynamicApps(e.ctx); err != nil || n != 0 {
		t.Errorf("CountDynamicApps after delete = %d, %v; want 0", n, err)
	}
	for _, g := range []granted{g1, g2} {
		stored, err := e.st.GetGrant(e.ctx, g.grant.ID)
		e.must(err)
		checkGrant(t, stored, revoked(g.grant, oauth.ReasonAppDeleted, at))
		_, err = e.st.LookupAccess(e.ctx, g.access.Hash, at)
		e.wantErr(err, oauth.ErrNotFound)
	}
	stored, err := e.st.GetGrant(e.ctx, g3.grant.ID)
	e.must(err)
	checkGrant(t, stored, revoked(g3.grant, oauth.ReasonUser, e.t0.Add(2*time.Hour)))

	// A listing still shows the grants with their app, the deleted one included.
	infos, err := e.st.ListGrants(e.ctx, oauth.GrantFilter{AppID: app.ID})
	e.must(err)
	if len(infos) != 3 {
		t.Fatalf("ListGrants has %d grants, want 3", len(infos))
	}
	for _, gi := range infos {
		eq(t, "listed app id", gi.App.ID, app.ID)
		eqTimePtr(t, "listed app DeletedAt", gi.App.DeletedAt, at)
	}

	_, err = e.st.DeleteApp(e.ctx, app.ID, at)
	e.wantErr(err, oauth.ErrNotFound)
	_, err = e.st.DeleteApp(e.ctx, uuid(), at)
	e.wantErr(err, oauth.ErrNotFound)
	_, err = e.st.DeleteApp(e.ctx, "not-a-uuid", at)
	e.wantErr(err, oauth.ErrNotFound)
	upd := app
	upd.Name = "after delete"
	e.wantErr(e.st.UpdateApp(e.ctx, upd), oauth.ErrNotFound)
}

func testSecrets(t *testing.T, e *env) {
	app, other := e.createApp(), e.createApp()
	s1, s2 := e.secret(app.ID), e.secret(app.ID)
	s1.CreatedAt, s2.CreatedAt = e.t0.Add(time.Second), e.t0.Add(2*time.Second)
	e.must(e.st.CreateSecret(e.ctx, s2))
	e.must(e.st.CreateSecret(e.ctx, s1))

	list, err := e.st.ListSecrets(e.ctx, app.ID)
	e.must(err)
	if len(list) != 2 {
		t.Fatalf("ListSecrets has %d secrets, want 2", len(list))
	}
	checkSecret(t, list[0], s1) // oldest first, whatever the insertion order
	checkSecret(t, list[1], s2)

	at := e.t0.Add(time.Hour)
	e.must(e.st.TouchSecret(e.ctx, s2.ID, at))
	e.must(e.st.TouchSecret(e.ctx, uuid(), at)) // an unknown id is not an error
	e.must(e.st.TouchSecret(e.ctx, "not-a-uuid", at))
	list, err = e.st.ListSecrets(e.ctx, app.ID)
	e.must(err)
	s2.LastUsedAt = tp(at)
	checkSecret(t, list[0], s1)
	checkSecret(t, list[1], s2)

	// A taken hash is ErrConflict and adds nothing; an unknown app is ErrNotFound.
	clash := e.secret(app.ID)
	clash.Hash = s1.Hash
	e.wantErr(e.st.CreateSecret(e.ctx, clash), oauth.ErrConflict)
	e.wantErr(e.st.CreateSecret(e.ctx, e.secret(uuid())), oauth.ErrNotFound)
	if list, _ = e.st.ListSecrets(e.ctx, app.ID); len(list) != 2 {
		t.Errorf("ListSecrets after the failed inserts has %d secrets, want 2", len(list))
	}

	// DeleteSecret removes a secret of that app only.
	e.wantErr(e.st.DeleteSecret(e.ctx, other.ID, s1.ID), oauth.ErrNotFound)
	e.wantErr(e.st.DeleteSecret(e.ctx, app.ID, uuid()), oauth.ErrNotFound)
	e.wantErr(e.st.DeleteSecret(e.ctx, app.ID, "not-a-uuid"), oauth.ErrNotFound)
	e.must(e.st.DeleteSecret(e.ctx, app.ID, s1.ID))
	e.wantErr(e.st.DeleteSecret(e.ctx, app.ID, s1.ID), oauth.ErrNotFound)
	list, err = e.st.ListSecrets(e.ctx, app.ID)
	e.must(err)
	if len(list) != 1 {
		t.Fatalf("ListSecrets has %d secrets, want 1", len(list))
	}
	checkSecret(t, list[0], s2)

	// No such app, or none that is a UUID: an empty list.
	for _, id := range []string{uuid(), "not-a-uuid"} {
		if list, err = e.st.ListSecrets(e.ctx, id); err != nil || len(list) != 0 {
			t.Errorf("ListSecrets(%q) = %v, %v; want none", id, list, err)
		}
	}
}
