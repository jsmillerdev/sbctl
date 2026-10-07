package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestRegion(t *testing.T) {
	c := Default()
	if c.Region != "us-east-1" {
		t.Fatalf("default region %q", c.Region)
	}
	for in, want := range map[string]string{"": "us-east-1", "local": "us-east-1", "Frankfurt": "us-east-1", "eu-west-2": "eu-west-2", "ap-southeast-2": "ap-southeast-2", "ap-southeast-4": "us-east-1", "us-gov-west-1": "us-east-1", "eu-south-1": "us-east-1"} {
		if got := c.ProjectRegion(in); got != want {
			t.Errorf("ProjectRegion(%q) = %q, want %q", in, got, want)
		}
	}
	c.Region = "eu-central-1"
	if got := c.ProjectRegion("local"); got != "eu-central-1" {
		t.Errorf("configured region not used: %q", got)
	}
	bad := Default()
	for _, r := range []string{"mars", "eu-south-1", "ap-east-1", "us-gov-west-1", "xx-word-1"} {
		bad.Region = r
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "regions.ts") {
			t.Errorf("region %q: Validate = %v, want a rejection that names Studio's list", r, err)
		}
	}
	for _, r := range Regions {
		ok := Default()
		ok.Region = r
		if err := ok.Validate(); err != nil {
			t.Errorf("region %q: %v", r, err)
		}
	}
	empty := Default()
	empty.Region = ""
	if err := empty.Validate(); err != nil || empty.Region != "us-east-1" {
		t.Errorf("empty region: %v %q", err, empty.Region)
	}
}

func TestMailEnv(t *testing.T) {
	if (Mail{}).Enabled() || (Mail{}).GoTrueEnv() != nil {
		t.Fatal("no host, no mail")
	}
	if (Mail{SMTPHost: "smtp.example.test"}).Enabled() {
		t.Fatal("a sender address is required")
	}
	env := Mail{SMTPHost: "smtp.example.test", SMTPFrom: "ops@example.test", SMTPUser: "u", SMTPPass: "p"}.GoTrueEnv()
	if env["GOTRUE_SMTP_HOST"] != "smtp.example.test" || env["GOTRUE_SMTP_PORT"] != "587" || env["GOTRUE_SMTP_USER"] != "u" ||
		env["GOTRUE_SMTP_PASS"] != "p" || env["GOTRUE_SMTP_ADMIN_EMAIL"] != "ops@example.test" {
		t.Fatalf("env: %v", env)
	}
	if _, ok := (Mail{SMTPHost: "h", SMTPFrom: "a@b.test"}).GoTrueEnv()["GOTRUE_SMTP_USER"]; ok {
		t.Fatal("no user, no credentials")
	}
	t.Setenv("SBCTL_MAIL_SMTP_HOST", "relay.example.test")
	t.Setenv("SBCTL_MAIL_SMTP_FROM", "x@example.test")
	t.Setenv("SBCTL_CONFIG", "/nonexistent/sbctl.toml")
	c, err := Load("")
	if err != nil || !c.Mail.Enabled() || c.Mail.SMTPHost != "relay.example.test" {
		t.Fatalf("env override: %+v %v", c.Mail, err)
	}
}
