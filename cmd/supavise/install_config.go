package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/jsmillerdev/supavise/internal/config"
)

// installOptions are the flags of `supavise install`. A field is applied to the config only
// when its flag was given, so re-running the installer with no flags changes nothing and
// re-running it with one flag changes that setting only.
type installOptions struct {
	Domain         string
	PublicIP       string
	Email          string
	TLS            string
	DNSProvider    string
	DNSCredentials []string
	DNSCredFile    string
	Region         string

	S3Bucket          string
	S3Prefix          string
	S3Region          string
	S3Endpoint        string
	S3PathStyle       bool
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3CredFile        string

	StudioURL    string
	StudioSHA256 string
	NoStudio     bool
	NoFunctions  bool

	// KeyPassphraseFile holds the passphrase that protects an encrypted copy of the master key
	// in the backup backend. KeyEscrowed is set by the installer when the backend holds such a
	// copy at the end of the run, so the summary knows whether to remind the operator.
	KeyPassphraseFile string
	KeyEscrowed       bool

	// Fresh is set by the caller when no config.toml existed: defaults that differ between a new
	// install and config.Default() apply only then, so a re-run keeps what the operator chose.
	Fresh bool

	// AutoUpgrade, MaintenanceWindow, NoOSUpdates and OSReboot set the [update] section.
	AutoUpgrade       bool
	MaintenanceWindow string
	NoOSUpdates       bool
	OSReboot          string

	Sets           []string
	ClaimTTL       time.Duration
	ClaimTokenFile string
	Firewall       string
	SkipOSCheck    bool
}

// applyInstall puts the options whose flags were given (changed) into cfg.
func applyInstall(cfg *config.Config, o installOptions, changed func(string) bool) error {
	if changed("domain") {
		cfg.Domain = strings.ToLower(strings.Trim(strings.TrimSpace(o.Domain), "."))
		if cfg.Domain != "" && !domainRe.MatchString(cfg.Domain) {
			return fmt.Errorf("--domain %q is not a domain name", o.Domain)
		}
	}
	if changed("public-ip") {
		if o.PublicIP != "" {
			if _, err := netip.ParseAddr(o.PublicIP); err != nil {
				return fmt.Errorf("--public-ip %q is not an IP address", o.PublicIP)
			}
		}
		cfg.PublicIP = o.PublicIP
	}
	if changed("email") {
		cfg.TLS.Email = o.Email
	}
	if changed("tls") {
		cfg.TLS.Mode = o.TLS
	}
	if changed("dns") {
		switch o.DNSProvider {
		case "", "route53", "cloudflare", "hetzner", "digitalocean":
			if o.DNSProvider != cfg.TLS.DNSProvider {
				// The old provider's secrets mean nothing to the new one, and a stale key of the
				// same name could be sent to it: start from the credentials given now.
				cfg.TLS.Credentials = nil
			}
			cfg.TLS.DNSProvider = o.DNSProvider
		default:
			return fmt.Errorf("--dns %q: want route53, cloudflare, hetzner or digitalocean", o.DNSProvider)
		}
	}
	creds := map[string]string{}
	if o.DNSCredFile != "" {
		m, err := readKeyValueFile(o.DNSCredFile)
		if err != nil {
			return err
		}
		for k, v := range m {
			creds[k] = v
		}
	}
	for _, kv := range o.DNSCredentials {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("--dns-credential %q: want KEY=VALUE", kv)
		}
		creds[strings.ToLower(k)] = v
	}
	if len(creds) > 0 {
		if cfg.TLS.Credentials == nil {
			cfg.TLS.Credentials = map[string]string{}
		}
		for k, v := range creds {
			cfg.TLS.Credentials[k] = v
		}
	}
	if changed("region") {
		cfg.Region = o.Region
	}

	if changed("s3-prefix") && !changed("s3-bucket") {
		return errors.New("--s3-prefix needs --s3-bucket")
	}
	if changed("s3-bucket") {
		if o.S3Bucket == "" {
			cfg.Backup.Backend = "file://" + config.DefaultStateDir + "/backups"
		} else {
			cfg.Backup.Backend = "s3://" + o.S3Bucket
			if p := strings.Trim(o.S3Prefix, "/"); p != "" {
				cfg.Backup.Backend += "/" + p
			}
		}
	}
	if changed("s3-region") {
		cfg.Backup.S3Region = o.S3Region
	}
	if changed("s3-endpoint") {
		cfg.Backup.S3Endpoint = o.S3Endpoint
	}
	if changed("s3-path-style") {
		cfg.Backup.S3ForcePathStyle = o.S3PathStyle
	}
	akid, secret := o.S3AccessKeyID, o.S3SecretAccessKey
	setS3Creds := changed("s3-access-key-id") || changed("s3-secret-access-key")
	if o.S3CredFile != "" {
		m, err := readKeyValueFile(o.S3CredFile)
		if err != nil {
			return err
		}
		if akid, secret = m["access_key_id"], m["secret_access_key"]; akid == "" || secret == "" {
			return fmt.Errorf("%s: want access_key_id=... and secret_access_key=... lines", o.S3CredFile)
		}
		setS3Creds = true
	}
	if setS3Creds {
		if (akid == "") != (secret == "") {
			return errors.New("the S3 access key id and secret access key go together")
		}
		cfg.Backup.S3AccessKeyID, cfg.Backup.S3SecretAccessKey = akid, secret
	}

	if changed("studio-url") {
		cfg.Studio.ArtifactURL = o.StudioURL
	}
	if changed("studio-sha256") {
		cfg.Studio.ArtifactSHA256 = strings.ToLower(o.StudioSHA256)
	}
	if o.NoStudio {
		cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = "", ""
	}
	// Edge Functions are on for a new install, as on hosted Supabase; --no-functions turns them
	// off, and a re-run without either keeps the value in config.toml.
	if o.NoFunctions {
		cfg.Functions.Enabled = false
	} else if o.Fresh {
		cfg.Functions.Enabled = true
	}
	if changed("auto-upgrade") {
		cfg.Update.Mode = config.UpdateNotify
		if o.AutoUpgrade {
			cfg.Update.Mode = config.UpdateAuto
		}
	}
	if changed("maintenance-window") {
		if _, err := config.ParseWindow(o.MaintenanceWindow); err != nil {
			return fmt.Errorf("--maintenance-window: %w", err)
		}
		cfg.Update.Window = o.MaintenanceWindow
	}
	if changed("os-reboot") {
		cfg.Update.OSReboot = o.OSReboot
	}
	// Unattended OS security updates are on for a new install, as a hosted platform patches its
	// hosts; --no-os-updates opts out, and a re-run without it keeps the value in config.toml, so
	// a node installed before this setting existed is not patched behind its operator's back.
	if o.NoOSUpdates {
		cfg.Update.OSSecurityUpdates = false
	} else if o.Fresh {
		cfg.Update.OSSecurityUpdates = true
	}
	for _, kv := range o.Sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("--set %q: want key=value (a config.toml path such as ports.project_base=38000)", kv)
		}
		if err := setConfigKey(cfg, k, v); err != nil {
			return fmt.Errorf("--set %s: %w", k, err)
		}
	}
	if (cfg.Studio.ArtifactURL == "") != (cfg.Studio.ArtifactSHA256 == "") {
		return errors.New("studio artifact_url and artifact_sha256 must be set together (--studio-url and --studio-sha256)")
	}
	if cfg.Backup.S3Region != "" && !strings.HasPrefix(cfg.Backup.Backend, "s3://") {
		return errors.New("--s3-region without --s3-bucket")
	}
	return cfg.Validate()
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// readKeyValueFile reads KEY=VALUE lines (blank lines and # comments skipped). Keys are
// lower-cased.
func readKeyValueFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		m[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return m, sc.Err()
}

