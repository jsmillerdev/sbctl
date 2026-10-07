package members

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmillerdev/supavise/internal/registry"
)

// newPGEnv returns an env over a throwaway database next to the one SUPAVISE_TEST_DATABASE_URL
// names (CI provides one): other packages' tests truncate the registry tables of the shared
// database, so this one gets its own. The registry migrations run as in production.
func newPGEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("supavise_members_%d_%d", os.Getpid(), time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `create database `+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `drop database if exists `+name+` with (force)`)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	reg, err := registry.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	mkOrg := func(slug string) OrgRef {
		o, err := reg.CreateOrganization(ctx, slug, slug)
		if err != nil {
			t.Fatal(err)
		}
		return OrgRef{ID: o.ID, Slug: o.Slug}
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	e := &env{a: mkOrg("acme"), b: mkOrg("beta"), now: &now}
	for i := 0; i < 3; i++ {
		ref := ""
		for len(ref) < 20 {
			ref += string(rune('a' + rand.Intn(26)))
		}
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: e.a.ID, Name: ref, Status: registry.StatusActiveHealthy, Engine: registry.EnginePostgres}); err != nil {
			t.Fatal(err)
		}
		e.refs = append(e.refs, ref)
	}
	e.newUser = func() string {
		var id string
		if err := reg.Pool().QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.svc = &Service{Store: NewPG(reg.Pool()), Now: func() time.Time { return *e.now },
		Orgs: func(context.Context) ([]OrgRef, error) { return []OrgRef{e.a, e.b}, nil }}
	e.setCutoff = func(c time.Time) {
		if _, err := reg.Pool().Exec(ctx, `update supavise.member_meta set at = $1 where key = 'legacy_cutoff'`, c); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// TestServicePG runs the service tests, including the concurrent last-owner race, against
// Postgres.
func TestServicePG(t *testing.T) { testService(t, newPGEnv) }

// TestProjectDeletionDropsScopedRoles: the registry's foreign key removes a deleted project
// from every project-scoped role, and a role left without projects disappears from listings.
func TestProjectDeletionDropsScopedRoles(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	u := e.member(t, e.a, 0)
	if err := e.svc.AssignProjectRole(ctx, nil, e.a, u, RoleDeveloper, []string{e.refs[0], e.refs[1]}); err != nil {
		t.Fatal(err)
	}
	pg := e.svc.Store.(*PG)
	if _, err := pg.pool.Exec(ctx, `delete from supavise.projects where ref = $1`, e.refs[0]); err != nil {
		t.Fatal(err)
	}
	rs, _ := e.svc.Store.ProjectRolesOf(ctx, u)
	if len(rs) != 1 || len(rs[0].Refs) != 1 || rs[0].Refs[0] != e.refs[1] {
		t.Fatalf("roles after deleting a project: %+v", rs)
	}
	if _, err := pg.pool.Exec(ctx, `delete from supavise.projects where ref = $1`, e.refs[1]); err != nil {
		t.Fatal(err)
	}
	if rs, _ := e.svc.Store.ProjectRolesOf(ctx, u); len(rs) != 0 {
		t.Fatalf("a role without projects must not be listed: %+v", rs)
	}
	// A project that does not exist is refused, not silently ignored.
	if err := e.svc.AssignProjectRole(ctx, nil, e.a, u, RoleDeveloper, []string{"zzzzzzzzzzzzzzzzzzzz"}); err == nil {
		t.Fatal("unknown project accepted")
	}
}
