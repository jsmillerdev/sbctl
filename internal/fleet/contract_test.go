package fleet_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// These tests pin what package fleet repeats from packages that import it (lifecycle) or
// that it must not import (api, which imports lifecycle): secret names and role names.

type nullSup struct{}

func (nullSup) Render(context.Context, units.Spec) error             { return nil }
func (nullSup) Start(context.Context, string) error                  { return nil }
func (nullSup) Stop(context.Context, string) error                   { return nil }
func (nullSup) Remove(context.Context, string) error                 { return nil }
func (nullSup) Status(context.Context, string) (units.Status, error) { return units.Status{}, nil }

type dirs map[string]string

func (d dirs) Dir(svc string) (string, error) { return "/art/" + svc, nil }

func node(t *testing.T) (*config.Config, *registry.Memory, secrets.Secrets) {
	t.Helper()
	sec, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "sbctl.test"
	reg := registry.NewMemory()
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: config.SystemRef, Name: "system", Class: "system"}); err != nil {
		t.Fatal(err)
	}
	return cfg, reg, sec
}

func seal(t *testing.T, reg registry.Registry, sec secrets.Secrets, name, val string) {
	t.Helper()
	b, err := sec.Seal([]byte(val))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.PutSecret(context.Background(), config.SystemRef, name, b); err != nil {
		t.Fatal(err)
	}
}

func TestPGMetaKeyIsTheOneTheAPIUses(t *testing.T) {
	if fleet.SecretPGMetaCryptoKey != api.SecretPGMetaCryptoKey {
		t.Fatalf("secret names differ: %q vs %q", fleet.SecretPGMetaCryptoKey, api.SecretPGMetaCryptoKey)
	}
	cfg, reg, sec := node(t)
	for _, svc := range []string{"supavisor", "realtime", "storage"} {
		seal(t, reg, sec, "fleet_"+svc+"_password", "pw")
	}
	ctx := context.Background()
	// The API asks first (it creates the key on first use); the unit must get the same one.
	want, err := api.EnsurePGMetaCryptoKey(ctx, reg, sec)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Registry: reg, Secrets: sec, Supervisor: nullSup{}, Artifacts: dirs{}})
	if err != nil {
		t.Fatal(err)
	}
	specs, err := m.Specs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Service == config.SvcPGMeta {
			if got := s.Env["CRYPTO_KEY"]; got != want {
				t.Fatalf("sb-pgmeta CRYPTO_KEY = %q, the API encrypts with %q", got, want)
			}
			return
		}
	}
	t.Fatal("no pgmeta spec")

}

func TestConfiguredPGMetaKeyMatchesTheAPI(t *testing.T) {
	cfg, reg, sec := node(t)
	cfg.API.PGMetaCryptoKey = "operator-chosen-key"
	for _, svc := range []string{"supavisor", "realtime", "storage"} {
		seal(t, reg, sec, "fleet_"+svc+"_password", "pw")
	}
	m, _ := fleet.NewManager(fleet.Deps{Cfg: cfg, Registry: reg, Secrets: sec, Supervisor: nullSup{}, Artifacts: dirs{}})
	specs, err := m.Specs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Service == config.SvcPGMeta && s.Env["CRYPTO_KEY"] != "operator-chosen-key" {
			t.Fatalf("CRYPTO_KEY = %q", s.Env["CRYPTO_KEY"])
		}
	}
}

// The services connect as the roles lifecycle.InitSystem creates, with the passwords it
// seals; the names must agree or no service could start.
func TestServicesUseLifecycleFleetRoles(t *testing.T) {
	cfg, reg, sec := node(t)
	ctx := context.Background()
	for _, fr := range lifecycle.FleetRoles {
		seal(t, reg, sec, "fleet_"+fr.Service+"_password", "pw-"+fr.Service)
	}
	// lifecycle reads the passwords back under its own names: the convention matches.
	eng := lifecycle.NewEngine(cfg, reg, sec, nil, nil, lifecycle.Options{})
	creds, err := eng.FleetCredentials(ctx)
	if err != nil || len(creds) != 3 {
		t.Fatalf("lifecycle does not find the secrets under the names fleet reads: %v %v", creds, err)
	}
	m, _ := fleet.NewManager(fleet.Deps{Cfg: cfg, Registry: reg, Secrets: sec, Supervisor: nullSup{}, Artifacts: dirs{}})
	specs, err := m.Specs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]map[string]string{}
	for _, s := range specs {
		env[s.Service] = s.Env
	}
	for _, c := range creds {
		var dsn string
		switch c.Service {
		case "supavisor":
			dsn = env["supavisor"]["DATABASE_URL"]
		case "storage":
			dsn = env["storage"]["DATABASE_MULTITENANT_URL"]
		case "realtime":
			e := env["realtime"]
			if e["DB_USER"] != c.Role || e["DB_PASSWORD"] != c.Password || e["DB_NAME"] != c.Database {
				t.Errorf("realtime connects as %s/%s to %s, lifecycle created %s/%s for %s", e["DB_USER"], e["DB_PASSWORD"], e["DB_NAME"], c.Role, c.Password, c.Database)
			}
			continue
		}
		if !strings.Contains(dsn, "//"+c.Role+":"+c.Password+"@") || !strings.HasSuffix(dsn, "/"+c.Database) {
			t.Errorf("%s DSN %q does not match role %s, database %s", c.Service, dsn, c.Role, c.Database)
		}
	}
}

// What Setup returns must be assignable to lifecycle.Options.Fleet.
func TestSetupResultFitsLifecycleOptions(t *testing.T) {
	cfg, reg, sec := node(t)
	for _, svc := range []string{"supavisor", "realtime", "storage"} {
		seal(t, reg, sec, "fleet_"+svc+"_password", "pw")
	}
	fl, err := fleet.Setup(context.Background(), fleet.Deps{Cfg: cfg, Registry: reg, Secrets: sec})
	if err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Options{Fleet: fl}
}
