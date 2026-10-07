package branching

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/secrets"
)

// The data of a branch is a copy of the parent's, and the parent's credentials come with it where
// the parent's own setup put them: a Vault secret that holds the service key for a cron job that
// calls an Edge Function, a database setting such as app.settings.service_role_key, the key
// inside the command of a cron job. A branch reaches the node's loopback (its own services, the
// proxy, the other projects' Postgres), so a cloned parent credential is a key to the parent
// that an agent working in the branch can read and use. After the branch's own credentials exist
// (rotateCredentials), rewriteCredentials replaces, in the places a Supabase project keeps them,
// every value that is one of the parent's credentials with the branch's corresponding value:
//
//   - Vault secrets (vault.decrypted_secrets, rewritten through vault.update_secret, which
//     encrypts the new value under the secret's own key),
//   - pg_cron job commands (active or paused),
//   - database and role settings (pg_db_role_setting, the ALTER DATABASE/ROLE ... SET values).
//
// What counts as the parent's credential: the legacy anon and service_role JWTs (and any other
// anon or service_role JWT that verifies against the parent's JWT secret, an older key included),
// the sb_publishable_ and sb_secret_ keys, the JWT secret, and the parent's six database
// passwords. A value is rewritten when it contains one (a "Bearer <key>" header, a connection
// string), not only when it equals one, except that a credential shorter than
// minSubstringLen (none that sbctl generates) is matched whole.
//
// Not detected: a credential in anything else, such as an arbitrary user table, a function body,
// a trigger's arguments (a database webhook's headers), a foreign table option or a Storage
// object, and a credential of the parent that is not one of those above (a third party's API key
// is not the branch's to replace; with denied egress it cannot leave the node). What was
// rewritten is recorded by name, never by value, in sbctl_branch.rewritten_credentials and in the
// branch.credentials_rewritten event.

const minSubstringLen = 20

// RewriteTable lists the credentials of the parent that were replaced in a database.
const RewriteTable = "sbctl_branch.rewritten_credentials"

type keyPair struct{ label, old, new string }

// credentialSwap maps the parent's credentials to the branch's.
type credentialSwap struct {
	literals        []keyPair
	parentJWTSecret string
	jwtSecret       keyPair
	branchByRole    map[string]string // anon, service_role -> the branch's legacy key
}

func newCredentialSwap(parent, branch *secrets.ProjectKeys) *credentialSwap {
	cs := &credentialSwap{
		parentJWTSecret: parent.JWTSecret,
		jwtSecret:       keyPair{"jwt_secret", parent.JWTSecret, branch.JWTSecret},
		branchByRole:    map[string]string{secrets.RoleAnon: branch.AnonKey, secrets.RoleServiceRole: branch.ServiceRoleKey},
	}
	for _, p := range []keyPair{
		{"secret_key", parent.SecretKey, branch.SecretKey},
		{"publishable_key", parent.PublishableKey, branch.PublishableKey},
		{"service_role_key", parent.ServiceRoleKey, branch.ServiceRoleKey},
		{"anon_key", parent.AnonKey, branch.AnonKey},
		{"db_password", parent.DBPassword, branch.DBPassword},
		{"admin_password", parent.AdminPassword, branch.AdminPassword},
		{"authenticator_password", parent.AuthenticatorPassword, branch.AuthenticatorPassword},
		{"auth_admin_password", parent.AuthAdminPassword, branch.AuthAdminPassword},
		{"storage_admin_password", parent.StorageAdminPassword, branch.StorageAdminPassword},
		{"replication_password", parent.ReplicationPassword, branch.ReplicationPassword},
	} {
		if p.old != "" && p.new != "" && p.old != p.new {
			cs.literals = append(cs.literals, p)
		}
	}
	if cs.jwtSecret.old == "" || cs.jwtSecret.new == "" || cs.jwtSecret.old == cs.jwtSecret.new {
		cs.jwtSecret = keyPair{}
	}
	return cs
}

