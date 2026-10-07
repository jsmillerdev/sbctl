// Package config loads /etc/supavise/config.toml with SUPAVISE_* environment overrides
// and defines the fixed filesystem, port and hostname layout every package shares.
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	DefaultPath       = "/etc/supavise/config.toml"
	DefaultKeyPath    = "/etc/supavise/master.key"
	DefaultStateDir   = "/var/lib/supavise"
	DefaultBinPath    = "/usr/local/bin/supavise"
	DefaultRegion     = "us-east-1"
	EnvPrefix         = "SUPAVISE_"
	EnvConfigPath     = "SUPAVISE_CONFIG"
	SupervisorSystemd = "systemd"
	SupervisorExec    = "exec"
)

// Config is the whole node configuration. Every field can be overridden by an
// environment variable named SUPAVISE_ plus the upper-cased TOML path joined by "_",
// for example SUPAVISE_TLS_DNS_PROVIDER or SUPAVISE_BACKUP_S3_ENDPOINT. Map fields take
// one variable per key: SUPAVISE_TLS_CREDENTIALS_CF_API_TOKEN sets credentials["cf_api_token"].
type Config struct {
	// Domain is the base domain. Empty means "<public_ip>.sslip.io".
	Domain   string `toml:"domain"`
	PublicIP string `toml:"public_ip"`
	StateDir string `toml:"state_dir"`
	KeyPath  string `toml:"key_path"`
	// BinPath is the absolute path of the supavise binary, used in archive_command.
	BinPath string `toml:"bin_path"`
	// Supervisor is "systemd" (production) or "exec" (development and tests: child processes).
	Supervisor string `toml:"supervisor"`
	// Platform selects the artifact build, for example "linux-amd64". Empty means the running platform.
	Platform string `toml:"platform"`
	LogLevel string `toml:"log_level"`
	// Region is the AWS region code shown for every project (Studio and the Supabase CLI
	// resolve it against a list of real regions, so a free-form label breaks the project
	// list). It is only a label here: nothing is placed by it. Default "us-east-1".
	Region string `toml:"region"`

	Listen    Listen    `toml:"listen"`
	Ports     Ports     `toml:"ports"`
	TLS       TLS       `toml:"tls"`
	Backup    Backup    `toml:"backup"`
	Artifacts Artifacts `toml:"artifacts"`
	Studio    Studio    `toml:"studio"`
	API       API       `toml:"api"`
	Mail      Mail      `toml:"mail"`
	Fleet     Fleet     `toml:"fleet"`
	Functions Functions `toml:"functions"`
	Branching Branching `toml:"branching"`
	Alerts    Alerts    `toml:"alerts"`
	Health    Health    `toml:"health"`
	Compute   Compute   `toml:"compute"`
	// Defaults are the systemd limits of the shared services and the system project. A user
	// project takes the limits of its size (internal/lifecycle, sizes.go) instead.
	Defaults Limits `toml:"defaults"`
}

type Listen struct {
	HTTP  string `toml:"http"`  // public, default ":80"
	HTTPS string `toml:"https"` // public, default ":443"
	Admin string `toml:"admin"` // loopback, default "127.0.0.1:7000"
}

type TLS struct {
	// Mode is "auto" (DNS-01 if dns_provider is set, else HTTP-01 per host), "dns01", "http01" or "off".
	Mode        string            `toml:"mode"`
	Email       string            `toml:"email"`
	DNSProvider string            `toml:"dns_provider"` // route53 | cloudflare | hetzner | digitalocean
	CA          string            `toml:"ca"`           // ACME directory URL; empty = Let's Encrypt production
	CACert      string            `toml:"ca_cert"`      // PEM file of the root that signs the ACME directory's own TLS certificate (private CAs, Pebble)
	Credentials map[string]string `toml:"credentials"`  // provider-specific, e.g. api_token
}

