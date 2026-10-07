package branching

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The users of a cloned project can sign in to the parent, and their sessions come with the
// data: GoTrue keeps refresh tokens in plaintext (auth.refresh_tokens), the key that checks them
// per session (auth.sessions.refresh_token_hmac_key), the hashes of outstanding recovery, magic
// link and confirmation tokens (auth.one_time_tokens and the *_token columns of auth.users), and
// PKCE and OAuth flow state. A branch's postgres role may read all of them, and the parent's
// GoTrue listens on loopback, so a copy of any of these is a session of a production user. The
// branch keeps users and identities (they sign in to the branch again with their passwords) and
// loses every row that is a bearer secret of the parent's GoTrue.
//
// The two lists below cover the auth schema of the GoTrue release pinned in versions.yaml
// (auth-v2.195.0). A table in neither list is reported in the isolation event
// (auth_tables_not_reviewed), and the integration test fails on it, so a GoTrue upgrade that adds
// a table gets looked at.

// authWipedTables are emptied in a branch with data, children before parents. All of them hold
// sessions, one-time tokens or the state of a sign-in that is still in flight.
var authWipedTables = []string{
	"saml_relay_states",    // request ids of SAML sign-ins in flight
	"flow_state",           // PKCE auth codes and the provider's access and refresh tokens
	"oauth_authorizations", // OAuth server authorization codes
	"oauth_client_states",  // code verifiers of OAuth sign-ins in flight
	"webauthn_challenges",  // passkey challenges and their session data
	"mfa_challenges",       // pending MFA challenges (SMS codes, WebAuthn session data)
	"mfa_amr_claims",       // how each session was authenticated (cascades from sessions)
	"refresh_tokens",       // plaintext refresh tokens
	"sessions",             // sessions and their refresh token HMAC keys
	"one_time_tokens",      // hashes of recovery, magic link, confirmation and change tokens
}

// authKeptTables are left alone: accounts, linked identities, MFA enrollments, passkeys, OAuth
// and SSO configuration. Nothing in them is a session of the parent's GoTrue, but they are
// production data: password hashes, TOTP secrets, and the client secrets of third-party identity
// providers stay in the branch (README, "What a branch with data can and cannot reach").
var authKeptTables = []string{
	"audit_log_entries", "custom_oauth_providers", "identities", "instances", "mfa_factors",
	"oauth_clients", "oauth_consents", "saml_providers", "schema_migrations", "sso_domains",
	"sso_providers", "users", "webauthn_credentials",
}

// authClearedColumns are columns that hold a one-time secret inside a table that stays. The value
// is the SQL that replaces it: GoTrue reads the users columns into plain strings, so they become
// empty strings, not NULL.
var authClearedColumns = []struct{ table, column, value string }{
	{"users", "confirmation_token", "''"},
	{"users", "recovery_token", "''"},
	{"users", "email_change_token_new", "''"},
	{"users", "email_change_token_current", "''"},
	{"users", "phone_change_token", "''"},
	{"users", "reauthentication_token", "''"},
	{"mfa_factors", "last_webauthn_challenge_data", "null"},
}

