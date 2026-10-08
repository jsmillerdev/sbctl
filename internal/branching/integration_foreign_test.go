package branching

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/lifecycle"
)

// authClient does not keep connections open: the test ports (39000-39999) lie inside Linux's
// ephemeral range, and a kept-alive client socket on one of them would stop a Postgres that is
// started later from binding it.
var authClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// authCall posts a JSON body to a project's GoTrue on loopback and returns the status and the
// decoded JSON object. GoTrue answers sign-in requests without an apikey; the proxy in front of it
// is what asks for one.
func authCall(t *testing.T, port int, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	var lastErr error
	for i := 0; i < 40; i++ {
		resp, err := authClient.Post(fmt.Sprintf("http://127.0.0.1:%d%s", port, path), "application/json", bytes.NewReader(b))
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	t.Fatalf("GoTrue on port %d did not answer %s: %v", port, path, lastErr)
	return 0, nil
}

// A branch with data must not be able to act on another project (or the parent) through the
// node's loopback: its foreign servers and user mappings are disabled and the stored passwords
// dropped, connection strings in cron commands are replaced, and the parent's credentials inside
// the data (Vault secrets, cron commands, database settings) are the branch's own. The parent's
// own foreign server keeps working. The opt-out keeps the foreign server and still gets its
// credentials replaced.
func TestIntegrationCloneNeutralizesForeignServersAndParentCredentials(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	other, err := st.node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "other project", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := st.node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "fdw parent", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	pref := parent.Ref
	otherKeys, err := st.node.Engine.Keys(ctx, other.Ref)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := st.node.Engine.Keys(ctx, pref)
	if err != nil {
		t.Fatal(err)
	}
	otherPort := st.cfg.PortsFor(other.Ref, other.Seq).Postgres
	if _, err := st.conn(other.Ref, lifecycle.RoleAdmin).Exec(ctx, `create table public.victim (id int primary key, v text); insert into public.victim values (1, 'orig')`); err != nil {
		t.Fatal(err)
	}

	admin := st.conn(pref, lifecycle.RoleAdmin)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	opts := fmt.Sprintf(`host '127.0.0.1', port '%d', dbname 'postgres'`, otherPort)
	must(`create extension if not exists postgres_fdw`)
	must(`create server other_pg foreign data wrapper postgres_fdw options (` + opts + `)`)
	must(fmt.Sprintf(`create user mapping for public server other_pg options (user 'postgres', password '%s')`, otherKeys.DBPassword))
	must(`create foreign table public.victim_ft (id int, v text) server other_pg options (schema_name 'public', table_name 'victim')`)
	must(`create extension if not exists dblink`)
	must(`create server other_dl foreign data wrapper dblink_fdw options (` + opts + `)`)
	must(fmt.Sprintf(`create user mapping for supabase_admin server other_dl options (user 'postgres', password '%s')`, otherKeys.DBPassword))
	// The control: the parent can write to the other project through its foreign table.
	must(`insert into public.victim_ft values (2, 'written by the parent')`)

	cron := true
	if _, err := admin.Exec(ctx, `create extension if not exists pg_cron`); err != nil {
		t.Logf("pg_cron is not usable on this cluster, the cron assertions are skipped: %v", err)
		cron = false
	}
	if cron {
		// Schedules that never fire (31 February): the parent's own jobs must not touch the other project during the test.
		must(fmt.Sprintf(`select cron.schedule('dblink-job', '0 0 31 2 *', $cmd$select dblink_exec('host=127.0.0.1 port=%d dbname=postgres user=postgres password=%s', 'update public.victim set v = ''hacked''')$cmd$)`, otherPort, otherKeys.DBPassword))
		must(`select cron.schedule('key-job', '0 0 31 2 *', $cmd$select 'Bearer ` + pk.ServiceRoleKey + `'$cmd$)`)
	}
	vault := true
	if _, err := admin.Exec(ctx, `create extension if not exists supabase_vault cascade`); err != nil {
		t.Logf("supabase_vault is not usable on this cluster, the vault assertions are skipped: %v", err)
		vault = false
	}
	if vault {
		must(`select vault.create_secret($1, 'parent_service_key')`, pk.ServiceRoleKey)
		must(`select vault.create_secret($1, 'auth_header')`, "Bearer "+pk.AnonKey)
		must(`select vault.create_secret('keep me', 'unrelated')`)
	}
	must(`alter database postgres set "app.settings.service_role_key" to '` + pk.ServiceRoleKey + `'`)
	must(`alter database postgres set "app.settings.unrelated" to 'keep me too'`)

	// Databases a branch's owner could otherwise use to keep the parent's foreign servers: one that
	// refuses connections and one that is a template. Isolation and the credential rewrite work
	// through both and leave their flags as they were.
	connDB := func(ref, db string) *pgx.Conn {
		t.Helper()
		dsn, err := st.node.Engine.ConnString(ctx, ref, lifecycle.RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		cc, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cc.Database = db
		c, err := pgx.ConnectConfig(ctx, cc)
		if err != nil {
			t.Fatalf("connect to %s/%s: %v", ref, db, err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	hiddenVault := vault
	for _, h := range []struct{ db, srv, flag string }{
		{"hidden_db", "hidden_srv", "allow_connections false"},
		{"tmpl_db", "tmpl_srv", "is_template true"},
	} {
		must(`create database ` + h.db)
		hc := connDB(pref, h.db)
		for _, q := range []string{
			`create extension postgres_fdw`,
			`create server ` + h.srv + ` foreign data wrapper postgres_fdw options (` + opts + `)`,
			fmt.Sprintf(`create user mapping for public server %s options (user 'postgres', password '%s')`, h.srv, otherKeys.DBPassword),
		} {
			if _, err := hc.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if h.db == "hidden_db" && vault {
			if _, err := hc.Exec(ctx, `create extension if not exists supabase_vault cascade`); err != nil {
				t.Logf("supabase_vault in %s is not usable, its assertion is skipped: %v", h.db, err)
				hiddenVault = false
			} else if _, err := hc.Exec(ctx, `select vault.create_secret($1, 'hidden_key')`, pk.ServiceRoleKey); err != nil {
				t.Fatal(err)
			}
		}
		hc.Close(ctx)
		must(`alter database ` + h.db + ` ` + h.flag)
	}

	// A signed-in user of the parent: a refresh token and a session, plus one-time tokens and a
	// flow in flight. None of it may exist in a branch, and the parent's refresh token must be
	// refused by the branch's GoTrue.
	parentAuth := st.cfg.PortsFor(pref, parent.Seq).GoTrue
	const userEmail, userPassword = "sessions@example.test", "a long enough password 12345"
	if code, body := authCall(t, parentAuth, "/signup", map[string]any{"email": userEmail, "password": userPassword}); code != 200 {
		t.Fatalf("sign up on the parent: %d %v", code, body)
	}
	code, tok := authCall(t, parentAuth, "/token?grant_type=password", map[string]any{"email": userEmail, "password": userPassword})
	parentRefresh, _ := tok["refresh_token"].(string)
	if code != 200 || parentRefresh == "" {
		t.Fatalf("sign in on the parent: %d %v", code, tok)
	}
	must(`insert into auth.one_time_tokens (id, user_id, token_type, token_hash, relates_to)
		select gen_random_uuid(), id, 'recovery_token', 'parent-recovery-token-hash', email from auth.users where email = '` + userEmail + `'`)
	must(`insert into auth.flow_state (id, auth_code, code_challenge_method, code_challenge, provider_type, authentication_method)
		values (gen_random_uuid(), 'parent-auth-code', 'plain', 'parent-code-challenge', 'email', 'magiclink')`)
	must(`update auth.users set recovery_token = 'parent-recovery-token', confirmation_token = 'parent-confirmation-token' where email = '` + userEmail + `'`)
	var parentTokens int
	if err := admin.QueryRow(ctx, `select count(*) from auth.refresh_tokens where token = $1`, parentRefresh).Scan(&parentTokens); err != nil || parentTokens != 1 {
		t.Fatalf("the parent's refresh token is not in its auth.refresh_tokens (%d, %v)", parentTokens, err)
	}
	// Storage's object metadata: the buckets stay in a branch, the rows that describe objects do
	// not (the bytes live in the parent's Storage backend). The tables are Storage's own where its
	// migrations have run on this cluster and a minimal stand-in otherwise; the trigger plays
	// Storage's protect_delete and fails any DELETE that does not suppress triggers.
	must(`create schema if not exists storage`)
	must(`create table if not exists storage.buckets (id text primary key, name text not null)`)
	must(`create table if not exists storage.migrations (id int primary key, name text)`) // the marker the wipe looks for
	must(`create table if not exists storage.objects (id uuid primary key default gen_random_uuid(), bucket_id text references storage.buckets(id), name text)`)
	must(`create or replace function public.test_protect_delete() returns trigger language plpgsql as $f$ begin raise exception 'Direct deletion from storage tables is not allowed'; end $f$`)
	must(`drop trigger if exists test_protect_delete on storage.objects`)
	must(`create trigger test_protect_delete before delete on storage.objects for each statement execute function public.test_protect_delete()`)
	must(`insert into storage.buckets (id, name) values ('parent-bucket', 'parent-bucket') on conflict do nothing`)
	must(`insert into storage.objects (bucket_id, name) values ('parent-bucket', 'a.txt'), ('parent-bucket', 'dir/b.txt')`)
	var parentCronNodes string
	if cron {
		if err := admin.QueryRow(ctx, `select string_agg(jobname || '@' || nodename || ':' || coalesce(nodeport::text, '-'), ',' order by jobid) from cron.job`).Scan(&parentCronNodes); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := admin.Exec(ctx, `checkpoint`); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SUPAVISE_TEST_EXPECT_METHOD") == MethodBackup {
		// Whatever the filesystem can do, take the base-backup path (a restore of the parent's
		// base backup and WAL), as TestIntegrationCloneIsolatesTheParentsIntegrations does.
		st.cfg.Branching.Clone = "backup"
		if _, err := st.bk.BaseBackup(ctx, pref); err != nil {
			t.Fatalf("base backup of the parent: %v", err)
		}
	}

	branchCheck := func(b *Branch, optOut bool) {
		t.Helper()
		bk, err := st.node.Engine.Keys(ctx, b.Ref)
		if err != nil {
			t.Fatal(err)
		}
		ba := st.conn(b.Ref, lifecycle.RoleAdmin)
		str := func(q string, args ...any) string {
			t.Helper()
			var v *string
			if err := ba.QueryRow(ctx, q, args...).Scan(&v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if v == nil {
				return ""
			}
			return *v
		}
		host := func(srv string) string {
			return str(`select (select split_part(o, '=', 2) from unnest(srvoptions) o where o like 'host=%') from pg_foreign_server where srvname = $1`, srv)
		}
		passwords := func(srv string) int {
			var n int
			if err := ba.QueryRow(ctx, `select count(*) from pg_user_mappings m, unnest(m.umoptions) o where m.srvname = $1 and o like 'password=%'`, srv).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		// Credentials are the branch's own wherever the parent had put its own.
		if got := str(`select current_setting('app.settings.service_role_key')`); got != bk.ServiceRoleKey {
			t.Errorf("branch %s: app.settings.service_role_key is not the branch's service key (the parent's: %v)", b.Name, got == pk.ServiceRoleKey)
		}
		if got := str(`select current_setting('app.settings.unrelated')`); got != "keep me too" {
			t.Errorf("branch %s: an unrelated setting was changed: %q", b.Name, got)
		}
		if vault {
			if got := str(`select decrypted_secret from vault.decrypted_secrets where name = 'parent_service_key'`); got != bk.ServiceRoleKey {
				t.Errorf("branch %s: the vault secret is the branch's service key = %v, the parent's = %v", b.Name, got == bk.ServiceRoleKey, got == pk.ServiceRoleKey)
			}
			if got := str(`select decrypted_secret from vault.decrypted_secrets where name = 'auth_header'`); got != "Bearer "+bk.AnonKey {
				t.Errorf("branch %s: the vault header was not rewritten to the branch's anon key", b.Name)
			}
			if got := str(`select decrypted_secret from vault.decrypted_secrets where name = 'unrelated'`); got != "keep me" {
				t.Errorf("branch %s: an unrelated vault secret was changed: %q", b.Name, got)
			}
		}
		if cron {
			if got := str(`select command from cron.job where jobname = 'key-job'`); got != `select 'Bearer `+bk.ServiceRoleKey+`'` {
				t.Errorf("branch %s: the key in a cron command was not replaced: %q", b.Name, got)
			}
		}
		// What was rewritten is recorded by name, never by value.
		for _, secret := range []string{pk.ServiceRoleKey, pk.AnonKey, pk.JWTSecret, otherKeys.DBPassword, bk.ServiceRoleKey} {
			if n := str(`select count(*)::text from `+RewriteTable+` t where t::text like '%' || $1 || '%'`, secret); n != "0" {
				t.Errorf("branch %s: %s holds a credential value", b.Name, RewriteTable)
			}
		}
		if n := str(`select count(*)::text from ` + RewriteTable); n == "0" || n == "" {
			t.Errorf("branch %s: nothing recorded in %s", b.Name, RewriteTable)
		}
		evs, _ := st.node.Registry.ListEvents(ctx, b.Ref, 50)
		var rewrote bool
		for _, e := range evs {
			if e.Kind != "branch.credentials_rewritten" {
				continue
			}
			rewrote = true
			for _, secret := range []string{pk.ServiceRoleKey, pk.AnonKey, pk.JWTSecret, bk.ServiceRoleKey, bk.JWTSecret} {
				if strings.Contains(string(e.Payload), secret) {
					t.Errorf("branch %s: the rewrite event carries a credential value: %s", b.Name, e.Payload)
				}
			}
			var res RewriteResult
			if err := json.Unmarshal(e.Payload, &res); err != nil || res.DBSettings < 1 || vault && res.VaultSecrets < 2 || cron && res.CronCommands < 1 {
				t.Errorf("branch %s: rewrite event %s (%v)", b.Name, e.Payload, err)
			}
		}
		if !rewrote {
			t.Errorf("branch %s: no branch.credentials_rewritten event", b.Name)
		}

		// No session of the parent's users is left, with or without the opt-out: they sign in again.
		bp, err := st.node.Registry.GetProject(ctx, b.Ref)
		if err != nil {
			t.Fatal(err)
		}
		for q, want := range map[string]string{
			`select count(*) from auth.refresh_tokens where token = '` + parentRefresh + `'`:                                   "0",
			`select count(*) from auth.refresh_tokens`:                                                                         "0",
			`select count(*) from auth.sessions`:                                                                               "0",
			`select count(*) from auth.one_time_tokens`:                                                                        "0",
			`select count(*) from auth.flow_state`:                                                                             "0",
			`select count(*) from auth.users where recovery_token <> '' or confirmation_token <> ''`:                           "0",
			`select count(*) from auth.users where email = '` + userEmail + `'`:                                                "1",
			`select count(*) from auth.identities i join auth.users u on u.id = i.user_id where u.email = '` + userEmail + `'`: "1",
		} {
			if got := str(`select (` + q + `)::text`); got != want {
				t.Errorf("branch %s: %s = %s, want %s", b.Name, q, got, want)
			}
		}
		branchAuth := st.cfg.PortsFor(b.Ref, bp.Seq).GoTrue
		if code, body := authCall(t, branchAuth, "/token?grant_type=refresh_token", map[string]any{"refresh_token": parentRefresh}); code == 200 {
			t.Errorf("branch %s: GoTrue accepted a refresh token taken from the parent: %v", b.Name, body)
		}
		if cron {
			port := st.cfg.PortsFor(b.Ref, bp.Seq).Postgres
			if got := str(`select count(*)::text from cron.job where nodename <> '127.0.0.1' or nodeport <> ` + fmt.Sprint(port)); got != "0" {
				t.Errorf("branch %s: %s cron job(s) still name a node other than the branch (port %d)", b.Name, got, port)
			}
		}
		// The branch has the parent's buckets and none of its objects, with or without the opt-out.
		if got := str(`select count(*)::text from storage.objects`); got != "0" {
			t.Errorf("branch %s: %s storage.objects row(s) of the parent remain", b.Name, got)
		}
		if got := str(`select count(*)::text from storage.buckets where id = 'parent-bucket'`); got != "1" {
			t.Errorf("branch %s: the parent's bucket is gone (%s)", b.Name, got)
		}
		// The databases that refuse connections or are templates were worked through, and are as they were.
		if got := str(`select datallowconn::text || ',' || datistemplate::text from pg_database where datname = 'hidden_db'`); got != "false,false" {
			t.Errorf("branch %s: hidden_db flags = %s, want false,false", b.Name, got)
		}
		if got := str(`select datallowconn::text || ',' || datistemplate::text from pg_database where datname = 'tmpl_db'`); got != "true,true" {
			t.Errorf("branch %s: tmpl_db flags = %s, want true,true", b.Name, got)
		}
		inDB := func(db, q string, args ...any) string {
			t.Helper()
			// hidden_db refuses connections: open it for this look and close it again.
			if db == "hidden_db" {
				if _, err := ba.Exec(ctx, `alter database hidden_db allow_connections true`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := ba.Exec(ctx, `alter database hidden_db allow_connections false`); err != nil {
						t.Fatal(err)
					}
				}()
			}
			c := connDB(b.Ref, db)
			defer c.Close(ctx) // a socket held open here could sit on a port a later cluster needs
			var v *string
			if err := c.QueryRow(ctx, q, args...).Scan(&v); err != nil {
				t.Fatalf("%s/%s: %s: %v", b.Name, db, q, err)
			}
			if v == nil {
				return ""
			}
			return *v
		}
		if hiddenVault {
			if got := inDB("hidden_db", `select decrypted_secret from vault.decrypted_secrets where name = 'hidden_key'`); got != bk.ServiceRoleKey {
				t.Errorf("branch %s: the vault secret in hidden_db is the branch's key = %v, the parent's = %v", b.Name, got == bk.ServiceRoleKey, got == pk.ServiceRoleKey)
			}
		}
		for _, h := range []struct{ db, srv string }{{"hidden_db", "hidden_srv"}, {"tmpl_db", "tmpl_srv"}} {
			q := `select (select split_part(o, '=', 2) from unnest(srvoptions) o where o like 'host=%') from pg_foreign_server where srvname = '` + h.srv + `'`
			if optOut {
				if got := inDB(h.db, q); got != "127.0.0.1" {
					t.Errorf("branch %s: the opt-out changed %s in %s (host %q)", b.Name, h.srv, h.db, got)
				}
				continue
			}
			if got := inDB(h.db, q); got != disabledHost {
				t.Errorf("branch %s: foreign server %s in %s has host %q, want %q", b.Name, h.srv, h.db, got, disabledHost)
			}
			if got := inDB(h.db, `select count(*)::text from pg_user_mappings m, unnest(m.umoptions) o where m.srvname = '`+h.srv+`' and o like 'password=%'`); got != "0" {
				t.Errorf("branch %s: %s user mapping password(s) of %s remain in %s", b.Name, got, h.srv, h.db)
			}
		}
		// The isolation event has counts for all of it, and no value.
		for _, e := range evs {
			if e.Kind != "branch.isolated" {
				continue
			}
			var res IsolateResult
			if err := json.Unmarshal(e.Payload, &res); err != nil {
				t.Fatal(err)
			}
			if res.AuthRowsDeleted["refresh_tokens"] < 1 || res.AuthRowsDeleted["sessions"] < 1 || res.AuthRowsDeleted["one_time_tokens"] != 1 ||
				res.AuthRowsDeleted["flow_state"] != 1 || res.AuthRowsCleared["users"] != 1 {
				t.Errorf("branch %s: isolation event auth counts: %s", b.Name, e.Payload)
			}
			if res.StorageRowsDeleted["objects"] != 2 {
				t.Errorf("branch %s: isolation event storage counts: %s", b.Name, e.Payload)
			}
			if len(res.AuthTablesNotReviewed) != 0 {
				t.Errorf("branch %s: auth tables that isolate_auth.go does not know: %v (a new GoTrue release: classify them)", b.Name, res.AuthTablesNotReviewed)
			}
			if len(res.DatabasesOpened) != 1 || res.DatabasesOpened[0] != "hidden_db" {
				t.Errorf("branch %s: databases opened = %v, want [hidden_db]", b.Name, res.DatabasesOpened)
			}
			if cron && res.CronNodesReset < 1 {
				t.Errorf("branch %s: no cron node reset recorded: %s", b.Name, e.Payload)
			}
			for _, secret := range []string{parentRefresh, "parent-recovery-token", "parent-auth-code", "parent-recovery-token-hash"} {
				if strings.Contains(string(e.Payload), secret) {
					t.Errorf("branch %s: the isolation event carries a session value", b.Name)
				}
			}
		}
		// The branch signs its users in again, with tokens of its own.
		if code, body := authCall(t, branchAuth, "/token?grant_type=password", map[string]any{"email": userEmail, "password": userPassword}); code != 200 || body["refresh_token"] == parentRefresh {
			t.Errorf("branch %s: sign-in on the branch: %d %v", b.Name, code, body)
		}

		if optOut {
			// The opt-out keeps the parent's outbound side effects, foreign servers included.
			if host("other_pg") != "127.0.0.1" || host("other_dl") != "127.0.0.1" || passwords("other_pg") != 1 {
				t.Errorf("branch %s: the opt-out changed the foreign servers (host %q, passwords %d)", b.Name, host("other_pg"), passwords("other_pg"))
			}
			if str(`select to_regclass('`+PausedForeignTable+`')::text`) != "" {
				t.Errorf("branch %s: the opt-out recorded foreign servers", b.Name)
			}
			return
		}
		for _, srv := range []string{"other_pg", "other_dl"} {
			if h := host(srv); h != disabledHost {
				t.Errorf("branch %s: foreign server %s host = %q, want %q", b.Name, srv, h, disabledHost)
			}
			if n := passwords(srv); n != 0 {
				t.Errorf("branch %s: %d user mapping(s) of %s still hold a password", b.Name, n, srv)
			}
		}
		// The foreign table cannot write: it fails, and the other project is untouched.
		if _, err := ba.Exec(ctx, `insert into public.victim_ft values (3, 'written by the branch')`); err == nil {
			t.Errorf("branch %s: the foreign table accepted a write", b.Name)
		}
		if _, err := ba.Exec(ctx, `select dblink_connect('supavise_c', 'other_dl')`); err == nil {
			t.Errorf("branch %s: dblink connected through the named server", b.Name)
		}
		// Recorded: server, wrapper, the original host, port and database, who lost a password; never the password.
		want := fmt.Sprintf("postgres_fdw|127.0.0.1|%d|postgres|{public}", otherPort)
		if got := str(`select fdw_name || '|' || original_host || '|' || original_port || '|' || original_dbname || '|' || passwords_dropped_for::text from ` + PausedForeignTable + ` where server_name = 'other_pg'`); got != want {
			t.Errorf("branch %s: paused_foreign_servers row for other_pg = %q, want %q", b.Name, got, want)
		}
		if n := str(`select count(*)::text from `+PausedForeignTable+` t where t::text like '%' || $1 || '%'`, otherKeys.DBPassword); n != "0" {
			t.Errorf("branch %s: %s holds the stored password", b.Name, PausedForeignTable)
		}
		if cron {
			cmd := str(`select command from cron.job where jobname = 'dblink-job'`)
			if strings.Contains(cmd, otherKeys.DBPassword) || strings.Contains(cmd, "127.0.0.1") || !strings.Contains(cmd, disabledConnString) || !strings.Contains(cmd, "update public.victim") {
				t.Errorf("branch %s: dblink command = %q", b.Name, cmd)
			}
			if n := str(`select count(*)::text from ` + NeutralizedCronTable + ` n join cron.job j using (jobid) where j.jobname = 'dblink-job'`); n != "1" {
				t.Errorf("branch %s: the neutralized cron job is not recorded (%s)", b.Name, n)
			}
		}
		for _, e := range evs {
			if e.Kind != "branch.isolated" {
				continue
			}
			var res IsolateResult
			if err := json.Unmarshal(e.Payload, &res); err != nil || res.ForeignServers != 4 || res.MappingPasswords != 4 || cron && res.CronCommands != 1 {
				t.Errorf("branch %s: isolation event %s (%v)", b.Name, e.Payload, err)
			}
			if strings.Contains(string(e.Payload), otherKeys.DBPassword) {
				t.Errorf("branch %s: the isolation event carries a password", b.Name)
			}
		}
	}

	b := st.createBranch(pref, "no-fdw", func(in *CreateInput) { in.WithData = true })
	branchCheck(b, false)
	open := st.createBranch(pref, "fdw-opt-out", func(in *CreateInput) { in.WithData, in.AllowEgress = true, true })
	branchCheck(open, true)

	// The other project got exactly the parent's write, and the parent's own foreign server still works.
	var rows int
	if err := st.conn(other.Ref, lifecycle.RoleAdmin).QueryRow(ctx, `select count(*) from public.victim`).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("the other project has %d rows (%v), want 2: the branch must not have written to it", rows, err)
	}
	must(`insert into public.victim_ft values (4, 'parent again')`)
	var v string
	if err := admin.QueryRow(ctx, `select v from public.victim_ft where id = 4`).Scan(&v); err != nil || v != "parent again" {
		t.Errorf("the parent's foreign table after the branches: %q %v", v, err)
	}
	var parentHost string
	if err := admin.QueryRow(ctx, `select (select split_part(o, '=', 2) from unnest(srvoptions) o where o like 'host=%') from pg_foreign_server where srvname = 'other_pg'`).Scan(&parentHost); err != nil || parentHost != "127.0.0.1" {
		t.Errorf("the parent's server host = %q %v", parentHost, err)
	}
	// The parent's session is as it was: its refresh token still works against the parent.
	if code, body := authCall(t, parentAuth, "/token?grant_type=refresh_token", map[string]any{"refresh_token": parentRefresh}); code != 200 {
		t.Errorf("the parent's refresh token stopped working after the branches: %d %v", code, body)
	}
	if cron {
		var nodes string
		if err := admin.QueryRow(ctx, `select string_agg(jobname || '@' || nodename || ':' || coalesce(nodeport::text, '-'), ',' order by jobid) from cron.job`).Scan(&nodes); err != nil || nodes != parentCronNodes {
			t.Errorf("the parent's cron nodes changed: %q, was %q (%v)", nodes, parentCronNodes, err)
		}
	}
	if vault {
		var parentVault string
		if err := admin.QueryRow(ctx, `select decrypted_secret from vault.decrypted_secrets where name = 'parent_service_key'`).Scan(&parentVault); err != nil || parentVault != pk.ServiceRoleKey {
			t.Errorf("the parent's vault secret changed: %v", err)
		}
	}
	if cron {
		var cmd string
		if err := admin.QueryRow(ctx, `select command from cron.job where jobname = 'dblink-job'`).Scan(&cmd); err != nil || !strings.Contains(cmd, otherKeys.DBPassword) {
			t.Errorf("the parent's cron command changed: %q %v", cmd, err)
		}
	}

	// A table of the user's with a foreign key into storage.objects: the wipe would leave rows that
	// point at nothing, so it is skipped (with the reason recorded) and the branch is still created.
	must(`create table public.user_avatars (id int primary key, object_id uuid references storage.objects(id))`)
	must(`insert into public.user_avatars select 1, id from storage.objects order by name limit 1`)
	fk := st.createBranch(pref, "storage-fk", func(in *CreateInput) { in.WithData = true })
	fkConn := st.conn(fk.Ref, lifecycle.RoleAdmin)
	var objects, avatars int
	if err := fkConn.QueryRow(ctx, `select (select count(*) from storage.objects), (select count(*) from public.user_avatars)`).Scan(&objects, &avatars); err != nil || objects != 2 || avatars != 1 {
		t.Errorf("branch %s: storage.objects has %d rows and user_avatars %d (%v), want the parent's 2 and 1 (the wipe is skipped)", fk.Name, objects, avatars, err)
	}
	var skipped bool
	fkEvs, _ := st.node.Registry.ListEvents(ctx, fk.Ref, 50)
	for _, e := range fkEvs {
		if e.Kind != "branch.isolated" {
			continue
		}
		var res IsolateResult
		if err := json.Unmarshal(e.Payload, &res); err != nil {
			t.Fatal(err)
		}
		skipped = len(res.StorageWipeSkipped) == 1 && strings.Contains(res.StorageWipeSkipped[0], "user_avatars_object_id_fkey") &&
			strings.Contains(res.StorageWipeSkipped[0], "public.user_avatars -> storage.objects") && len(res.StorageRowsDeleted) == 0
	}
	if !skipped {
		t.Errorf("branch %s: the isolation event does not say that the Storage wipe was skipped: %v", fk.Name, fkEvs)
	}
}