type Backup struct {
	// Backend is "file:///var/lib/supavise/backups" or "s3://bucket/prefix".
	Backend          string `toml:"backend"`
	S3Endpoint       string `toml:"s3_endpoint"` // empty = AWS
	S3Region         string `toml:"s3_region"`
	S3ForcePathStyle bool   `toml:"s3_force_path_style"`
	RetentionDays    int    `toml:"retention_days"`
	// BaseBackupOnCalendar is a systemd OnCalendar expression for the nightly base backup timer.
	BaseBackupOnCalendar string `toml:"base_backup_on_calendar"`
	// S3AccessKeyID and S3SecretAccessKey are optional static credentials for the s3
	// backend. When empty the AWS default chain applies (environment, shared config,
	// instance role), which is what the CloudFormation install uses. The secret key sits in
	// config.toml in plain text: with it set, config.toml must be mode 0600 and owned by
	// the supavise user (supavise backups warns otherwise).
	S3AccessKeyID     string `toml:"s3_access_key_id"`
	S3SecretAccessKey string `toml:"s3_secret_access_key"`
	// ArchiveTimeoutSeconds is PostgreSQL's archive_timeout for project clusters: the
	// longest an unarchived WAL change waits before the segment is switched and pushed.
	// Zero means backup.DefaultArchiveTimeout (300 seconds).
	ArchiveTimeoutSeconds int `toml:"archive_timeout_seconds"`
	// WALRelay chooses how a cluster's archive_command and restore_command reach the
	// backend: "on" through the daemon, over a unix socket inside the project's own
	// directory (the Postgres unit holds no backend credentials and has no access to the
	// backups); "off" by running `supavise wal push` itself, which reads this file's backend
	// settings; "auto" (the default) is "on" under systemd, where units have a mount
	// sandbox, and "off" under the exec backend of development and tests.
	WALRelay string `toml:"wal_relay"`
}

// WALRelayEnabled reports whether clusters archive through the daemon (Backup.WALRelay).
func (c *Config) WALRelayEnabled() bool {
	switch c.Backup.WALRelay {
	case "on":
		return true
	case "off":
		return false
	}
	return c.Supervisor == SupervisorSystemd
}

type Artifacts struct {
	// BaseURL is the release download root; an asset URL is <base_url>/<tag>/<tag>-<platform>.tar.zst.
	BaseURL string `toml:"base_url"`
	// VersionsFile overrides the internal/versions/versions.yaml embedded in the binary.
	VersionsFile string `toml:"versions_file"`
	// CacheDir holds downloaded archives; defaults to <state_dir>/artifacts/.cache.
	CacheDir string `toml:"cache_dir"`
}

type Studio struct {
	// ArtifactURL points at our platform-mode Studio build (tar.zst); its SHA-256 is required.
	ArtifactURL     string `toml:"artifact_url"`
	ArtifactSHA256  string `toml:"artifact_sha256"`
	HCaptchaSiteKey string `toml:"hcaptcha_site_key"`
}

// Limits are systemd resource controls applied to every unit of a project.
type Limits struct {
	MemoryMax string `toml:"memory_max" json:"memory_max,omitempty"` // e.g. "1G"
	CPUQuota  string `toml:"cpu_quota" json:"cpu_quota,omitempty"`   // e.g. "100%"
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		StateDir:   DefaultStateDir,
		KeyPath:    DefaultKeyPath,
		BinPath:    DefaultBinPath,
		Supervisor: SupervisorSystemd,
		LogLevel:   "info",
		Region:     DefaultRegion,
		Listen:     Listen{HTTP: ":80", HTTPS: ":443", Admin: "127.0.0.1:7000"},
		Ports:      DefaultPorts(),
		TLS:        TLS{Mode: "auto", Credentials: map[string]string{}},
		Backup: Backup{
			Backend:              "file://" + DefaultStateDir + "/backups",
			RetentionDays:        7,
			BaseBackupOnCalendar: "*-*-* 03:00:00",
		},
		Artifacts: Artifacts{BaseURL: "https://github.com/supabase/slim-services/releases/download"},
		Defaults:  Limits{MemoryMax: "1G", CPUQuota: "100%"},
	}
}

// Load reads the file at path (DefaultPath, or $SUPAVISE_CONFIG, when path is empty),
// applies SUPAVISE_* overrides and validates. A missing file is not an error.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = DefaultPath
	}
	c := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if err := applyEnv(reflect.ValueOf(c).Elem(), EnvPrefix, os.Environ()); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

