package branching

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/lifecycle"
)

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

	if _, err := admin.Exec(ctx, `checkpoint`); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SBCTL_TEST_EXPECT_METHOD") == MethodBackup {
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
		if _, err := ba.Exec(ctx, `select dblink_connect('sbctl_c', 'other_dl')`); err == nil {
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
			if err := json.Unmarshal(e.Payload, &res); err != nil || res.ForeignServers != 2 || res.MappingPasswords != 2 || cron && res.CronCommands != 1 {
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
}
