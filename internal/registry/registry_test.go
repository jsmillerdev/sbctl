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

// TestPostgres runs them against a real database when SBCTL_TEST_DATABASE_URL points
// at an empty database (it creates schema sbctl).
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("SBCTL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SBCTL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate sbctl.projects, sbctl.organizations, sbctl.access_tokens, sbctl.backups, sbctl.events cascade`); err != nil {
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
	dsn := os.Getenv("SBCTL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SBCTL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, tempDatabase(t, dsn, "sbctl_mig"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "0900_members.sql"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`insert into sbctl.organizations (slug, name) values ('one', 'One'), ('two', 'Two')`,
		`insert into sbctl.api_users (user_id, email) values ('11111111-1111-4111-8111-111111111111', 'a@example.test'), ('22222222-2222-4222-8222-222222222222', 'b@example.test')`,
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
	if err := pool.QueryRow(ctx, `select count(*) from sbctl.org_members where role_id = 1`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `select count(*) from sbctl.member_legacy_checked`).Scan(&checked)
	_ = pool.QueryRow(ctx, `select at from sbctl.member_meta where key = 'legacy_cutoff'`).Scan(&cutoff)
	if owners != 4 || checked != 2 || cutoff.IsZero() {
		t.Fatalf("owners=%d (want 2 users x 2 orgs) checked=%d cutoff=%v", owners, checked, cutoff)
	}
	// Applying again changes nothing.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
