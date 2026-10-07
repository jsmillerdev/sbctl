package branching

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Migration is one row of supabase_migrations.schema_migrations: what `supabase db push`
// and the Management API's apply-migration record.
type Migration struct {
	Version    string
	Name       string
	Statements []string
}

// sql is the migration as one script, for diffs and equality.
func (m Migration) sql() string { return strings.Join(m.Statements, ";\n") }

func sameMigration(a, b Migration) bool {
	return a.Name == b.Name && normalizeSQL(a.sql()) == normalizeSQL(b.sql())
}

func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(s), ";")), " ")
}

// Conflict is a version both histories have, with different content.
type Conflict struct {
	Version string
	Source  Migration
	Target  Migration
}

// Plan compares a source history with a target history.
type Plan struct {
	// Apply are the source's migrations the target lacks, in version order.
	Apply []Migration
	// TargetOnly are the target's migrations the source lacks.
	TargetOnly []Migration
	// Conflicts are versions on both sides with different content (never in Apply).
	Conflicts []Conflict
	// OutOfOrder are versions in Apply that sort before the target's latest version: they
	// would run after a migration that came later in version order.
	OutOfOrder []string
}

// Compare diffs two migration histories. Versions compare as strings, which orders the
// CLI's 14-digit timestamps correctly.
func Compare(source, target []Migration) Plan {
	t := make(map[string]Migration, len(target))
	latest := ""
	for _, m := range target {
		t[m.Version] = m
		latest = max(latest, m.Version)
	}
	s := make(map[string]Migration, len(source))
	var p Plan
	for _, m := range source {
		s[m.Version] = m
		have, ok := t[m.Version]
		switch {
		case !ok:
			p.Apply = append(p.Apply, m)
			if m.Version < latest {
				p.OutOfOrder = append(p.OutOfOrder, m.Version)
			}
		case !sameMigration(m, have):
			p.Conflicts = append(p.Conflicts, Conflict{Version: m.Version, Source: m, Target: have})
		}
	}
	for _, m := range target {
		if _, ok := s[m.Version]; !ok {
			p.TargetOnly = append(p.TargetOnly, m)
		}
	}
	byVersion := func(ms []Migration) {
		sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	}
	byVersion(p.Apply)
	byVersion(p.TargetOnly)
	sort.Slice(p.Conflicts, func(i, j int) bool { return p.Conflicts[i].Version < p.Conflicts[j].Version })
	sort.Strings(p.OutOfOrder)
	return p
}

// Summary describes a plan for error messages and events.
func (p Plan) Summary() string {
	var parts []string
	if len(p.TargetOnly) > 0 {
		parts = append(parts, "target has migrations the source lacks: "+versions(p.TargetOnly))
	}
	if len(p.Conflicts) > 0 {
		var v []string
		for _, c := range p.Conflicts {
			v = append(v, c.Version)
		}
		parts = append(parts, "versions with different content: "+strings.Join(v, ", "))
	}
	if len(p.OutOfOrder) > 0 {
		parts = append(parts, "migrations older than the target's latest: "+strings.Join(p.OutOfOrder, ", "))
	}
	return strings.Join(parts, "; ")
}

func versions(ms []Migration) string {
	v := make([]string, len(ms))
	for i, m := range ms {
		v[i] = m.Version
	}
	return strings.Join(v, ", ")
}

// Database runs SQL on a project's "postgres" database as the postgres role.
type Database interface {
	// Migrations returns the project's recorded migration history (empty when it has none).
	Migrations(ctx context.Context, ref string) ([]Migration, error)
	// Apply runs the migrations in order and records each one. With o.Atomic they run in a
	// single transaction (all or nothing) and fall back to one transaction each when a
	// statement cannot run inside a transaction block.
	Apply(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error)
	// RunScript executes a script (a seed) in one transaction.
	RunScript(ctx context.Context, ref, script string) error
}

// ApplyOptions tune Database.Apply.
type ApplyOptions struct {
	Atomic bool
	// LockTimeout bounds how long a statement waits for a lock (0: no limit). Merges set it
	// so that DDL on a busy parent fails instead of queueing behind, and blocking, traffic.
	LockTimeout time.Duration
}

// ApplyResult reports how Apply ran.
type ApplyResult struct {
	Applied int
	// NonAtomic is set when the all-or-nothing transaction was not possible.
	NonAtomic bool
}

// pgDatabase is the Database over pgx.
type pgDatabase struct {
	dsn func(ctx context.Context, ref string) (string, error)
}

