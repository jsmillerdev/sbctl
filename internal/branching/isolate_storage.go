package branching

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A branch has Storage objects of its own, and none of the parent's. Storage keeps an object's
// bytes in the project's file or S3 backend (a tenant of its own, per ref) and its metadata in
// the project's database (storage.objects). A clone of the parent's data carries the metadata but
// not the bytes, so every listed object would answer 404 on download, and a branch that deletes
// one would ask the backend to remove a file it never had. Hosted Supabase does not copy Storage
// objects into a branch either, so the branch keeps the buckets (their names, visibility, size
// limits and the policies on them are part of the schema and the settings) and loses the rows
// that describe objects.
//
// The tables below hold object metadata, or the state of an upload in flight, and nothing else;
// children before parents. Rows of other Storage tables (buckets, migrations, the Iceberg and
// vector catalogs) stay.
//
// One exception: a table of the user's that has a foreign key into one of these tables (an
// avatar_id column that references storage.objects) would keep rows that point at nothing, and
// the wipe cannot delete or null them without changing the user's data. The whole wipe is then
// skipped for that database, not a part of it (a branch with objects but no folder tree is worse
// than either), and the branch is created as before: its objects are listed and answer 404, which
// is what this change fixes for the common case. The reason and the constraint names are in the
// branch.isolated event (storage_wipe_skipped) and the exception is in the branching README.
var storageWipedTables = []string{
	"s3_multipart_uploads_parts", // parts of S3-protocol uploads in flight
	"s3_multipart_uploads",       // S3-protocol uploads in flight
	"prefixes",                   // the folder tree Storage derives from object names
	"objects",                    // one row per object
}

// clearStorageObjects empties storageWipedTables in the Storage schema of the connected database
// and records the row counts in res. It acts only where Storage's own schema is: buckets,
// objects and Storage's migrations table (a user's schema that is also called storage and has
// tables of those names, but not the migrations table, is left alone). User triggers do not fire (the same reason as in
// wipeAuthSessions: a trigger on storage.objects may call the parent's integrations while the
// branch still has network), which also skips Storage's own protect_delete trigger.
func clearStorageObjects(ctx context.Context, c *pgx.Conn, res *IsolateResult) error {
	var has bool
	if err := c.QueryRow(ctx, `select to_regclass('storage.buckets') is not null and to_regclass('storage.objects') is not null and to_regclass('storage.migrations') is not null`).Scan(&has); err != nil {
		return err
	}
	if !has {
		return nil
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		return fmt.Errorf("suppress triggers for the Storage wipe: %w", err)
	}
	kept, err := wipeClosureViolations(ctx, tx, "storage", storageWipedTables)
	if err != nil {
		return err
	}
	if len(kept) > 0 {
		var db string
		if err := tx.QueryRow(ctx, `select current_database()`).Scan(&db); err != nil {
			return err
		}
		var l []string
		for _, f := range kept {
			l = append(l, f.String())
		}
		res.StorageWipeSkipped = appendUnique(res.StorageWipeSkipped, fmt.Sprintf(
			"%s: a table of the project references Storage's object metadata, so the rows were kept: %s", db, strings.Join(l, ", ")))
		return nil // the deferred Rollback ends the transaction
	}
	if res.StorageRowsDeleted == nil {
		res.StorageRowsDeleted = map[string]int64{}
	}
	for _, t := range storageWipedTables {
		var exists bool
		if err := tx.QueryRow(ctx, `select to_regclass('storage.' || quote_ident($1)) is not null`, t).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		tag, err := tx.Exec(ctx, "delete from storage."+pgx.Identifier{t}.Sanitize())
		if err != nil {
			return fmt.Errorf("empty storage.%s: %w", t, err)
		}
		res.StorageRowsDeleted[t] += tag.RowsAffected()
	}
	return tx.Commit(ctx)
}
