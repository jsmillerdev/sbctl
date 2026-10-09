package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/oauth/storetest"
	"github.com/supavise/supavise/internal/registry"
)

// These tests run when SUPAVISE_TEST_DATABASE_URL names a Postgres server (CI provides one) and skip
// otherwise. Each test works in a database of its own, created next to the one the URL names and
// dropped at the end: other packages' tests truncate the tables of the shared database, and the
// registry migrations (including 1350_oauth.sql) run in it as in production.

type pgDB struct {
	reg  *registry.Postgres
	pool *pgxpool.Pool
	dsn  string
}

func newPGDB(t *testing.T) *pgDB {
	t.Helper()
	base := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("supavise_oauth_%d_%x", os.Getpid(), suffix)
	if _, err := admin.Exec(ctx, `create database `+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `drop database if exists `+name+` with (force)`)
		admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	reg, err := registry.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return &pgDB{reg: reg, pool: reg.Pool(), dsn: u.String()}
}

// pgTestStore is the store under test: a PGStore that can also create organizations, which is what
// storetest.OrgSeeder asks for.
type pgTestStore struct {
	*oauth.PGStore
	reg *registry.Postgres
}

func (s pgTestStore) SeedOrg(ctx context.Context, slug string) (int64, error) {
	o, err := s.reg.CreateOrganization(ctx, slug, slug)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

// newStore returns a store over empty OAuth tables and no organizations.
func (db *pgDB) newStore(t *testing.T) pgTestStore {
	t.Helper()
	_, err := db.pool.Exec(context.Background(), `truncate supavise.oauth_authorizations, supavise.oauth_tokens, supavise.oauth_grants,
		supavise.oauth_app_secrets, supavise.oauth_apps, supavise.organizations restart identity cascade`)
	if err != nil {
		t.Fatal(err)
	}
	return pgTestStore{PGStore: oauth.NewPGStore(db.pool), reg: db.reg}
}

// TestPGStoreContract runs the contract of storetest, which includes C2 (CodeSingleUseConcurrent:
// N redemptions of one code, one winner) and the concurrent refresh rotation of T2. Run it with
// -race.
func TestPGStoreContract(t *testing.T) {
	db := newPGDB(t)
	storetest.Run(t, func(t *testing.T) oauth.Store { return db.newStore(t) })
}

// TestPGMigration1350 is O3 with TestMigrationsFrom1300AreAdditive (internal/registry): the migration
// applies on a fresh database, is recorded once, and a second run changes nothing.
func TestPGMigration1350(t *testing.T) {
	db := newPGDB(t) // registry.Open has migrated a fresh database
	ctx := context.Background()
	const name = "1350_oauth.sql"

	applied := func() []string {
		t.Helper()
		got, err := registry.AppliedMigrations(ctx, db.dsn)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	count := func(want string) int {
		n := 0
		for _, a := range applied() {
			if a == want {
				n++
			}
		}
		return n
	}
	if !slices.Contains(registry.MigrationNames(), name) {
		t.Fatalf("%s is not embedded", name)
	}
	if n := count(name); n != 1 {
		t.Fatalf("%s is recorded %d times after a fresh migrate, want 1", name, n)
	}
	before := applied()
	if err := registry.Migrate(ctx, db.pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if after := applied(); !slices.Equal(before, after) {
		t.Errorf("a second Migrate changed the applied migrations:\n before %v\n after  %v", before, after)
	}

	var tables []string
	rows, err := db.pool.Query(ctx, `select table_name from information_schema.tables where table_schema = 'supavise' and table_name like 'oauth\_%' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	want := []string{"oauth_app_secrets", "oauth_apps", "oauth_authorizations", "oauth_grants", "oauth_tokens"}
	if !slices.Equal(tables, want) {
		t.Errorf("tables = %v, want %v", tables, want)
	}
	// Nothing of OAuth lives in access_tokens (U6): no column of it was added by this migration.
	var cols int
	if err := db.pool.QueryRow(ctx, `select count(*) from information_schema.columns where table_schema = 'supavise' and table_name = 'access_tokens' and column_name like '%oauth%'`).Scan(&cols); err != nil || cols != 0 {
		t.Errorf("access_tokens has %d oauth columns (%v), want none", cols, err)
	}
}

// TestPGSchemaConstraints: the database refuses what the plan says it refuses, whatever the caller does.
func TestPGSchemaConstraints(t *testing.T) {
	db := newPGDB(t)
	ctx := context.Background()
	st := db.newStore(t)
	org, err := st.SeedOrg(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	appSQL := func(regType, org, name, uris, scopes, method string) string {
		return fmt.Sprintf(`insert into supavise.oauth_apps (registration_type, org_id, name, redirect_uris, scopes, token_endpoint_auth_method)
			values ('%s', %s, %s, %s, %s, '%s')`, regType, org, name, uris, scopes, method)
	}
	one, sc := `'{https://a.example/cb}'`, `'{projects:read}'`
	valid := appSQL("dynamic", "null", "'ok'", one, sc, "none")
	if _, err := db.pool.Exec(ctx, valid); err != nil {
		t.Fatalf("the valid app was refused: %v", err)
	}
	o := fmt.Sprint(org)
	for name, sql := range map[string]string{
		"a manual app without an organization": appSQL("manual", "null", "'x'", one, sc, "none"),
		"a dynamic app with an organization":   appSQL("dynamic", o, "'x'", one, sc, "none"),
		"an unknown registration type":         appSQL("other", "null", "'x'", one, sc, "none"),
		"an empty name":                        appSQL("dynamic", "null", "''", one, sc, "none"),
		"a name of 101 characters":             appSQL("dynamic", "null", "repeat('x', 101)", one, sc, "none"),
		"no redirect URI":                      appSQL("dynamic", "null", "'x'", "'{}'", sc, "none"),
		"eleven redirect URIs":                 appSQL("dynamic", "null", "'x'", "array_fill('https://a.example/cb'::text, array[11])", sc, "none"),
		"no scopes":                            appSQL("dynamic", "null", "'x'", one, "'{}'", "none"),
		"an unknown auth method":               appSQL("dynamic", "null", "'x'", one, sc, "private_key_jwt"),
	} {
		pgExpectCode(t, db, "23514", name, sql)
	}
	var appID string
	if err := db.pool.QueryRow(ctx, `select id::text from supavise.oauth_apps limit 1`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	var grantID int64
	if err := db.pool.QueryRow(ctx, `insert into supavise.oauth_grants (app_id, user_id, org_id, scopes) values ($1::uuid, gen_random_uuid(), $2, '{projects:read}') returning id`, appID, org).Scan(&grantID); err != nil {
		t.Fatalf("the valid grant was refused: %v", err)
	}
	pgExpectCode(t, db, "23514", "an unknown revocation reason",
		fmt.Sprintf(`update supavise.oauth_grants set revoked_at = now(), revoked_reason = 'bogus' where id = %d`, grantID))
	pgExpectCode(t, db, "23514", "an unknown token kind",
		fmt.Sprintf(`insert into supavise.oauth_tokens (grant_id, kind, token_hash, prefix, expires_at) values (%d, 'bogus', '\x01', 'p', now())`, grantID))
	pgExpectCode(t, db, "23514", "an unknown authorization status",
		fmt.Sprintf(`insert into supavise.oauth_authorizations (id, app_id, redirect_uri, scopes, expires_at, status) values (gen_random_uuid(), '%s', 'x', '{}', now(), 'bogus')`, appID))
	pgExpectCode(t, db, "23503", "a grant for an organization that does not exist",
		fmt.Sprintf(`insert into supavise.oauth_grants (app_id, user_id, org_id, scopes) values ('%s', gen_random_uuid(), 99999, '{}')`, appID))
}

func pgExpectCode(t *testing.T, db *pgDB, code, what, sql string) {
	t.Helper()
	_, err := db.pool.Exec(context.Background(), sql)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != code {
		t.Errorf("%s: error = %v, want SQLSTATE %s", what, err, code)
	}
}

// TestPGForeignKeysAreIndexed: every foreign key of the OAuth tables has an index that starts with its
// column, so deleting an organization, an app or a grant does not scan the tables that refer to it.
func TestPGForeignKeysAreIndexed(t *testing.T) {
	db := newPGDB(t)
	rows, err := db.pool.Query(context.Background(), `
		select r.relname, a.attname
		  from pg_constraint c
		  join pg_class r on r.oid = c.conrelid
		  join pg_namespace n on n.oid = r.relnamespace
		  join pg_attribute a on a.attrelid = c.conrelid and a.attnum = c.conkey[1]
		 where c.contype = 'f' and n.nspname = 'supavise' and r.relname like 'oauth\_%'
		   and not exists (select 1 from pg_index i where i.indrelid = c.conrelid and i.indkey[0] = c.conkey[1])`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s.%s is a foreign key without an index", table, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestPGLookupAccessUsesIndexes: the query of every OAuth request finds the token by its unique index
// and the grant, app and organization by primary key, without scanning a table. The planner needs
// statistics to show that, so the test loads 6,000 tokens of 3,000 grants first and analyzes them.
// The plan it printed when this was written (Postgres 17):
//
//	Nested Loop
//	  ->  Nested Loop
//	        ->  Nested Loop
//	              ->  Index Scan using oauth_tokens_token_hash_key on oauth_tokens t
//	                    Index Cond: (token_hash = <hash>)
//	                    Filter: ((expires_at > <now>) AND (kind = 'access'::text))
//	              ->  Index Scan using oauth_grants_pkey on oauth_grants g
//	                    Index Cond: (id = t.grant_id)
//	                    Filter: (revoked_at IS NULL)
//	        ->  Index Scan using oauth_apps_pkey on oauth_apps a
//	              Index Cond: (id = g.app_id)
//	              Filter: (deleted_at IS NULL)
//	  ->  Index Scan using organizations_pkey on organizations o
//	        Index Cond: (id = g.org_id)
func TestPGLookupAccessUsesIndexes(t *testing.T) {
	db := newPGDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`insert into supavise.organizations (slug, name) select 'org' || i, 'org' || i from generate_series(1, 50) i`,
		`insert into supavise.oauth_apps (registration_type, name, redirect_uris, scopes)
		 select 'dynamic', 'app' || i, '{https://a.example/cb}', '{projects:read}' from generate_series(1, 100) i`,
		`with a as (select id, row_number() over () rn from supavise.oauth_apps),
		      o as (select id, row_number() over () rn from supavise.organizations)
		 insert into supavise.oauth_grants (app_id, user_id, org_id, scopes)
		 select a.id, gen_random_uuid(), o.id, '{projects:read}'
		   from generate_series(1, 3000) i join a on a.rn = 1 + i % 100 join o on o.rn = 1 + i % 50`,
		`insert into supavise.oauth_tokens (grant_id, kind, token_hash, prefix, expires_at)
		 select g.id, case when k = 1 then 'access' else 'refresh' end,
		        decode(md5(g.id::text || k::text) || md5(k::text || g.id::text), 'hex'), 'sbp_oaut', now() + interval '1 hour'
		   from supavise.oauth_grants g, generate_series(1, 2) k`,
		`analyze supavise.organizations, supavise.oauth_apps, supavise.oauth_grants, supavise.oauth_tokens`,
	} {
		if _, err := db.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.pool.Query(ctx, `explain (costs off) `+oauth.PGLookupAccessSQL, make([]byte, sha256.Size), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	t.Log("plan:\n" + text)
	for _, want := range []string{"oauth_tokens_token_hash_key", "oauth_grants_pkey", "oauth_apps_pkey", "organizations_pkey"} {
		if !strings.Contains(text, want) {
			t.Errorf("the plan does not use %s", want)
		}
	}
	if strings.Contains(text, "Seq Scan") {
		t.Error("the plan scans a table")
	}
}

// pgFixture builds the rows the PG-only tests need through the store.
type pgFixture struct {
	t   *testing.T
	st  pgTestStore
	ctx context.Context
	org int64
	t0  time.Time
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	db := newPGDB(t)
	f := &pgFixture{t: t, st: db.newStore(t), ctx: context.Background(), t0: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	org, err := f.st.SeedOrg(f.ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	f.org = org
	return f
}

func (f *pgFixture) uuid() string {
	f.t.Helper()
	var id string
	if err := f.st.reg.Pool().QueryRow(f.ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func pgHash() []byte {
	var b [16]byte
	_, _ = rand.Read(b[:])
	sum := sha256.Sum256(b[:])
	return sum[:]
}

func (f *pgFixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *pgFixture) app() oauth.App {
	f.t.Helper()
	a := oauth.App{
		ID: f.uuid(), RegistrationType: oauth.RegistrationDynamic, Name: "client", RedirectURIs: []string{"http://127.0.0.1/cb"},
		Scopes: []string{"projects:read", "database:read"}, TokenEndpointAuthMethod: oauth.AuthMethodNone, CreatedAt: f.t0, UpdatedAt: f.t0,
	}
	f.must(f.st.CreateApp(f.ctx, a, nil))
	return a
}

// approved stores an approved authorization of the app and returns its code hash.
func (f *pgFixture) approved(app oauth.App, user string, org int64) []byte {
	f.t.Helper()
	a := oauth.Authorization{ID: f.uuid(), AppID: app.ID, RedirectURI: "http://127.0.0.1/cb", Scopes: app.Scopes, CreatedAt: f.t0, ExpiresAt: f.t0.Add(time.Hour)}
	f.must(f.st.CreateAuthorization(f.ctx, a))
	code := pgHash()
	_, err := f.st.DecideAuthorization(f.ctx, a.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: user, At: f.t0.Add(time.Minute), OrgID: org, CodeHash: code, CodeExpiresAt: f.t0.Add(2 * time.Minute),
	})
	f.must(err)
	return code
}

func (f *pgFixture) newGrant(app oauth.App, user string, access, refresh []byte) oauth.NewGrant {
	at := f.t0.Add(2 * time.Minute)
	return oauth.NewGrant{
		Grant:   oauth.Grant{AppID: app.ID, UserID: user, OrgID: f.org, Scopes: app.Scopes, CreatedAt: at},
		Access:  oauth.Token{Hash: access, Prefix: "sbp_oaut", CreatedAt: at, ExpiresAt: at.Add(time.Hour)},
		Refresh: oauth.Token{Hash: refresh, Prefix: "sbr_abcd", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)},
		At:      at,
	}
}

// grant redeems a new code and returns the hashes of the two tokens.
func (f *pgFixture) grant(app oauth.App, user string) (grantID int64, access, refresh []byte) {
	f.t.Helper()
	code := f.approved(app, user, f.org)
	access, refresh = pgHash(), pgHash()
	f.must(f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		res, err := tx.Complete(ctx, f.newGrant(app, user, access, refresh))
		if err == nil {
			grantID = res.Grant.ID
		}
		return err
	}))
	return grantID, access, refresh
}

// TestPGOrganizationDeleteCascades: deleting an organization removes its manual apps, their secrets,
// the grants in it and the authorizations decided for it, and leaves the dynamic app (section 2.13).
func TestPGOrganizationDeleteCascades(t *testing.T) {
	f := newPGFixture(t)
	other, err := f.st.SeedOrg(f.ctx, "beta")
	f.must(err)
	user := f.uuid()

	manual := oauth.App{
		ID: f.uuid(), RegistrationType: oauth.RegistrationManual, OrgID: other, Name: "published", RedirectURIs: []string{"https://a.example/cb"},
		Scopes: []string{"projects:read"}, TokenEndpointAuthMethod: oauth.AuthMethodBasic, CreatedBy: user, CreatedAt: f.t0, UpdatedAt: f.t0,
	}
	f.must(f.st.CreateApp(f.ctx, manual, &oauth.AppSecret{ID: f.uuid(), Alias: "sba_1a2b********", Hash: pgHash(), CreatedAt: f.t0}))
	dynamic := f.app()
	code := f.approved(dynamic, user, other)
	f.must(f.st.WithCode(f.ctx, dynamic.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		ng := f.newGrant(dynamic, user, pgHash(), pgHash())
		ng.Grant.OrgID = other
		_, err := tx.Complete(ctx, ng)
		return err
	}))
	keptGrant, _, _ := f.grant(dynamic, f.uuid()) // in the organization that stays

	if _, err := f.st.reg.Pool().Exec(f.ctx, `delete from supavise.organizations where id = $1`, other); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"oauth_apps": 1, "oauth_app_secrets": 0, "oauth_grants": 1, "oauth_tokens": 2} {
		var n int
		if err := f.st.reg.Pool().QueryRow(f.ctx, `select count(*) from supavise.`+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s has %d rows after the organization was deleted, want %d", table, n, want)
		}
	}
	if _, err := f.st.GetGrant(f.ctx, keptGrant); err != nil {
		t.Errorf("the grant in the other organization: %v", err)
	}
	var withOrg int
	if err := f.st.reg.Pool().QueryRow(f.ctx, `select count(*) from supavise.oauth_authorizations where org_id = $1`, other).Scan(&withOrg); err != nil || withOrg != 0 {
		t.Errorf("authorizations decided for the deleted organization: %d, %v", withOrg, err)
	}
}

// TestPGRefusesPlaintext: the store takes hashes. A secret handed over as it is, or a token
// stored whole in the prefix column, is ErrInvalid and writes nothing.
func TestPGRefusesPlaintext(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	user := f.uuid()
	plain := []byte("sbc_" + strings.Repeat("ab", 32)) // the shape of an authorization code, not its digest

	a := oauth.Authorization{ID: f.uuid(), AppID: app.ID, RedirectURI: "http://127.0.0.1/cb", Scopes: app.Scopes, CreatedAt: f.t0, ExpiresAt: f.t0.Add(time.Hour)}
	f.must(f.st.CreateAuthorization(f.ctx, a))
	_, err := f.st.DecideAuthorization(f.ctx, a.ID, oauth.Decision{
		Status: oauth.StatusApproved, DecidedBy: user, At: f.t0.Add(time.Minute), OrgID: f.org, CodeHash: plain, CodeExpiresAt: f.t0.Add(2 * time.Minute),
	})
	if !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("approval with a code that is not a digest: %v, want ErrInvalid", err)
	}
	if got, err := f.st.GetAuthorization(f.ctx, a.ID); err != nil || got.Status != oauth.StatusPending || got.CodeHash != nil {
		t.Errorf("the refused approval changed the row: %+v, %v", got, err)
	}

	secret := oauth.AppSecret{ID: f.uuid(), AppID: app.ID, Alias: "sba_1a2b********", Hash: []byte("sba_" + strings.Repeat("cd", 32)), CreatedAt: f.t0}
	if err := f.st.CreateSecret(f.ctx, secret); !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("CreateSecret with a secret that is not a digest: %v, want ErrInvalid", err)
	}
	if err := f.st.CreateApp(f.ctx, oauth.App{ID: f.uuid(), RegistrationType: oauth.RegistrationDynamic, Name: "n", RedirectURIs: []string{"https://a.example/cb"},
		Scopes: []string{"projects:read"}, TokenEndpointAuthMethod: oauth.AuthMethodNone, CreatedAt: f.t0, UpdatedAt: f.t0}, &secret); !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("CreateApp with a secret that is not a digest: %v, want ErrInvalid", err)
	}
	if list, err := f.st.ListSecrets(f.ctx, app.ID); err != nil || len(list) != 0 {
		t.Errorf("secrets after the refusals: %v, %v", list, err)
	}

	code := f.approved(app, user, f.org)
	for name, mutate := range map[string]func(*oauth.NewGrant){
		"an access token that is not a digest": func(ng *oauth.NewGrant) { ng.Access.Hash = []byte("sbp_oauth_" + strings.Repeat("ef", 20)) },
		"a refresh token that is not a digest": func(ng *oauth.NewGrant) { ng.Refresh.Hash = plain },
		"a token stored whole as its prefix":   func(ng *oauth.NewGrant) { ng.Access.Prefix = "sbp_oauth_" + strings.Repeat("ef", 20) },
	} {
		ng := f.newGrant(app, user, pgHash(), pgHash())
		mutate(&ng)
		err := f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
			_, err := tx.Complete(ctx, ng)
			return err
		})
		if !errors.Is(err, oauth.ErrInvalid) {
			t.Errorf("Complete with %s: %v, want ErrInvalid", name, err)
		}
	}
	if infos, err := f.st.ListGrants(f.ctx, oauth.GrantFilter{AppID: app.ID}); err != nil || len(infos) != 0 {
		t.Errorf("grants after the refusals: %d, %v", len(infos), err)
	}
}

// TestPGDuplicateTokenHashRollsBackTheRedemption: a token hash that exists makes Complete fail with
// ErrConflict and, because fn returns the error, leaves the code approved and no second grant.
func TestPGDuplicateTokenHashRollsBackTheRedemption(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	_, access, _ := f.grant(app, f.uuid())

	user := f.uuid()
	code := f.approved(app, user, f.org)
	err := f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		_, err := tx.Complete(ctx, f.newGrant(app, user, access, pgHash()))
		return err
	})
	if !errors.Is(err, oauth.ErrConflict) {
		t.Fatalf("Complete with a taken access token hash: %v, want ErrConflict", err)
	}
	infos, err := f.st.ListGrants(f.ctx, oauth.GrantFilter{AppID: app.ID})
	f.must(err)
	if len(infos) != 1 {
		t.Errorf("the app has %d grants, want only the first", len(infos))
	}
	f.must(f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
		if s := tx.Authorization().Status; s != oauth.StatusApproved {
			t.Errorf("the code is %q after the failed redemption, want approved", s)
		}
		return nil
	}))
}

func TestPGNarrowScopesCannotWiden(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	_, _, refresh := f.grant(app, f.uuid())
	now := f.t0.Add(time.Hour)
	err := f.st.RotateRefresh(f.ctx, oauth.RotateInput{AppID: app.ID, TokenHash: refresh, Now: now, Grace: oauth.RefreshGrace},
		func(ctx context.Context, tx oauth.RotateTx) error {
			return tx.NarrowScopes(ctx, []string{"projects:read", "projects:write"})
		})
	if !errors.Is(err, oauth.ErrInvalid) {
		t.Fatalf("NarrowScopes to a scope the grant lacks: %v, want ErrInvalid", err)
	}
	tok, err := f.st.GetToken(f.ctx, refresh)
	f.must(err)
	if tok.UsedAt != nil {
		t.Error("the refused rotation left the token stamped")
	}
}

func TestPGRevokeRefusesUnknownReason(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	id, _, _ := f.grant(app, f.uuid())
	if _, err := f.st.RevokeGrants(f.ctx, oauth.GrantFilter{ID: id}, "bogus", f.t0); !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("RevokeGrants with an unknown reason: %v, want ErrInvalid", err)
	}
	if g, err := f.st.GetGrant(f.ctx, id); err != nil || g.RevokedAt != nil {
		t.Errorf("the grant after the refused revocation: %+v, %v", g, err)
	}
}

// TestPGMalformedIDsAreNotFound: an id that is not a UUID is "not found" at every entry point, never a
// driver error (storetest covers the common ones).
func TestPGMalformedIDsAreNotFound(t *testing.T) {
	f := newPGFixture(t)
	bad := "not-a-uuid"
	secret := oauth.AppSecret{ID: f.uuid(), AppID: bad, Alias: "a", Hash: pgHash(), CreatedAt: f.t0}
	if err := f.st.CreateSecret(f.ctx, secret); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("CreateSecret for a malformed app id: %v", err)
	}
	a := oauth.Authorization{ID: f.uuid(), AppID: bad, RedirectURI: "x", CreatedAt: f.t0, ExpiresAt: f.t0.Add(time.Hour)}
	if err := f.st.CreateAuthorization(f.ctx, a); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("CreateAuthorization for a malformed app id: %v", err)
	}
	if n, err := f.st.CountPending(f.ctx, bad, f.t0); err != nil || n != 0 {
		t.Errorf("CountPending for a malformed app id = %d, %v", n, err)
	}
	err := f.st.RotateRefresh(f.ctx, oauth.RotateInput{AppID: bad, TokenHash: pgHash(), Now: f.t0, Grace: oauth.RefreshGrace}, nil)
	if !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("RotateRefresh for a malformed app id: %v", err)
	}
}

// TestPGWithCodeWaitsForTheLock: a second redemption of a code does not run while the first holds the
// row, and then sees what the first committed. This is what makes C2 hold, shown without relying on
// timing luck.
func TestPGWithCodeWaitsForTheLock(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	user := f.uuid()
	code := f.approved(app, user, f.org)

	inside, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a test that fails early must not leave the first transaction parked: the pool would wait for it
	first := make(chan error, 1)
	go func() {
		first <- f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
			if _, err := tx.Complete(ctx, f.newGrant(app, user, pgHash(), pgHash())); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-first:
		t.Fatalf("the first redemption ended before it held the row: %v", err)
	}

	seen := make(chan string, 1)
	second := make(chan error, 1)
	go func() {
		second <- f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
			seen <- tx.Authorization().Status
			return nil
		})
	}()
	select {
	case s := <-seen:
		t.Fatalf("the second redemption ran (state %q) while the first held the row", s)
	case <-time.After(300 * time.Millisecond):
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second redemption: %v", err)
	}
	if s := <-seen; s != oauth.StatusExchanged {
		t.Errorf("the second redemption saw %q, want exchanged", s)
	}
}

// TestPGRotateRefreshWaitsForTheLock: a second exchange of a refresh token waits for the first, then
// passes the grace test against the first one's stamp.
func TestPGRotateRefreshWaitsForTheLock(t *testing.T) {
	f := newPGFixture(t)
	app := f.app()
	_, _, refresh := f.grant(app, f.uuid())
	now := f.t0.Add(time.Hour)
	rotate := func(at time.Time, fn func(context.Context, oauth.RotateTx) error) error {
		return f.st.RotateRefresh(f.ctx, oauth.RotateInput{AppID: app.ID, TokenHash: refresh, Now: at, Grace: oauth.RefreshGrace}, fn)
	}

	inside, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	first := make(chan error, 1)
	go func() {
		first <- rotate(now, func(ctx context.Context, tx oauth.RotateTx) error {
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-first:
		t.Fatalf("the first exchange ended before it held the token: %v", err)
	}

	stamp := make(chan *time.Time, 1)
	second := make(chan error, 1)
	go func() {
		second <- rotate(now.Add(5*time.Second), func(ctx context.Context, tx oauth.RotateTx) error {
			stamp <- tx.Token().UsedAt
			return nil
		})
	}()
	select {
	case <-stamp:
		t.Fatal("the second exchange ran while the first held the token")
	case <-time.After(300 * time.Millisecond):
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second exchange: %v", err)
	}
	if got := <-stamp; got == nil || !got.Equal(now) {
		t.Errorf("the second exchange saw UsedAt %v, want the first stamp %v", got, now)
	}
}

// TestPGCompleteRefusesAGrantThatWasNotApproved: a code can only be turned into the grant its
// approver approved. A grant for another user, organization, app or resource, or with a scope the
// request did not carry, is ErrInvalid and writes nothing (C4, enforced where the row is locked).
func TestPGCompleteRefusesAGrantThatWasNotApproved(t *testing.T) {
	f := newPGFixture(t)
	app, other := f.app(), f.app()
	user := f.uuid()
	code := f.approved(app, user, f.org)
	otherOrg, err := f.st.SeedOrg(f.ctx, "beta")
	f.must(err)

	redeem := func(ng oauth.NewGrant) error {
		return f.st.WithCode(f.ctx, app.ID, code, func(ctx context.Context, tx oauth.CodeTx) error {
			_, err := tx.Complete(ctx, ng)
			return err
		})
	}
	for name, mutate := range map[string]func(*oauth.NewGrant){
		"another user":         func(ng *oauth.NewGrant) { ng.Grant.UserID = f.uuid() },
		"another organization": func(ng *oauth.NewGrant) { ng.Grant.OrgID = otherOrg },
		"another app":          func(ng *oauth.NewGrant) { ng.Grant.AppID = other.ID },
		"a wider scope":        func(ng *oauth.NewGrant) { ng.Grant.Scopes = append(slices.Clone(app.Scopes), "projects:write") },
		"another resource":     func(ng *oauth.NewGrant) { ng.Grant.Resource = "https://api.example.com/mcp" },
	} {
		ng := f.newGrant(app, user, pgHash(), pgHash())
		mutate(&ng)
		if err := redeem(ng); !errors.Is(err, oauth.ErrInvalid) {
			t.Errorf("Complete with %s: %v, want ErrInvalid", name, err)
		}
	}
	if infos, err := f.st.ListGrants(f.ctx, oauth.GrantFilter{}); err != nil || len(infos) != 0 {
		t.Fatalf("grants after the refusals: %d, %v", len(infos), err)
	}
	// The code was not spent, and a narrower grant of the same approval is fine.
	ng := f.newGrant(app, user, pgHash(), pgHash())
	ng.Grant.Scopes = app.Scopes[:1]
	f.must(redeem(ng))

	// An approval names its approver.
	a := oauth.Authorization{ID: f.uuid(), AppID: app.ID, RedirectURI: "http://127.0.0.1/cb", Scopes: app.Scopes, CreatedAt: f.t0, ExpiresAt: f.t0.Add(time.Hour)}
	f.must(f.st.CreateAuthorization(f.ctx, a))
	_, err = f.st.DecideAuthorization(f.ctx, a.ID, oauth.Decision{
		Status: oauth.StatusApproved, At: f.t0.Add(time.Minute), OrgID: f.org, CodeHash: pgHash(), CodeExpiresAt: f.t0.Add(2 * time.Minute),
	})
	if !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("approval without an approver: %v, want ErrInvalid", err)
	}
}