func replaceCredential(s string, p keyPair) (string, bool) {
	switch {
	case p.old == "":
		return s, false
	case s == p.old:
		return p.new, true
	case len(p.old) >= minSubstringLen && strings.Contains(s, p.old):
		return strings.ReplaceAll(s, p.old, p.new), true
	}
	return s, false
}

var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{8,}`)

// rewrite returns s with the parent's credentials replaced by the branch's and the labels of the
// credentials it replaced (names, never values).
func (c *credentialSwap) rewrite(s string) (string, []string) {
	var labels []string
	add := func(l string) {
		for _, x := range labels {
			if x == l {
				return
			}
		}
		labels = append(labels, l)
	}
	for _, p := range c.literals {
		var ok bool
		if s, ok = replaceCredential(s, p); ok {
			add(p.label)
		}
	}
	// Any other legacy key signed with the parent's secret (a key issued earlier, re-signed since
	// or kept in a note): valid against the parent whatever it says in exp.
	if c.parentJWTSecret != "" && strings.Contains(s, "eyJ") {
		s = jwtLike.ReplaceAllStringFunc(s, func(tok string) string {
			role := parentSignedRole(tok, c.parentJWTSecret)
			repl, ok := c.branchByRole[role]
			if !ok || repl == "" {
				return tok
			}
			add(role + "_key")
			return repl
		})
	}
	if ns, ok := replaceCredential(s, c.jwtSecret); ok {
		s = ns
		add(c.jwtSecret.label)
	}
	return s, labels
}

// parentSignedRole returns the role claim of tok when it is an HS256 JWT signed with secret
// (whatever its expiry), "" otherwise.
func parentSignedRole(tok, secret string) string {
	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithoutClaimsValidation()).
		ParseWithClaims(tok, claims, func(*jwt.Token) (any, error) { return []byte(secret), nil })
	if err != nil {
		return ""
	}
	role, _ := claims["role"].(string)
	return role
}

// RewrittenCredential names one place where a parent credential was replaced.
type RewrittenCredential struct {
	Kind     string   `json:"kind"` // vault_secret, cron_command, db_setting
	Database string   `json:"database,omitempty"`
	Name     string   `json:"name"`
	Replaced []string `json:"replaced"` // which credentials, by label
}

// RewriteResult says what rewriteCredentials changed (names only).
type RewriteResult struct {
	VaultSecrets int                   `json:"vault_secrets"`
	CronCommands int                   `json:"cron_commands"`
	DBSettings   int                   `json:"db_settings"`
	Items        []RewrittenCredential `json:"rewritten,omitempty"`
}

func (r *RewriteResult) add(it RewrittenCredential) {
	switch it.Kind {
	case "vault_secret":
		r.VaultSecrets++
	case "cron_command":
		r.CronCommands++
	case "db_setting":
		r.DBSettings++
	}
	r.Items = append(r.Items, it)
}

// rewriteCredentials runs rewriteDatabase in every database of the cluster at dsn.
func rewriteCredentials(ctx context.Context, dsn string, sw *credentialSwap) (RewriteResult, error) {
	var res RewriteResult
	c, err := connect(ctx, dsn, "")
	if err != nil {
		return res, err
	}
	rows, err := c.Query(ctx, `select datname from pg_database where datallowconn and not datistemplate order by datname`)
	var dbs []string
	if err == nil {
		dbs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err == nil {
		// The settings catalog is shared by all databases: once, from the first connection.
		err = rewriteSettings(ctx, c, sw, &res)
	}
	closeConn(c)
	if err != nil {
		return res, fmt.Errorf("database and role settings: %w", err)
	}
	for _, db := range dbs {
		if err := rewriteDatabase(ctx, dsn, db, sw, &res); err != nil {
			return res, fmt.Errorf("database %s: %w", db, err)
		}
	}
	return res, nil
}

// quietSession keeps the values this session handles out of the server log.
func quietSession(ctx context.Context, c *pgx.Conn) error {
	for _, stmt := range []string{`set log_statement = 'none'`, `set pgaudit.log = 'none'`, `set log_min_duration_statement = -1`} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	// Parameters of a failed statement go into the log from PostgreSQL 13: not these.
	_, _ = c.Exec(ctx, `set log_parameter_max_length_on_error = 0`)
	return nil
}

func rewriteDatabase(ctx context.Context, dsn, db string, sw *credentialSwap, res *RewriteResult) error {
	c, err := connect(ctx, dsn, db)
	if err != nil {
		return err
	}
	defer closeConn(c)
	if err := quietSession(ctx, c); err != nil {
		return err
	}
	var items []RewrittenCredential

	var hasView, hasUpdate bool
	if err := c.QueryRow(ctx, `select to_regclass('vault.decrypted_secrets') is not null, to_regprocedure('vault.update_secret(uuid,text,text,text,uuid)') is not null`).Scan(&hasView, &hasUpdate); err != nil {
		return err
	}
	if hasView {
		if !hasUpdate {
			return fmt.Errorf("vault.decrypted_secrets exists but vault.update_secret(uuid, text, text, text, uuid) does not: the parent's secrets cannot be replaced")
		}
		rows, err := c.Query(ctx, `select id::text, coalesce(name, ''), decrypted_secret from vault.decrypted_secrets`)
		if err != nil {
			return fmt.Errorf("read the vault: %w", err)
		}
		type change struct{ id, val string }
		var changes []change
		for rows.Next() {
			var id, name string
			var secret *string
			if err := rows.Scan(&id, &name, &secret); err != nil {
				rows.Close()
				return err
			}
			if secret == nil {
				continue
			}
			if nv, labels := sw.rewrite(*secret); len(labels) > 0 {
				changes = append(changes, change{id, nv})
				if name == "" {
					name = id
				}
				items = append(items, RewrittenCredential{Kind: "vault_secret", Database: db, Name: name, Replaced: labels})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the vault: %w", err)
		}
		for _, ch := range changes {
			if _, err := c.Exec(ctx, `select vault.update_secret($1::uuid, $2::text)`, ch.id, ch.val); err != nil {
				return fmt.Errorf("rewrite a vault secret: %w", err)
			}
		}
	}

	var hasCron bool
	if err := c.QueryRow(ctx, `select to_regclass('cron.job') is not null`).Scan(&hasCron); err != nil {
		return err
	}
	if hasCron {
		rows, err := c.Query(ctx, `select jobid, coalesce(jobname, ''), command from cron.job order by jobid`)
		if err != nil {
			return err
		}
		type change struct {
			id  int64
			cmd string
		}
		var changes []change
		for rows.Next() {
			var id int64
			var name, cmd string
			if err := rows.Scan(&id, &name, &cmd); err != nil {
				rows.Close()
				return err
			}
			if nc, labels := sw.rewrite(cmd); len(labels) > 0 {
				changes = append(changes, change{id, nc})
				if name == "" {
					name = fmt.Sprintf("job %d", id)
				}
				items = append(items, RewrittenCredential{Kind: "cron_command", Database: db, Name: name, Replaced: labels})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, ch := range changes {
			if _, err := c.Exec(ctx, `update cron.job set command = $2 where jobid = $1`, ch.id, ch.cmd); err != nil {
				return fmt.Errorf("rewrite a cron command: %w", err)
			}
		}
	}
	for _, it := range items {
		res.add(it)
	}
	return recordRewrites(ctx, c, items)
}

// rewriteSettings rewrites the values of database and role settings (pg_db_role_setting).
func rewriteSettings(ctx context.Context, c *pgx.Conn, sw *credentialSwap, res *RewriteResult) error {
	if err := quietSession(ctx, c); err != nil {
		return err
	}
	rows, err := c.Query(ctx, `select coalesce(d.datname, ''), coalesce(r.rolname, ''), x.cfg
		from pg_db_role_setting s
		left join pg_database d on d.oid = s.setdatabase
		left join pg_roles r on r.oid = s.setrole,
		unnest(s.setconfig) as x(cfg)`)
	if err != nil {
		return err
	}
	type change struct{ db, role, name, val string }
	var changes []change
	var items []RewrittenCredential
	for rows.Next() {
		var db, role, cfg string
		if err := rows.Scan(&db, &role, &cfg); err != nil {
			rows.Close()
			return err
		}
		name, val, _ := strings.Cut(cfg, "=")
		if nv, labels := sw.rewrite(val); len(labels) > 0 {
			changes = append(changes, change{db, role, name, nv})
			items = append(items, RewrittenCredential{Kind: "db_setting", Database: db, Name: settingName(db, role, name), Replaced: labels})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ch := range changes {
		var stmt string
		var err error
		switch {
		case ch.role != "" && ch.db != "":
			err = c.QueryRow(ctx, `select format('alter role %I in database %I set %I to %L', $1::text, $2::text, $3::text, $4::text)`, ch.role, ch.db, ch.name, ch.val).Scan(&stmt)
		case ch.role != "":
			err = c.QueryRow(ctx, `select format('alter role %I set %I to %L', $1::text, $2::text, $3::text)`, ch.role, ch.name, ch.val).Scan(&stmt)
		case ch.db != "":
			err = c.QueryRow(ctx, `select format('alter database %I set %I to %L', $1::text, $2::text, $3::text)`, ch.db, ch.name, ch.val).Scan(&stmt)
		default:
			err = c.QueryRow(ctx, `select format('alter role all set %I to %L', $1::text, $2::text)`, ch.name, ch.val).Scan(&stmt)
		}
		if err != nil {
			return err
		}
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("rewrite the setting %s: %w", settingName(ch.db, ch.role, ch.name), err)
		}
	}
	for _, it := range items {
		res.add(it)
	}
	return recordRewrites(ctx, c, items)
}

func settingName(db, role, name string) string {
	switch {
	case db != "" && role != "":
		return fmt.Sprintf("%s (role %s in database %s)", name, role, db)
	case db != "":
		return fmt.Sprintf("%s (database %s)", name, db)
	case role != "":
		return fmt.Sprintf("%s (role %s)", name, role)
	}
	return name + " (all roles)"
}

// recordRewrites lists what was replaced in the database c is connected to, names only.
func recordRewrites(ctx context.Context, c *pgx.Conn, items []RewrittenCredential) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`create schema if not exists sbctl_branch`,
		`create table if not exists ` + RewriteTable + ` (
			kind text not null,
			name text not null,
			replaced text[] not null,
			rewritten_at timestamptz not null default now(),
			primary key (kind, name))`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx, `insert into `+RewriteTable+` (kind, name, replaced) values ($1, $2, $3) on conflict (kind, name) do nothing`, it.Kind, it.Name, it.Replaced); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// rewriteBranchCredentials is Service.rewrite: it replaces the parent's credentials inside the
// data of the branch ref with the branch's own, after rotateCredentials gave it its own.
func (s *Service) rewriteBranchCredentials(ctx context.Context, ref string) error {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.Branch == nil {
		return fmt.Errorf("%s is not a branch", ref)
	}
	parentKeys, err := s.eng.Keys(ctx, p.Branch.ParentRef)
	if err != nil {
		return fmt.Errorf("read the parent's credentials: %w", err)
	}
	branchKeys, err := s.eng.Keys(ctx, ref)
	if err != nil {
		return fmt.Errorf("read the branch's credentials: %w", err)
	}
	res, err := rewriteCredentials(ctx, s.adminSocketDSN(ref, p.Seq), newCredentialSwap(parentKeys, branchKeys))
	if err != nil {
		return err
	}
	s.event(ctx, ref, "branch.credentials_rewritten", res)
	return nil
}
