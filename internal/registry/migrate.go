package registry

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// Migrate creates schema supavise if needed and applies every embedded migration not yet
// recorded in supavise.schema_migrations, each in its own transaction. The pool must be
// connected to the "supavise" database as its owner.
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
		create schema if not exists supavise;
		create table if not exists supavise.schema_migrations (
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
		if err := conn.QueryRow(ctx, `select exists (select 1 from supavise.schema_migrations where version = $1)`, name).Scan(&done); err != nil {
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
			_, err := tx.Exec(ctx, `insert into supavise.schema_migrations (version) values ($1)`, name)
			return err
		}); err != nil {
			return fmt.Errorf("registry: migration %s: %w", name, err)
		}
	}
	return nil
}

// SchemaVersion is the name of the newest migration this binary embeds ("1190_custom_domains.sql"),
// which is the registry schema it expects. Migrations only go forward, so a registry whose
// AppliedSchema is newer than this was migrated by a newer binary.
func SchemaVersion() string {
	names, err := Migrations()
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[len(names)-1], "migrations/")
}

// AppliedSchema returns the newest migration recorded in the registry database at dsn, without
// applying any (Open would). It is "" for a registry that has none. `supavise upgrade` reads it to
// see whether a binary can still run on the registry as it is.
func AppliedSchema(ctx context.Context, dsn string) (string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var v *string
	err = conn.QueryRow(ctx, `select max(version) from supavise.schema_migrations`).Scan(&v)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42P01" { // undefined_table: never migrated
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return strings.TrimPrefix(*v, "migrations/"), nil
}