// wipeAuthSessions removes the parent's GoTrue session material from the database c is connected
// to (it does nothing where there is no auth schema) and adds the row counts, never values, to res.
// It runs in one transaction and is safe to run twice.
func wipeAuthSessions(ctx context.Context, c *pgx.Conn, res *IsolateResult) error {
	// Only GoTrue's own auth schema is touched: a user's unrelated schema named auth is left alone.
	var has bool
	if err := c.QueryRow(ctx, `select to_regclass('auth.schema_migrations') is not null
		and to_regclass('auth.users') is not null and to_regclass('auth.refresh_tokens') is not null`).Scan(&has); err != nil {
		return err
	}
	if !has {
		return nil
	}
	rows, err := c.Query(ctx, `select c.relname from pg_class c where c.relnamespace = 'auth'::regnamespace and c.relkind in ('r', 'p') order by 1`)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	known := map[string]bool{}
	for _, t := range authWipedTables {
		known[t] = true
	}
	for _, t := range authKeptTables {
		known[t] = true
	}
	for _, t := range tables {
		if !known[t] {
			res.AuthTablesNotReviewed = appendUnique(res.AuthTablesNotReviewed, t)
		}
	}
	sort.Strings(res.AuthTablesNotReviewed)

	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// User triggers on these tables must not fire: they would run the parent's integrations from
	// a branch that may still have network. replica mode also skips foreign-key actions, so every
	// table that references a wiped table must itself be wiped; refuse otherwise.
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		return fmt.Errorf("suppress triggers for the session wipe: %w", err)
	}
	if err := checkWipeClosure(ctx, tx); err != nil {
		return err
	}
	if res.AuthRowsDeleted == nil {
		res.AuthRowsDeleted = map[string]int64{}
	}
	for _, t := range authWipedTables {
		if !have[t] {
			continue
		}
		tag, err := tx.Exec(ctx, "delete from auth."+pgx.Identifier{t}.Sanitize())
		if err != nil {
			return fmt.Errorf("empty auth.%s: %w", t, err)
		}
		res.AuthRowsDeleted[t] += tag.RowsAffected()
	}

	// The columns, grouped by table; a column a release does not have is skipped.
	rows, err = tx.Query(ctx, `select table_name, column_name from information_schema.columns where table_schema = 'auth' order by table_name, ordinal_position`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var t, col string
		if err := rows.Scan(&t, &col); err != nil {
			rows.Close()
			return err
		}
		cols[t+"."+col] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if res.AuthRowsCleared == nil {
		res.AuthRowsCleared = map[string]int64{}
	}
	byTable := map[string][]int{}
	var order []string
	for i, cc := range authClearedColumns {
		if !cols[cc.table+"."+cc.column] {
			continue
		}
		if _, ok := byTable[cc.table]; !ok {
			order = append(order, cc.table)
		}
		byTable[cc.table] = append(byTable[cc.table], i)
	}
	for _, t := range order {
		var set, where []string
		for _, i := range byTable[t] {
			cc := authClearedColumns[i]
			col := pgx.Identifier{cc.column}.Sanitize()
			set = append(set, col+" = "+cc.value)
			where = append(where, col+" is distinct from "+cc.value)
		}
		tag, err := tx.Exec(ctx, "update auth."+pgx.Identifier{t}.Sanitize()+" set "+strings.Join(set, ", ")+" where "+strings.Join(where, " or "))
		if err != nil {
			return fmt.Errorf("clear the one-time tokens in auth.%s: %w", t, err)
		}
		res.AuthRowsCleared[t] += tag.RowsAffected()
	}
	return tx.Commit(ctx)
}

func appendUnique(l []string, s string) []string {
	for _, x := range l {
		if x == s {
			return l
		}
	}
	return append(l, s)
}

// checkWipeClosure fails when a table that is kept has a foreign key into a wiped table: with
// foreign-key actions suppressed, wiping the referenced table would leave dangling rows.
func checkWipeClosure(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		select con.conname, src.relname, dst.relname
		from pg_constraint con
		join pg_class src on src.oid = con.conrelid
		join pg_class dst on dst.oid = con.confrelid
		where con.contype = 'f'
		  and dst.relnamespace = 'auth'::regnamespace
		  and dst.relname = any($1)
		  and not (src.relnamespace = 'auth'::regnamespace and src.relname = any($1))`, authWipedTables)
	if err != nil {
		return err
	}
	type fk struct{ name, from, to string }
	bad, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fk, error) {
		var f fk
		err := r.Scan(&f.name, &f.from, &f.to)
		return f, err
	})
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		var l []string
		for _, f := range bad {
			l = append(l, fmt.Sprintf("%s (%s -> auth.%s)", f.name, f.from, f.to))
		}
		return fmt.Errorf("a kept table references a session table, so the wipe would leave dangling rows: %s", strings.Join(l, ", "))
	}
	return nil
}
