package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(`
domain = "Example.com."
[tls]
mode = "dns01"
dns_provider = "cloudflare"
[backup]
retention_days = 3
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBCTL_BACKUP_RETENTION_DAYS", "9")
	t.Setenv("SBCTL_BACKUP_S3_FORCE_PATH_STYLE", "true")
	t.Setenv("SBCTL_TLS_CREDENTIALS_API_TOKEN", "tok")
	t.Setenv("SBCTL_SUPERVISOR", "exec")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backup.RetentionDays != 9 || !c.Backup.S3ForcePathStyle || c.TLS.Credentials["api_token"] != "tok" || c.Supervisor != "exec" {
		t.Fatalf("env overrides not applied: %+v", c)
	}
	if c.Listen.HTTPS != ":443" {
		t.Fatalf("defaults lost: %+v", c.Listen)
	}
	if got := c.ProjectHost("abcdefghijklmnopqrst"); got != "abcdefghijklmnopqrst.api.example.com" {
		t.Fatal(got)
	}
	if got := c.RefFromProjectHost("ABCDEFGHIJKLMNOPQRST.api.example.com:443"); got != "abcdefghijklmnopqrst" {
		t.Fatal(got)
	}
	if got := c.RefFromProjectHost("studio.example.com"); got != "" {
		t.Fatal(got)
	}
}

func TestMissingFileAndValidation(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Defaults, Limits{MemoryMax: "1G", CPUQuota: "100%"}) {
		t.Fatal(c.Defaults)
	}
	t.Setenv("SBCTL_SUPERVISOR", "docker")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("want validation error")
	}
}

func TestLayout(t *testing.T) {
	d := Default()
	if p := d.PortsFor("abcdefghijklmnopqrst", 2); p != (ProjectPorts{20006, 20007, 20008}) {
		t.Fatal(p)
	}
	if p := d.PortsFor(SystemRef, 0); p.Postgres != 5433 || p.GoTrue != 9999 || p.PostgREST != 0 {
		t.Fatal(p)
	}
	t.Setenv("SBCTL_PORTS_PROJECT_BASE", "34000")
	t.Setenv("SBCTL_PORTS_SYSTEM_POSTGRES", "34999")
	c2, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if p := c2.PortsFor("abcdefghijklmnopqrst", 1); p.Postgres != 34003 || c2.PortsFor(SystemRef, 0).Postgres != 34999 {
		t.Fatal(p)
	}
	if u := UnitName(SvcPostgres, "abc"); u != "sb-postgres@abc.service" {
		t.Fatal(u)
	}
	if u := UnitName(SvcRealtime, ""); u != "sb-realtime.service" {
		t.Fatal(u)
	}
	c := Default()
	c.PublicIP = "203.0.113.7"
	if c.StudioHost() != "studio.203.0.113.7.sslip.io" {
		t.Fatal(c.StudioHost())
	}
	if got := c.Paths().Artifact(SvcGoTrue, "auth-v2"); got != "/var/lib/sbctl/artifacts/auth/auth-v2" {
		t.Fatal(got)
	}
}