// configMap is cfg as the generic TOML tree (zero values included).
func configMap(cfg *config.Config) (map[string]any, error) {
	b, err := toml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, toml.Unmarshal(b, &m)
}

// setConfigKey sets one config.toml path ("ports.project_base", "tls.credentials.api_token")
// on cfg, converting value to the type of the existing setting.
func setConfigKey(cfg *config.Config, path, value string) error {
	m, err := configMap(cfg)
	if err != nil {
		return err
	}
	parts := strings.Split(path, ".")
	cur := m
	for i, p := range parts {
		last := i == len(parts)-1
		old, ok := cur[p]
		if !ok {
			if i > 0 && parts[i-1] == "credentials" { // free-form map: any key
				cur[p] = value
				break
			}
			return fmt.Errorf("unknown config key %q", path)
		}
		if last {
			switch old.(type) {
			case string:
				cur[p] = value
			case int64:
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return fmt.Errorf("%q is not a number", value)
				}
				cur[p] = n
			case bool:
				b, err := strconv.ParseBool(value)
				if err != nil {
					return fmt.Errorf("%q is not true or false", value)
				}
				cur[p] = b
			default:
				return fmt.Errorf("%q is a table, set one of its keys", path)
			}
			break
		}
		next, ok := old.(map[string]any)
		if !ok {
			return fmt.Errorf("unknown config key %q", path)
		}
		cur = next
	}
	b, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return toml.Unmarshal(b, cfg)
}

// renderConfig writes cfg as config.toml text holding only the settings that differ
// from the built-in defaults, so later releases' default changes still apply.
func renderConfig(cfg *config.Config) ([]byte, error) {
	m, err := configMap(cfg)
	if err != nil {
		return nil, err
	}
	d, err := configMap(config.Default())
	if err != nil {
		return nil, err
	}
	diffMap(m, d)
	b, err := toml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("# Written by `supavise install`; edit freely. Only settings that differ from the defaults are listed.\n")
	out.WriteString("# Every setting can also come from an SUPAVISE_* environment variable (see internal/config).\n")
	out.Write(b)
	return out.Bytes(), nil
}

