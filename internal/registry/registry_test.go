package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMemory runs the shared conformance checks against the in-memory registry.
func TestMemory(t *testing.T) { testRegistry(t, NewMemory()) }

// TestPostgres runs them against a real database when SUPAVISE_TEST_DATABASE_URL points
// at an empty database (it creates schema supavise).
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate supavise.projects, supavise.organizations, supavise.access_tokens, supavise.backups, supavise.events cascade`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, r.Pool()); err != nil { // idempotent
		t.Fatal(err)
	}
	testRegistry(t, r)
}

func testRegistry(t *testing.T, r Registry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	changes, err := r.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	org, err := r.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateOrganization(ctx, "acme", "Again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup org: %v", err)
	}
	sys := &Project{Ref: "system", Name: "system", Status: StatusActiveHealthy}
	if err := r.CreateProject(ctx, sys); err != nil || sys.Seq != 0 {
		t.Fatalf("system: %v seq=%d", err, sys.Seq)
	}
	a := &Project{Ref: "aaaaaaaaaaaaaaaaaaaa", OrgID: org.ID, Name: "a", Versions: map[string]string{"postgres": "pg17"}}
	b := &Project{Ref: "bbbbbbbbbbbbbbbbbbbb", OrgID: org.ID, Name: "b"}
	if err := r.CreateProject(ctx, a); err != nil || a.Seq != 1 || a.Status != StatusComingUp {
		t.Fatalf("a: %v %+v", err, a)
	}
	if err := r.CreateProject(ctx, b); err != nil || b.Seq != 2 {
		t.Fatalf("b: %v %+v", err, b)
	}
	if err := r.DeleteProject(ctx, a.Ref); err != nil {
		t.Fatal(err)
	}
	c := &Project{Ref: "cccccccccccccccccccc", OrgID: org.ID, Name: "c"}
	if err := r.CreateProject(ctx, c); err != nil || c.Seq != 1 {
		t.Fatalf("seq reuse: %v %+v", err, c)
	}
	if err := r.SetProjectStatus(ctx, c.Ref, StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	c.Status = StatusActiveHealthy // UpdateProject writes every mutable field
	c.Limits.MemoryMax = "2G"
	c.Versions = map[string]string{"auth": "auth-v2"}
	if err := r.UpdateProject(ctx, c); err != nil || c.Status != StatusActiveHealthy || c.Limits.MemoryMax != "2G" {
		t.Fatalf("update: %v %+v", err, c)
	}
	got, err := r.GetProject(ctx, c.Ref)
	if err != nil || got.Versions["auth"] != "auth-v2" || got.OrgID != org.ID {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := r.GetProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ps, _ := r.ListProjects(ctx)
	if len(ps) != 3 || ps[0].Ref != "system" {
		t.Fatalf("list: %+v", ps)
	}

	if err := r.PutSecret(ctx, c.Ref, "jwt_secret", []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutSecret(ctx, c.Ref, "jwt_secret", []byte{3}); err != nil {
		t.Fatal(err)
	}
	if s, err := r.GetSecret(ctx, c.Ref, "jwt_secret"); err != nil || len(s) != 1 || s[0] != 3 {
		t.Fatalf("secret: %v %v", s, err)
	}
	// Put-if-absent: the first writer wins and says so; the project must exist.
	if ok, err := PutSecretIfAbsent(ctx, r, c.Ref, "saml_private_key", []byte{7}); err != nil || !ok {
		t.Fatalf("first put-if-absent: %v %v", ok, err)
	}
	if ok, err := PutSecretIfAbsent(ctx, r, c.Ref, "saml_private_key", []byte{8}); err != nil || ok {
		t.Fatalf("second put-if-absent: %v %v", ok, err)
	}
	if s, err := r.GetSecret(ctx, c.Ref, "saml_private_key"); err != nil || len(s) != 1 || s[0] != 7 {
		t.Fatalf("secret after put-if-absent: %v %v", s, err)
	}
	if _, err := PutSecretIfAbsent(ctx, r, "zzzzzzzzzzzzzzzzzzzz", "x", []byte{1}); err == nil {
		t.Fatal("a secret for a project that does not exist")
	}
	if err := r.PutRoute(ctx, Route{Host: "x.example.com", Ref: c.Ref}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := r.ListRoutes(ctx); len(rs) != 1 || rs[0].Kind != "api" {
		t.Fatalf("routes: %+v", rs)
	}

	tok := &AccessToken{UserID: "11111111-1111-1111-1111-111111111111", Name: "cli", Hash: []byte("h"), Prefix: "sbp_1"}
	if err := r.CreateAccessToken(ctx, tok); err != nil || tok.ID == 0 {
		t.Fatal(err)
	}
	if got, err := r.GetAccessTokenByHash(ctx, []byte("h")); err != nil || got.ID != tok.ID {
		t.Fatal(err)
	}
	if err := r.TouchAccessToken(ctx, tok.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteAccessToken(ctx, "22222222-2222-2222-2222-222222222222", tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted another user's token")
	}

	bk := &Backup{Ref: c.Ref}
	if err := r.CreateBackup(ctx, bk); err != nil || bk.Status != BackupRunning {
		t.Fatal(err)
	}
	now := time.Now()
	bk.Status, bk.FinishedAt, bk.StopLSN = BackupCompleted, &now, "0/3000100"
	if err := r.UpdateBackup(ctx, bk); err != nil {
		t.Fatal(err)
	}
	if bs, _ := r.ListBackups(ctx, c.Ref); len(bs) != 1 || bs[0].Status != BackupCompleted || bs[0].StopLSN != "0/3000100" {
		t.Fatalf("backups: %+v", bs)
	}
	if err := r.AppendEvent(ctx, c.Ref, "created", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendEvent(ctx, "", "node", nil); err != nil {
		t.Fatal(err)
	}
	if es, _ := r.ListEvents(ctx, c.Ref, 10); len(es) != 1 || es[0].Kind != "created" {
		t.Fatalf("events: %+v", es)
	}
	if err := r.DeleteProject(ctx, c.Ref); err != nil {
		t.Fatal(err)
	}
	if rs, _ := r.ListRoutes(ctx); len(rs) != 0 {
		t.Fatal("route not cascaded")
	}
	if bs, _ := r.ListBackups(ctx, c.Ref); len(bs) != 1 {
		t.Fatal("backup must outlive its project")
	}

	// An organization with a project cannot be deleted; an empty one can, once.
	if err := r.DeleteOrganization(ctx, org.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete an organization with a project: %v, want ErrConflict", err)
	}
	empty, err := r.CreateOrganization(ctx, "empty", "Empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteOrganization(ctx, empty.ID); err != nil {
		t.Fatalf("delete an empty organization: %v", err)
	}
	if err := r.DeleteOrganization(ctx, empty.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete it again: %v, want ErrNotFound", err)
	}
	if _, err := r.GetOrganization(ctx, "empty"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}

	testBranches(t, r, org.ID)
	testUpgrades(t, r, org.ID)

	seen := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for !(seen["projects"] && seen["routes"] && seen["project_secrets"]) {
		select {
		case ch := <-changes:
			seen[ch.Table] = true
		case <-timeout:
			t.Fatalf("change feed incomplete: %v", seen)
		}
	}
}

func testBranches(t *testing.T, r Registry, orgID int64) {
	t.Helper()
	ctx := context.Background()
	const parent, kid, kid2 = "dddddddddddddddddddd", "eeeeeeeeeeeeeeeeeeee", "ffffffffffffffffffff"
	if err := r.CreateProject(ctx, &Project{Ref: parent, OrgID: orgID, Name: "parent"}); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	b := &Project{Ref: kid, OrgID: orgID, Name: "feature", Branch: &BranchInfo{
		ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent, Name: "feature", GitBranch: "feat/x", ExpiresAt: &exp,
		NotifyURL: "https://example.test/hook", State: BranchCreatingProject, Egress: EgressPending}}
	if err := r.CreateProject(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetProject(ctx, kid)
	if err != nil || got.Branch == nil || got.Branch.ParentRef != parent || got.Branch.GitBranch != "feat/x" ||
		got.Branch.State != BranchCreatingProject || got.Branch.Egress != EgressPending || got.Branch.ExpiresAt == nil || !got.Branch.ExpiresAt.Equal(exp) || got.Branch.Persistent {
		t.Fatalf("branch round trip: %v %+v %+v", err, got, got.Branch)
	}
	if p, _ := r.GetProject(ctx, parent); p.Branch != nil {
		t.Fatal("an ordinary project must not carry branch info")
	}
	// Names are unique per parent; ids are unique everywhere.
	dup := &Project{Ref: kid2, OrgID: orgID, Name: "x", Branch: &BranchInfo{ID: "11111111-8b86-4011-b42d-00c04fc964ff", ParentRef: parent, Name: "feature"}}
	if err := r.CreateProject(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate branch name: %v", err)
	}
	// A parent with branches cannot be deleted.
	if err := r.DeleteProject(ctx, parent); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete parent with branches: %v", err)
	}
	got.Branch.State, got.Branch.Detail, got.Branch.CloneMethod, got.Branch.Persistent, got.Branch.ExpiresAt = BranchMigrationsPassed, "ok", "clonefile", true, nil
	got.Branch.Egress = EgressDenied // UpdateBranch must not write this
	if err := r.SetProjectStatus(ctx, kid, StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateBranch(ctx, kid, got.Branch); err != nil {
		t.Fatal(err)
	}
	again, _ := r.GetProject(ctx, kid)
	if again.Status != StatusActiveHealthy || again.Branch.State != BranchMigrationsPassed || !again.Branch.Persistent || again.Branch.ExpiresAt != nil || again.Branch.CloneMethod != "clonefile" || again.Branch.ParentRef != parent {
		t.Fatalf("update branch: %+v %+v", again, again.Branch)
	}
	// The egress policy has its own writer: UpdateBranch leaves it alone, so a read-modify-write
	// of the other fields cannot put a stale policy back.
	if again.Branch.Egress != EgressPending {
		t.Fatalf("UpdateBranch wrote the egress policy: %q", again.Branch.Egress)
	}
	if err := r.SetBranchEgress(ctx, kid, EgressDenied, EgressAllowed); !errors.Is(err, ErrConflict) {
		t.Fatalf("SetBranchEgress from the wrong policy: %v", err)
	}
	if err := r.SetBranchEgress(ctx, kid, EgressPending, EgressDenied); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBranchEgress(ctx, kid, EgressPending, EgressDenied); !errors.Is(err, ErrConflict) {
		t.Fatalf("SetBranchEgress twice from pending: %v", err)
	}
	stale := *again.Branch // read before the policy changed, written back after
	stale.Detail = "stale write"
	if err := r.UpdateBranch(ctx, kid, &stale); err != nil {
		t.Fatal(err)
	}
	if later, _ := r.GetProject(ctx, kid); later.Branch.Egress != EgressDenied || later.Branch.Detail != "stale write" {
		t.Fatalf("a stale UpdateBranch changed the policy or lost its own field: %+v", later.Branch)
	}
	if err := r.SetBranchEgress(ctx, parent, "", EgressDenied); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBranchEgress on an ordinary project: %v", err)
	}
	if err := r.SetBranchEgress(ctx, "zzzzzzzzzzzzzzzzzzzz", "", EgressDenied); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBranchEgress on a missing project: %v", err)
	}
	if err := r.UpdateBranch(ctx, parent, got.Branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateBranch on an ordinary project: %v", err)
	}
	if err := r.DeleteProject(ctx, kid); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteProject(ctx, parent); err != nil {
		t.Fatal(err)
	}
}

// tempDatabase creates an empty database next to the one dsn names and returns a DSN for it.
func tempDatabase(t *testing.T, dsn, prefix string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `create database `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			_, _ = a.Exec(context.Background(), `drop database if exists `+name+` with (force)`)
			a.Close()
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestMembersMigrationKeepsExistingUsersOwners applies every migration before 0900, adds an
// organization and dashboard users the way a node from before roles has them, then applies
// 0900: every known user must be Owner of every organization and be marked as checked.
func TestMembersMigrationKeepsExistingUsersOwners(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, tempDatabase(t, dsn, "supavise_mig"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "0900_members.sql"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`insert into supavise.organizations (slug, name) values ('one', 'One'), ('two', 'Two')`,
		`insert into supavise.api_users (user_id, email) values ('11111111-1111-4111-8111-111111111111', 'a@example.test'), ('22222222-2222-4222-8222-222222222222', 'b@example.test')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var owners, checked int
	var cutoff time.Time
	if err := pool.QueryRow(ctx, `select count(*) from supavise.org_members where role_id = 1`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `select count(*) from supavise.member_legacy_checked`).Scan(&checked)
	_ = pool.QueryRow(ctx, `select at from supavise.member_meta where key = 'legacy_cutoff'`).Scan(&cutoff)
	if owners != 4 || checked != 2 || cutoff.IsZero() {
		t.Fatalf("owners=%d (want 2 users x 2 orgs) checked=%d cutoff=%v", owners, checked, cutoff)
	}
	// Applying again changes nothing.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// HasDashboardSSO follows the rows of supavise.sso_providers.
