package registry

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is the pg_advisory_lock key that serializes concurrent migrators.
const migrationLockID = 0x5bc71

// Migrations returns the embedded migration file names in apply order.
func Migrations() ([]string, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// Migrate creates schema sbctl if needed and applies every embedded migration not yet
// recorded in sbctl.schema_migrations, each in its own transaction. The pool must be
// connected to the "sbctl" database as its owner.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error { return migrate(ctx, pool, "") }

// migrate is Migrate that stops before the migration named stopBefore ("" applies all);
// tests use it to put data in place before a data-moving migration runs.
func migrate(ctx context.Context, pool *pgxpool.Pool, stopBefore string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `
		create schema if not exists sbctl;
		create table if not exists sbctl.schema_migrations (
			version    text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return err
	}
	names, err := Migrations()
	if err != nil {
		return err
	}
	for _, name := range names {
		if stopBefore != "" && name >= "migrations/"+stopBefore {
			break
		}
		var done bool
		if err := conn.QueryRow(ctx, `select exists (select 1 from sbctl.schema_migrations where version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into sbctl.schema_migrations (version) values ($1)`, name)
			return err
		}); err != nil {
			return fmt.Errorf("registry: migration %s: %w", name, err)
		}
	}
	return nil
}