// diffMap deletes from m every entry equal to the one in def.
func diffMap(m, def map[string]any) {
	for k, v := range m {
		dv, ok := def[k]
		if !ok {
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			if dsub, ok := dv.(map[string]any); ok {
				diffMap(sub, dsub)
				if len(sub) == 0 {
					delete(m, k)
				}
				continue
			}
		}
		if reflect.DeepEqual(v, dv) {
			delete(m, k)
		}
	}
}

// readConfigFile reads path into a Config over the defaults, without environment
// overrides: the installer edits the file, not the shell's view of it.
func readConfigFile(path string) (*config.Config, bool, error) {
	cfg := config.Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := toml.Unmarshal(b, cfg); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// ---- host checks ------------------------------------------------------------

// parseOSRelease reads /etc/os-release.
func parseOSRelease(b []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" && !strings.HasPrefix(k, "#") {
			m[k] = strings.Trim(v, `"'`)
		}
	}
	return m
}

// checkOS accepts Ubuntu 24.04 and later and Debian 12 and later. The service artifacts need
// glibc 2.35, but the floor is higher because the polkit rule that lets the supavise user manage
// its units is JavaScript, which polkit 121 and later read (Debian 12 and Ubuntu 23.04 and
// later). Ubuntu 22.04 ships polkit 0.105, which ignores .rules files; a .pkla grant could not
// be limited to supavise-* units, so it would let the supavise user start any unit as root.
func checkOS(rel map[string]string) error {
	id, ver := rel["ID"], rel["VERSION_ID"]
	major, err := strconv.Atoi(strings.SplitN(ver, ".", 2)[0])
	if err != nil {
		return fmt.Errorf("cannot read the version of %q (VERSION_ID %q)", rel["PRETTY_NAME"], ver)
	}
	switch id {
	case "ubuntu":
		if major < 24 {
			return fmt.Errorf("Ubuntu %s is too old: 24.04 or later is required (22.04 ships a polkit that ignores the rule Supavise needs)", ver)
		}
		return nil
	case "debian":
		if major < 12 {
			return fmt.Errorf("Debian %s is too old: 12 or later is required", ver)
		}
		return nil
	}
	return fmt.Errorf("%q is not supported: Ubuntu 24.04+ or Debian 12+ is required (--skip-os-check tries anyway if its glibc is 2.35 or newer)", rel["PRETTY_NAME"])
}

var glibcRe = regexp.MustCompile(`(\d+)\.(\d+)\s*$`)

// checkGlibc parses the first line of `ldd --version` and requires 2.35 or later.
func checkGlibc(lddOutput string) error {
	first, _, _ := strings.Cut(lddOutput, "\n")
	m := glibcRe.FindStringSubmatch(first)
	if m == nil {
		return fmt.Errorf("cannot read the glibc version from %q", first)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if maj < 2 || (maj == 2 && min < 35) {
		return fmt.Errorf("glibc %s.%s is too old: 2.35 or later is required (the service artifacts need it)", m[1], m[2])
	}
	if !strings.Contains(strings.ToLower(first), "glibc") && !strings.Contains(strings.ToLower(first), "gnu libc") {
		return fmt.Errorf("this is not a glibc system (%q)", first)
	}
	return nil
}

// detectPublicIP asks the EC2 instance metadata service (IMDSv2), then checkip.amazonaws.com.
func detectPublicIP(ctx context.Context) (string, error) {
	hc := &http.Client{Timeout: 3 * time.Second}
	if ip := imdsPublicIP(ctx, hc, "http://169.254.169.254"); ip != "" {
		return ip, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot detect the public IP (%v); pass --public-ip", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(b))
	if _, err := netip.ParseAddr(ip); err != nil {
		return "", fmt.Errorf("cannot detect the public IP (got %q); pass --public-ip", ip)
	}
	return ip, nil
}

func imdsPublicIP(ctx context.Context, hc *http.Client, base string) string {
	short, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(short, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	req, err = http.NewRequestWithContext(short, http.MethodGet, base+"/latest/meta-data/public-ipv4", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if resp.StatusCode != 200 {
		return ""
	}
	if _, err := netip.ParseAddr(ip); err != nil {
		return ""
	}
	return ip
}

// dnsRecords lists the records a domain install needs, for the summary.
func dnsRecords(cfg *config.Config, ip string) []string {
	if cfg.Domain == "" {
		return nil
	}
	if ip == "" {
		ip = "<this server's public IP>"
	}
	typ := "A"
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() {
		typ = "AAAA"
	}
	return []string{
		fmt.Sprintf("%-28s %s  %s", cfg.APIHost(), typ, ip),
		fmt.Sprintf("%-28s %s  %s", cfg.StudioHost(), typ, ip),
		fmt.Sprintf("%-28s %s  %s", cfg.PoolerHost(), typ, ip),
		fmt.Sprintf("%-28s %s  %s", "*.api."+cfg.BaseDomain(), typ, ip),
	}
}