func (d *pgDatabase) conn(ctx context.Context, ref string) (*pgx.Conn, error) {
	dsn, err := d.dsn(ctx, ref)
	if err != nil {
		return nil, err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cc.RuntimeParams["application_name"] = "sbctl-branching"
	return pgx.ConnectConfig(ctx, cc)
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

const migrationsTable = "supabase_migrations.schema_migrations"

// Migrations implements Database.
func (d *pgDatabase) Migrations(ctx context.Context, ref string) ([]Migration, error) {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer closeConn(c)
	var has bool
	if err := c.QueryRow(ctx, `select to_regclass('`+migrationsTable+`') is not null`).Scan(&has); err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	rows, err := c.Query(ctx, `select version, coalesce(name, ''), coalesce(statements, '{}') from `+migrationsTable+` order by version`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Migration, error) {
		var m Migration
		err := r.Scan(&m.Version, &m.Name, &m.Statements)
		return m, err
	})
}

const ensureMigrationsTable = `
create schema if not exists supabase_migrations;
create table if not exists ` + migrationsTable + ` (version text not null primary key, statements text[], name text);
alter table ` + migrationsTable + ` add column if not exists created_by text;
alter table ` + migrationsTable + ` add column if not exists idempotency_key text unique;
alter table ` + migrationsTable + ` add column if not exists rollback text[];`

// notInTransaction is SQLSTATE 25001 (active_sql_transaction): CREATE INDEX CONCURRENTLY,
// VACUUM and the like.
func notInTransaction(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "25001"
}

func applyOne(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, m Migration) error {
	for i, st := range m.Statements {
		if strings.TrimSpace(st) == "" {
			continue
		}
		if _, err := q.Exec(ctx, st); err != nil {
			return fmt.Errorf("migration %s (statement %d): %w", m.Version, i+1, err)
		}
	}
	// A plain insert: a version that is already recorded aborts the transaction instead of
	// letting its DDL run twice with only one record.
	_, err := q.Exec(ctx, `insert into `+migrationsTable+` (version, name, statements) values ($1, nullif($2, ''), $3)`, m.Version, m.Name, m.Statements)
	return err
}

// Apply implements Database.
func (d *pgDatabase) Apply(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error) {
	var res ApplyResult
	if len(ms) == 0 {
		return res, nil
	}
	c, err := d.conn(ctx, ref)
	if err != nil {
		return res, err
	}
	defer closeConn(c)
	if o.LockTimeout > 0 {
		if _, err := c.Exec(ctx, fmt.Sprintf(`set lock_timeout = %d`, o.LockTimeout.Milliseconds())); err != nil {
			return res, err
		}
	}
	if _, err := c.Exec(ctx, ensureMigrationsTable); err != nil {
		return res, fmt.Errorf("prepare %s: %w", migrationsTable, err)
	}
	if o.Atomic {
		// One merge at a time per project: two would interleave their DDL. The lock belongs to
		// this session, so it also covers the statement-by-statement fallback below, and it is
		// released when the connection closes. The decision to apply was made before the lock
		// was held: look again, so that a version another merge just applied is not run twice.
		if _, err := c.Exec(ctx, `select pg_advisory_lock(hashtext('sbctl.branching.apply'))`); err != nil {
			return res, fmt.Errorf("wait for other migrations on %s: %w", ref, err)
		}
		versions := make([]string, len(ms))
		for i, m := range ms {
			versions[i] = m.Version
		}
		rows, err := c.Query(ctx, `select version from `+migrationsTable+` where version = any($1) order by version`, versions)
		if err != nil {
			return res, err
		}
		have, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return res, err
		}
		if len(have) > 0 {
			return res, fmt.Errorf("%w: %s already applied to %s while this operation waited; read the histories again", ErrDiverged, strings.Join(have, ", "), ref)
		}
	}
	if o.Atomic {
		err := pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
			for _, m := range ms {
				if err := applyOne(ctx, tx, m); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			res.Applied = len(ms)
			return res, nil
		}
		if !notInTransaction(err) {
			return res, err
		}
		res.NonAtomic = true
	}
	for _, m := range ms {
		if err := applyEach(ctx, c, m); err != nil {
			return res, err
		}
		res.Applied++
	}
	return res, nil
}

// applyEach runs one migration in its own transaction, or outside one when a statement
// refuses to run inside a transaction block.
func applyEach(ctx context.Context, c *pgx.Conn, m Migration) error {
	err := pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error { return applyOne(ctx, tx, m) })
	if err == nil || !notInTransaction(err) {
		return err
	}
	return applyOne(ctx, c, m)
}

// RunScript implements Database.
func (d *pgDatabase) RunScript(ctx context.Context, ref, script string) error {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return err
	}
	defer closeConn(c)
	return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, script)
		return err
	})
}
