package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/pflag"

	"github.com/jsmillerdev/supavise/internal/registry"
)

// orgsNode prepares what `supavise orgs` needs without a system cluster: a registry database of its
// own, made inside the database CI provides (SUPAVISE_TEST_DATABASE_URL) and dropped afterwards, so
// the test owns every organization in it, and a config that finds it through SUPAVISE_REGISTRY_DSN.
func orgsNode(t *testing.T) (cfgPath string, reg *registry.Postgres) {
	t.Helper()
	base := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dbName := "orgs_cli_" + hex.EncodeToString(suffix)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `drop database if exists `+dbName+` with (force)`)
		admin.Close(context.Background())
	})
	if _, err := admin.Exec(ctx, `create database `+dbName); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	dsn := u.String()
	reg, err = registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	if err := registry.Migrate(ctx, reg.Pool()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRegistryDSN, dsn)

	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	if err := os.WriteFile(key, []byte(keysTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.toml")
	body := "supervisor = \"exec\"\nstate_dir = \"" + filepath.Join(dir, "state") + "\"\nkey_path = \"" + key + "\"\n\n[backup]\nbackend = \"file://" + filepath.Join(dir, "backups") + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, reg
}

// runOrgs runs `supavise --config cfg orgs args...`. The command objects are shared by every run of
// the test binary, so a flag one run set (--json, --yes) would stay set for the next: they are reset
// first.
func runOrgs(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	for _, sub := range []string{"list", "delete"} {
		c, _, err := rootCmd.Find([]string{"orgs", sub})
		if err != nil {
			t.Fatal(err)
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "bool" {
				_ = f.Value.Set("false")
				f.Changed = false
			}
		})
	}
	return runRoot(t, append([]string{"--config", cfg, "orgs"}, args...)...)
}

func TestOrgsListAndDelete(t *testing.T) {
	cfg, reg := orgsNode(t)
	ctx := context.Background()
	keep, err := reg.CreateOrganization(ctx, "keep", "Keep")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range [][2]string{{"scrap-one", "Scrap one"}, {"scrap-two", "Scrap two"}} {
		if _, err := reg.CreateOrganization(ctx, o[0], o[1]); err != nil {
			t.Fatal(err)
		}
	}
	const ref = "abcdefghijklmnopqrst"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: keep.ID, Name: "kept project"}); err != nil {
		t.Fatal(err)
	}
	orgs := func(args ...string) []orgView {
		t.Helper()
		out, err := runOrgs(t, cfg, append([]string{"list", "--json"}, args...)...)
		if err != nil {
			t.Fatalf("orgs list: %v\n%s", err, out)
		}
		var views []orgView
		if err := json.Unmarshal([]byte(out), &views); err != nil {
			t.Fatalf("orgs list --json: %v\n%s", err, out)
		}
		return views
	}
	slugs := func() string {
		var s []string
		for _, v := range orgs() {
			s = append(s, v.Slug)
		}
		return strings.Join(s, ",")
	}

	// list: every organization with the refs of its projects, in the table too
	byslug := map[string]orgView{}
	for _, v := range orgs() {
		byslug[v.Slug] = v
	}
	if len(byslug) != 3 || len(byslug["keep"].Projects) != 1 || byslug["keep"].Projects[0] != ref || len(byslug["scrap-one"].Projects) != 0 || byslug["scrap-one"].Name != "Scrap one" {
		t.Fatalf("orgs list --json = %+v", byslug)
	}
	table, err := runOrgs(t, cfg, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SLUG", "NAME", "PROJECTS", "keep", "Keep", "scrap-one", "Scrap two"} {
		if !strings.Contains(table, want) {
			t.Errorf("orgs list lacks %q:\n%s", want, table)
		}
	}

	// delete without --yes only says what would go
	out, err := runOrgs(t, cfg, "delete", "keep")
	if err == nil || !strings.Contains(err.Error(), "nothing deleted") {
		t.Fatalf("delete without --yes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would be deleted with 1 project(s): "+ref) {
		t.Errorf("the preview does not name the project:\n%s", out)
	}
	if got := slugs(); got != "keep,scrap-one,scrap-two" {
		t.Fatalf("organizations after the preview: %s", got)
	}
	// an unknown organization is named, with the way to find the right one
	if _, err := runOrgs(t, cfg, "delete", "nope", "--yes"); err == nil || !strings.Contains(err.Error(), `no organization "nope"`) {
		t.Fatalf("unknown organization: %v", err)
	}

	// delete --yes removes an organization that has no project, and leaves the others alone
	out, err = runOrgs(t, cfg, "delete", "scrap-one", "--yes")
	if err != nil || !strings.Contains(out, "organization scrap-one deleted") {
		t.Fatalf("delete scrap-one: %v\n%s", err, out)
	}
	for _, v := range orgs() {
		if v.Slug == "scrap-one" {
			t.Fatal("scrap-one is still listed")
		}
	}
	if _, err := reg.GetOrganization(ctx, "scrap-one"); err == nil {
		t.Fatal("scrap-one is still in the registry")
	}
	if _, err := runOrgs(t, cfg, "delete", "scrap-two", "--yes"); err != nil {
		t.Fatalf("delete scrap-two: %v", err)
	}

	// the node's last organization is kept, with its projects
	out, err = runOrgs(t, cfg, "delete", "keep", "--yes")
	if err == nil || !strings.Contains(err.Error(), "last organization") {
		t.Fatalf("delete of the last organization: %v\n%s", err, out)
	}
	left := orgs()
	if len(left) != 1 || left[0].Slug != "keep" || len(left[0].Projects) != 1 {
		t.Fatalf("after the refused delete: %+v", left)
	}
	if _, err := reg.GetProject(ctx, ref); err != nil {
		t.Fatalf("the refused delete removed the project: %v", err)
	}
}
