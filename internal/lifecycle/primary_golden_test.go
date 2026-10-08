package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

// goldenKeys are fixed so that the rendered environment is the same on every run.
func goldenKeys() *secrets.ProjectKeys {
	return &secrets.ProjectKeys{
		JWTSecret: "jwt-secret-for-the-golden-test", AnonKey: "anon-key", ServiceRoleKey: "service-role-key",
		PublishableKey: "sb_publishable_x", SecretKey: "sb_secret_x",
		DBPassword: "db-password", AdminPassword: "admin-password", AuthenticatorPassword: "authenticator-password",
		AuthAdminPassword: "auth-admin-password", StorageAdminPassword: "storage-admin-password",
		ReplicationPassword: "replication-password", PGSodiumRootKey: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
	}
}

// renderGolden renders the units of p as the plane does today: the cluster's, GoTrue's and
// PostgREST's, each as its Spec, its environment file and its run script. The state directory
// is replaced by /STATE so that the text does not depend on where the test runs.
func renderGolden(t *testing.T, pl *PostgresPlane, cfg *config.Config, p *registry.Project) string {
	t.Helper()
	ctx := context.Background()
	keys := goldenKeys()
	pg, err := pl.postgresSpec(ctx, p, keys)
	if err != nil {
		t.Fatal(err)
	}
	api, err := pl.apiSpecs(ctx, p, keys)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, spec := range append([]units.Spec{pg}, api...) {
		js, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		env, err := units.FormatEnv(spec.Env)
		if err != nil {
			t.Fatal(err)
		}
		run, err := units.FormatRun(spec)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "### %s\n--- spec\n%s\n--- env\n%s--- run\n%s\n", spec.Unit(), js, env, run)
	}
	return strings.ReplaceAll(b.String(), cfg.StateDir, "/STATE")
}

// TestPrimaryUnitsMatchV011 is invariant I4: the units of a project that is homed on its node
// render exactly as v0.1.1 rendered them, so an upgrade to a release with replicas restarts no
// cluster (`supavise upgrade --restart-changed` compares digests of these files). The golden
// files in testdata/v0.1.1 were produced by the v0.1.1 sources with this test's fixtures
// (SUPAVISE_UPDATE_GOLDEN=1 writes them); do not regenerate them from a later release.
func TestPrimaryUnitsMatchV011(t *testing.T) {
	type tc struct {
		name  string
		setup func(pl *PostgresPlane, cfg *config.Config) *registry.Project
	}
	cases := []tc{
		{"user-relay", func(pl *PostgresPlane, cfg *config.Config) *registry.Project {
			cfg.Backup.WALRelay = "on"
			return testProject(cfg, "abcdefghijklmnopqrst", 2)
		}},
		{"user-direct-archive-initialized", func(pl *PostgresPlane, cfg *config.Config) *registry.Project {
			cfg.Backup.WALRelay = "off"
			pl.opts.ConfigPath = "/etc/supavise/config.toml"
			p := testProject(cfg, "abcdefghijklmnopqrst", 7)
			p.Class = "large"
			p.Limits = mustClass(t, "large").Limits()
			// An initialized cluster: no bootstrap password in the environment.
			if err := os.MkdirAll(pl.paths(p).Data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pl.paths(p).Data, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"user-saved-settings-and-branch-egress", func(pl *PostgresPlane, cfg *config.Config) *registry.Project {
			cfg.Backup.WALRelay = "on"
			pl.opts.ArchiveTimeout = 300
			pl.opts.Settings = &fakeSettings{
				auth: map[string]string{"GOTRUE_SITE_URL": "https://app.example.com", "GOTRUE_JWT_EXP": "900", "GOTRUE_DB_MAX_POOL_SIZE": ""},
				rest: map[string]string{"PGRST_DB_SCHEMAS": "public,extra", "PGRST_DB_MAX_ROWS": "25"},
				pg:   []string{"max_connections=80", "statement_timeout=30s", "shared_buffers=64MB"},
			}
			p := testProject(cfg, "zyxwvutsrqponmlkjihg", 11)
			p.Branch = &registry.BranchInfo{ParentRef: "abcdefghijklmnopqrst", Egress: registry.EgressDenied}
			return p
		}},
		{"system", func(pl *PostgresPlane, cfg *config.Config) *registry.Project {
			cfg.Backup.WALRelay = "on"
			return &registry.Project{Ref: config.SystemRef, Class: ClassSystem, Engine: registry.EnginePostgres, Limits: cfg.Defaults}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pl, cfg := testPlane(t)
			p := c.setup(pl, cfg)
			got := renderGolden(t, pl, cfg, p)
			path := filepath.Join("testdata", "v0.1.1", c.name+".golden")
			if os.Getenv("SUPAVISE_UPDATE_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("the units of %s no longer render as v0.1.1 rendered them (a restart of every project follows an upgrade):\n%s", c.name, firstDifference(string(want), got))
			}
		})
	}
}

// mustClass is ClassFor that fails the test.
func mustClass(t *testing.T, name string) Class {
	t.Helper()
	c, err := ClassFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// firstDifference names the first line where two texts differ.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d\n  v0.1.1: %s\n  now:    %s", i+1, wl, gl)
		}
	}
	return "(no difference)"
}