func TestPostgresHasDashboardSSO(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, tempDatabase(t, dsn, "supavise_sso"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if ok, err := r.HasDashboardSSO(ctx); err != nil || ok {
		t.Fatalf("empty: %v %v", ok, err)
	}
	org, err := r.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pool().Exec(ctx, `insert into supavise.sso_providers (id, org_id, entity_id) values ('a0000000-0000-4000-8000-000000000001', $1, 'e')`, org.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.HasDashboardSSO(ctx); err != nil || !ok {
		t.Fatalf("with a provider: %v %v", ok, err)
	}
	// The organization takes its providers and their users with it.
	if _, err := r.Pool().Exec(ctx, `delete from supavise.organizations where id = $1`, org.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.HasDashboardSSO(ctx); ok {
		t.Fatal("a provider outlived its organization")
	}
}

func testUpgrades(t *testing.T, r Registry, orgID int64) {
	t.Helper()
	ctx := context.Background()
	const ref = "dddddddddddddddddddd"
	if err := r.CreateProject(ctx, &Project{Ref: ref, OrgID: orgID, Name: "d"}); err != nil {
		t.Fatal(err)
	}
	st := Upgrades(r)
	if st == nil {
		t.Fatal("registry has no UpgradeStore")
	}
	if _, err := st.LatestUpgrade(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("latest before any upgrade: %v, want ErrNotFound", err)
	}
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	first := &Upgrade{TrackingID: "11111111-1111-4111-8111-111111111111", Ref: ref, From: map[string]string{"auth": "a1"}, To: map[string]string{"auth": "a2"},
		TargetVersion: "17", Progress: "0_requested", InitiatedAt: t0, LatestStatusAt: t0}
	if err := st.PutUpgrade(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.Status, first.Progress, first.BackupID, first.LatestStatusAt = UpgradeDone, "9_completed_upgrade", 7, t0.Add(time.Minute)
	if err := st.PutUpgrade(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &Upgrade{TrackingID: "22222222-2222-4222-8222-222222222222", Ref: ref, TargetVersion: "17", Status: UpgradeFailed, Progress: "5_initiated_data_upgrade",
		Error: "5_data_upgrade_completion_failed", Detail: "gotrue did not start", InitiatedAt: t0.Add(time.Hour), LatestStatusAt: t0.Add(time.Hour)}
	if err := st.PutUpgrade(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, err := st.LatestUpgrade(ctx, ref)
	if err != nil || got.TrackingID != second.TrackingID || got.Status != UpgradeFailed || got.Error != second.Error || got.Detail != "gotrue did not start" {
		t.Fatalf("latest = %+v, %v", got, err)
	}
	if len(got.From) != 0 || len(got.To) != 0 {
		t.Fatalf("versions of an upgrade that set none: %v %v", got.From, got.To)
	}
	if err := st.PutUpgrade(ctx, &Upgrade{TrackingID: "33333333-3333-4333-8333-333333333333", Ref: "nope", InitiatedAt: t0, LatestStatusAt: t0}); err == nil {
		t.Fatal("an upgrade of an unknown project was stored")
	}
	// A project's upgrades go with it.
	if err := r.DeleteProject(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestUpgrade(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upgrades survived their project: %v", err)
	}
}

// TestComputeSizesMigrationRenamesClasses applies every migration before 1250, adds projects the
// way a node from before compute sizes has them, then applies 1250: the old classes become the
// sizes they were closest to, default limits follow the size, hand-set limits stay.
func TestComputeSizesMigrationRenamesClasses(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, tempDatabase(t, dsn, "supavise_sizes"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "1250_compute_sizes.sql"); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ ref, class, limits string }{
		{"aaaaaaaaaaaaaaaaaaaa", "micro", `{"memory_max":"1G","cpu_quota":"100%"}`},
		{"bbbbbbbbbbbbbbbbbbbb", "default", `{"memory_max":"1G","cpu_quota":"100%"}`},
		{"cccccccccccccccccccc", "small", `{}`},
		{"dddddddddddddddddddd", "medium", `{"memory_max":"1G","cpu_quota":"100%"}`},
		{"eeeeeeeeeeeeeeeeeeee", "large", `{"memory_max":"6G","cpu_quota":"300%"}`}, // set by hand
		{"system", "system", `{"memory_max":"1G","cpu_quota":"100%"}`},
	}
	for i, r := range rows {
		if _, err := pool.Exec(ctx, `insert into supavise.projects (ref, seq, name, region, engine, class, status, versions, limits)
			values ($1, $2, $1, 'us-east-1', 'postgres', $3, 'ACTIVE_HEALTHY', '{}', $4::jsonb)`, r.ref, i+1, r.class, r.limits); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string][3]string{
		"aaaaaaaaaaaaaaaaaaaa": {"nano", "512M", "100%"},
		"bbbbbbbbbbbbbbbbbbbb": {"micro", "1G", "100%"},
		"cccccccccccccccccccc": {"small", "2G", "100%"},
		"dddddddddddddddddddd": {"medium", "4G", "200%"},
		"eeeeeeeeeeeeeeeeeeee": {"large", "6G", "300%"},
		"system":               {"system", "1G", "100%"},
	} {
		var class, mem, cpu string
		if err := pool.QueryRow(ctx, `select class, limits->>'memory_max', limits->>'cpu_quota' from supavise.projects where ref = $1`, ref).Scan(&class, &mem, &cpu); err != nil {
			t.Fatal(err)
		}
		if got := [3]string{class, mem, cpu}; got != want {
			t.Errorf("%s: %v, want %v", ref, got, want)
		}
	}
	// A project created without a class is a Micro, and applying again changes nothing.
	if _, err := pool.Exec(ctx, `insert into supavise.projects (ref, seq, name, region, engine, status, versions, limits)
		values ('ffffffffffffffffffff', 9, 'f', 'us-east-1', 'postgres', 'ACTIVE_HEALTHY', '{}', '{}')`); err != nil {
		t.Fatal(err)
	}
	var class string
	if err := pool.QueryRow(ctx, `select class from supavise.projects where ref = 'ffffffffffffffffffff'`).Scan(&class); err != nil || class != "micro" {
		t.Fatalf("default class = %q, %v", class, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
