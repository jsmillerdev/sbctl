package storagemigrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// privateDatabase creates a database that only this test uses and returns its DSN; the database is
// dropped when the test ends. It needs SUPAVISE_TEST_DATABASE_URL.
func privateDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "supavise_storagemigrate_test_" + hex.EncodeToString(b[:])
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name
}

func TestRowsReadStorageObjects(t *testing.T) {
	dsn := privateDatabase(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// A database that Storage never initialized has no objects.
	n := 0
	if err := rowsAt(ctx, dsn, func(Row) error { n++; return nil }); err != nil || n != 0 {
		t.Fatalf("without a storage schema: %d rows, %v", n, err)
	}

	for _, q := range []string{
		`create schema storage`,
		`create table storage.objects (id uuid primary key default gen_random_uuid(), bucket_id text, name text, version text, metadata jsonb)`,
		`insert into storage.objects (bucket_id, name, version, metadata) values
		   ('docs', 'top.txt', 'v1', '{"size": 17, "mimetype": "text/plain"}'),
		   ('docs', 'sp ace/ü-é.txt', 'v2', '{"size": 3}'),
		   ('docs', 'nosize.bin', 'v3', '{}'),
		   ('docs', 'nometa.bin', 'v4', null),
		   ('docs', 'old.bin', null, '{"size": 9}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var got []Row
	if err := rowsAt(ctx, dsn, func(r Row) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	want := []Row{
		{Bucket: "docs", Name: "nometa.bin", Version: "v4"},
		{Bucket: "docs", Name: "nosize.bin", Version: "v3"},
		{Bucket: "docs", Name: "sp ace/ü-é.txt", Version: "v2", Size: 3, HasSize: true},
		{Bucket: "docs", Name: "top.txt", Version: "v1", Size: 17, HasSize: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows (a row without a version has no key and is left out)\n got: %+v\nwant: %+v", got, want)
	}
	// The row's path is the key below <ref>/.
	if p := rowPath(want[2]); p != "docs/sp ace/ü-é.txt/v2" {
		t.Errorf("rowPath = %q", p)
	}
}

func TestRowsReportAProjectWhoseDatabaseIsDown(t *testing.T) {
	err := rowsAt(ctx, "host=/nonexistent/socket/dir port=1 user=u dbname=d connect_timeout=2", func(Row) error { return nil })
	if err != ErrOffline {
		t.Errorf("a missing socket: %v, want ErrOffline", err)
	}
	err = rowsAt(ctx, "host=127.0.0.1 port=1 user=u dbname=d connect_timeout=2", func(Row) error { return nil })
	if err != ErrOffline {
		t.Errorf("a closed port: %v, want ErrOffline", err)
	}
}