// Validate checks the fields other packages rely on.
func (c *Config) Validate() error {
	// Domain and PublicIP may both be empty at install time; BaseDomain returns "" until one is set.
	switch c.Supervisor {
	case SupervisorSystemd, SupervisorExec:
	default:
		return fmt.Errorf("config: supervisor must be %q or %q", SupervisorSystemd, SupervisorExec)
	}
	switch c.TLS.Mode {
	case "auto", "dns01", "http01", "off":
	default:
		return fmt.Errorf("config: tls.mode must be auto, dns01, http01 or off")
	}
	if c.TLS.Mode == "dns01" && c.TLS.DNSProvider == "" {
		return errors.New("config: tls.mode dns01 needs tls.dns_provider")
	}
	switch c.Backup.WALRelay {
	case "", "auto", "on", "off":
	default:
		return errors.New("config: backup.wal_relay must be auto, on or off")
	}
	if c.Backup.WALRelay == "off" && c.Supervisor == SupervisorSystemd {
		return errors.New("config: backup.wal_relay = \"off\" cannot work with supervisor = \"systemd\": the Postgres units hide /etc/supavise and the backups directory, so a direct `supavise wal push` can read neither the backend settings nor write the archive, and archiving would fail forever; use auto or on")
	}
	if err := c.Branching.validate(); err != nil {
		return err
	}
	if err := c.Health.validate(); err != nil {
		return err
	}
	if err := c.Compute.Validate(); err != nil {
		return err
	}
	if err := c.validateAlerts(); err != nil {
		return err
	}
	if c.StateDir == "" {
		return errors.New("config: state_dir is empty")
	}
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if !ValidRegion(c.Region) {
		return fmt.Errorf("config: region %q is not one of the regions Studio knows (%s; the list is AWS_REGIONS in Studio's packages/shared-data/regions.ts)", c.Region, strings.Join(Regions, ", "))
	}
	if err := c.Functions.Validate(); err != nil {
		return err
	}
	if c.Ports.ProjectBase < 1024 || c.MaxProjectSeq() < 1 {
		return fmt.Errorf("config: ports.project_base %d out of range", c.Ports.ProjectBase)
	}
	return nil
}

func applyEnv(v reflect.Value, prefix string, environ []string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("toml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + strings.ToUpper(tag)
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			if err := applyEnv(fv, name+"_", environ); err != nil {
				return err
			}
			continue
		case reflect.Map:
			p := name + "_"
			for _, kv := range environ {
				k, val, ok := strings.Cut(kv, "=")
				if !ok || !strings.HasPrefix(k, p) {
					continue
				}
				if fv.IsNil() {
					fv.Set(reflect.MakeMap(fv.Type()))
				}
				fv.SetMapIndex(reflect.ValueOf(strings.ToLower(strings.TrimPrefix(k, p))), reflect.ValueOf(val))
			}
			continue
		}
		val, ok := lookup(environ, name)
		if !ok {
			continue
		}
		switch fv.Kind() {
		case reflect.String:
			fv.SetString(val)
		case reflect.Bool:
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetBool(b)
		case reflect.Int, reflect.Int64:
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetInt(n)
		case reflect.Float64:
			x, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetFloat(x)
		}
	}
	return nil
}

func lookup(environ []string, name string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// Regions are the region codes Studio knows: AWS_REGIONS in packages/shared-data/regions.ts
// at the pinned Studio commit (internal/versions/versions.yaml, studio). Studio resolves a project's region
// against this table and its project list breaks on any other code, real AWS region or not
// (eu-south-1, ap-east-1 and us-gov-west-1 included). Update it when the Studio pin moves.
var Regions = []string{
	"us-west-1", "us-west-2", "us-east-1", "us-east-2", "ca-central-1",
	"eu-west-1", "eu-west-2", "eu-west-3", "eu-central-1", "eu-central-2", "eu-north-1",
	"ap-south-1", "ap-southeast-1", "ap-northeast-1", "ap-northeast-2", "ap-southeast-2",
	"sa-east-1",
}

// ValidRegion reports whether s is one of Regions.
func ValidRegion(s string) bool { return slices.Contains(Regions, s) }

// ProjectRegion maps a stored or requested project region to the label shown to
// clients: a code in Regions is kept, anything else (empty, the old "local") becomes the
// configured region.
func (c *Config) ProjectRegion(s string) string {
	if ValidRegion(s) {
		return s
	}
	if c.Region != "" {
		return c.Region
	}
	return DefaultRegion
}
