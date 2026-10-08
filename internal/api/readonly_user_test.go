package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The databases listing names hosted's read-only user, supabase_read_only_user, in the read-only
// connection string (readOnlyUser). The API never connects as it (SQL that must not write runs as
// supavise_read_only), but the name should be one the project's database has. This reads the SQL
// that the pinned Postgres artifact runs when it creates a project's cluster, so a pin that drops
// the role fails here. It needs SUPAVISE_TEST_UNPACKED (the unpacked artifacts directory, as for
// the other integration tests) and skips without it; no workflow sets it for this package, so in CI
// this test never runs. roles-smoke (tests/linux) asks a real cluster of each shipped archive for the
// same role, login, bypassrls, pg_read_all_data and the read-only session default included, and is
// what checks the archives.
func TestPinnedPostgresCreatesTheReadOnlyUser(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "postgres-17*"))
	if len(dirs) == 0 {
		t.Skipf("no postgres artifact under %s", root)
	}
	var sql strings.Builder
	for _, glob := range []string{"init-scripts/*.sql", "migrations/*.sql"} {
		files, _ := filepath.Glob(filepath.Join(dirs[len(dirs)-1], "share", "supabase-cli", "migrations", glob))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			sql.Write(b)
			sql.WriteByte('\n')
		}
	}
	if sql.Len() == 0 {
		t.Fatalf("no SQL under %s/share/supabase-cli/migrations", dirs[len(dirs)-1])
	}
	for what, re := range map[string]string{
		"is created with login and bypassrls": `(?i)create role supabase_read_only_user with login bypassrls;`,
		"reads all data":                      `(?i)grant pg_read_all_data to supabase_read_only_user;`,
		"opens its sessions read-only":        `(?i)alter role supabase_read_only_user set default_transaction_read_only = on;`,
	} {
		if !regexp.MustCompile(re).MatchString(sql.String()) {
			t.Errorf("%s: supabase_read_only_user %s", dirs[len(dirs)-1], what)
		}
	}
}
