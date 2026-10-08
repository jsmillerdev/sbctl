package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/config"
)

func changedSet(names ...string) func(string) bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return func(n string) bool { return m[n] }
}

func TestApplyInstallOnlyTouchesGivenFlags(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "old.example.com"
	cfg.TLS.Email = "ops@example.com"
	if err := applyInstall(cfg, installOptions{Email: "new@example.com", Domain: "ignored.example.com"}, changedSet("email")); err != nil {
		t.Fatal(err)
	}
	if cfg.Domain != "old.example.com" || cfg.TLS.Email != "new@example.com" {
		t.Fatalf("domain %q email %q", cfg.Domain, cfg.TLS.Email)
	}
}

func TestApplyInstallDomainAndDNS(t *testing.T) {
	cfg := config.Default()
	o := installOptions{Domain: " Example.COM. ", DNSProvider: "cloudflare", DNSCredentials: []string{"API_TOKEN=abc", "zone=z1"}, Region: "eu-west-1"}
	if err := applyInstall(cfg, o, changedSet("domain", "dns", "region")); err != nil {
		t.Fatal(err)
	}
	if cfg.Domain != "example.com" || cfg.TLS.DNSProvider != "cloudflare" || cfg.Region != "eu-west-1" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.TLS.Credentials["api_token"] != "abc" || cfg.TLS.Credentials["zone"] != "z1" {
		t.Fatalf("credentials %v", cfg.TLS.Credentials)
	}
	for _, bad := range []installOptions{
		{Domain: "not a domain"}, {Domain: "localhost"}, {Domain: "-x.example.com"},
	} {
		if err := applyInstall(config.Default(), bad, changedSet("domain")); err == nil {
			t.Errorf("domain %q accepted", bad.Domain)
		}
	}
	if err := applyInstall(config.Default(), installOptions{DNSProvider: "godaddy"}, changedSet("dns")); err == nil {
		t.Error("unknown DNS provider accepted")
	}
	if err := applyInstall(config.Default(), installOptions{DNSCredentials: []string{"nokey"}}, changedSet()); err == nil {
		t.Error("credential without = accepted")
	}
	if err := applyInstall(config.Default(), installOptions{PublicIP: "999.1.1.1"}, changedSet("public-ip")); err == nil {
		t.Error("bad IP accepted")
	}
	if err := applyInstall(config.Default(), installOptions{Region: "mars-1"}, changedSet("region")); err == nil {
		t.Error("unknown region accepted")
	}
}

func TestApplyInstallDNSProviderSwitchReplacesCredentials(t *testing.T) {
	cfg := config.Default()
	first := installOptions{DNSProvider: "cloudflare", DNSCredentials: []string{"api_token=cf", "zone=z1"}}
	if err := applyInstall(cfg, first, changedSet("dns")); err != nil {
		t.Fatal(err)
	}
	second := installOptions{DNSProvider: "route53", DNSCredentials: []string{"access_key_id=AKIA"}}
	if err := applyInstall(cfg, second, changedSet("dns")); err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.DNSProvider != "route53" || len(cfg.TLS.Credentials) != 1 || cfg.TLS.Credentials["access_key_id"] != "AKIA" {
		t.Fatalf("%q %v", cfg.TLS.DNSProvider, cfg.TLS.Credentials)
	}
	// Switching with no new credentials (instance role) leaves none behind.
	if err := applyInstall(cfg, installOptions{DNSProvider: "hetzner"}, changedSet("dns")); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TLS.Credentials) != 0 {
		t.Fatalf("stale credentials: %v", cfg.TLS.Credentials)
	}
	// Re-running with the same provider keeps what is there and merges new keys.
	_ = applyInstall(cfg, installOptions{DNSProvider: "hetzner", DNSCredentials: []string{"token=a"}}, changedSet("dns"))
	if err := applyInstall(cfg, installOptions{DNSProvider: "hetzner", DNSCredentials: []string{"extra=b"}}, changedSet("dns")); err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.Credentials["token"] != "a" || cfg.TLS.Credentials["extra"] != "b" {
		t.Fatalf("%v", cfg.TLS.Credentials)
	}
}

func TestWriteSecretFileTightensAnExistingMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	if err := os.WriteFile(p, []byte("old and longer content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(p, []byte("tok\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "tok\n" {
		t.Fatalf("content %q", b)
	}
}

func TestApplyInstallS3(t *testing.T) {
	cfg := config.Default()
	o := installOptions{S3Bucket: "my-bucket", S3Prefix: "/nodes/a/", S3Region: "eu-central-1"}
	if err := applyInstall(cfg, o, changedSet("s3-bucket", "s3-prefix", "s3-region")); err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Backend != "s3://my-bucket/nodes/a" || cfg.Backup.S3Region != "eu-central-1" || cfg.Backup.S3AccessKeyID != "" {
		t.Fatalf("%+v", cfg.Backup)
	}
	if err := applyInstall(config.Default(), installOptions{S3Prefix: "x"}, changedSet("s3-prefix")); err == nil {
		t.Error("prefix without bucket accepted")
	}
	if err := applyInstall(config.Default(), installOptions{S3AccessKeyID: "AK"}, changedSet("s3-access-key-id")); err == nil {
		t.Error("half of a key pair accepted")
	}
	if err := applyInstall(config.Default(), installOptions{S3Region: "eu-west-1"}, changedSet("s3-region")); err == nil {
		t.Error("s3 region on the local backend accepted")
	}
	f := filepath.Join(t.TempDir(), "s3")
	if err := os.WriteFile(f, []byte("# keys\naccess_key_id = AKIA\nsecret_access_key=\"s3cr3t\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = config.Default()
	if err := applyInstall(cfg, installOptions{S3Bucket: "b", S3CredFile: f}, changedSet("s3-bucket")); err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.S3AccessKeyID != "AKIA" || cfg.Backup.S3SecretAccessKey != "s3cr3t" {
		t.Fatalf("%+v", cfg.Backup)
	}
}

func TestApplyInstallStudio(t *testing.T) {
	cfg := config.Default()
	sum := strings.Repeat("AB", 32)
	if err := applyInstall(cfg, installOptions{StudioURL: "https://example.com/s.tar.zst", StudioSHA256: sum}, changedSet("studio-url", "studio-sha256")); err != nil {
		t.Fatal(err)
	}
	if cfg.Studio.ArtifactSHA256 != strings.ToLower(sum) {
		t.Fatal("sha not lower-cased")
	}
	if err := applyInstall(config.Default(), installOptions{StudioURL: "https://x"}, changedSet("studio-url")); err == nil {
		t.Error("url without sha accepted")
	}
	if err := applyInstall(cfg, installOptions{NoStudio: true}, changedSet()); err != nil || cfg.Studio.ArtifactURL != "" {
		t.Fatalf("--no-studio: %v %+v", err, cfg.Studio)
	}
}

func TestSetConfigKey(t *testing.T) {
	cfg := config.Default()
	for _, kv := range [][2]string{
		{"ports.project_base", "38000"},
		{"log_level", "debug"},
		{"backup.s3_force_path_style", "true"},
		{"tls.credentials.api_token", "tok"},
		{"listen.http", "127.0.0.1:38080"},
	} {
		if err := setConfigKey(cfg, kv[0], kv[1]); err != nil {
			t.Fatalf("%s: %v", kv[0], err)
		}
	}
	if cfg.Ports.ProjectBase != 38000 || cfg.LogLevel != "debug" || !cfg.Backup.S3ForcePathStyle ||
		cfg.TLS.Credentials["api_token"] != "tok" || cfg.Listen.HTTP != "127.0.0.1:38080" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.Ports.Studio != config.PortStudio {
		t.Fatal("an unrelated setting changed")
	}
	for _, bad := range [][2]string{{"nope", "1"}, {"ports.nope", "1"}, {"ports.project_base", "abc"}, {"ports", "1"}, {"backup.s3_force_path_style", "maybe"}, {"log_level.x", "1"}} {
		if err := setConfigKey(config.Default(), bad[0], bad[1]); err == nil {
			t.Errorf("%s=%s accepted", bad[0], bad[1])
		}
	}
}

func TestRenderConfigListsOnlyDifferences(t *testing.T) {
	cfg := config.Default()
	b, err := renderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "[") || strings.Contains(string(b), "=") {
		t.Fatalf("default config should list nothing:\n%s", b)
	}
	cfg.Domain = "example.com"
	cfg.TLS.DNSProvider = "route53"
	cfg.TLS.Credentials["hosted_zone_id"] = "Z1"
	cfg.Backup.Backend = "s3://b/p"
	cfg.Ports.ProjectBase = 38000
	b, err = renderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{`domain = 'example.com'`, `dns_provider = 'route53'`, `hosted_zone_id = 'Z1'`, `backend = 's3://b/p'`, `project_base = 38000`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"supervisor", "retention_days", "supavisor_session", "log_level"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("default %q written:\n%s", unwanted, text)
		}
	}
	// The file loads back to the same node settings.
	back := config.Default()
	if err := toml.Unmarshal(b, back); err != nil {
		t.Fatal(err)
	}
	if back.Domain != cfg.Domain || back.Ports.ProjectBase != 38000 || back.TLS.Credentials["hosted_zone_id"] != "Z1" || back.Backup.RetentionDays != 7 {
		t.Fatalf("%+v", back)
	}
}

func TestReadConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	cfg, existed, err := readConfigFile(p)
	if err != nil || existed || cfg.Supervisor != config.SupervisorSystemd {
		t.Fatalf("missing file: %v %v", existed, err)
	}
	if err := os.WriteFile(p, []byte("domain = 'a.example.com'\n[tls]\nemail = 'x@example.com'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, existed, err = readConfigFile(p)
	if err != nil || !existed || cfg.Domain != "a.example.com" || cfg.TLS.Email != "x@example.com" || cfg.TLS.Mode != "auto" {
		t.Fatalf("%+v %v %v", cfg, existed, err)
	}
	t.Setenv("SUPAVISE_DOMAIN", "from-env.example.com")
	if cfg, _, _ = readConfigFile(p); cfg.Domain != "a.example.com" {
		t.Fatal("the environment leaked into the file the installer rewrites")
	}
	if err := os.WriteFile(p, []byte("domain = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readConfigFile(p); err == nil {
		t.Fatal("broken file accepted")
	}
}

func TestCheckOS(t *testing.T) {
	for _, tc := range []struct {
		content string
		ok      bool
	}{
		{"ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"", true},
		{"ID=ubuntu\nVERSION_ID=\"22.04\"", false},
		{"ID=ubuntu\nVERSION_ID=\"25.10\"", true},
		{"ID=ubuntu\nVERSION_ID=\"20.04\"", false},
		{"ID=debian\nVERSION_ID=\"12\"", true},
		{"ID=debian\nVERSION_ID=\"11\"", false},
		{"ID=fedora\nVERSION_ID=\"40\"", false},
		{"ID=ubuntu", false},
	} {
		if err := checkOS(parseOSRelease([]byte(tc.content))); (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.content, err)
		}
	}
}

func TestCheckGlibc(t *testing.T) {
	for _, tc := range []struct {
		out string
		ok  bool
	}{
		{"ldd (Ubuntu GLIBC 2.39-0ubuntu8.3) 2.39\nCopyright", true},
		{"ldd (Ubuntu GLIBC 2.35-0ubuntu3.8) 2.35", true},
		{"ldd (Debian GLIBC 2.36-9+deb12u7) 2.36", true},
		{"ldd (Debian GLIBC 2.31-13+deb11u5) 2.31", false},
		{"ldd (GNU libc) 2.5", false},
		{"musl libc (x86_64)\nVersion 1.2.4", false},
		{"", false},
	} {
		if err := checkGlibc(tc.out); (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.out, err)
		}
	}
}

func TestIMDSPublicIP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			http.Error(w, "no ttl", 400)
			return
		}
		_, _ = w.Write([]byte("TOK"))
	})
	mux.HandleFunc("GET /latest/meta-data/public-ipv4", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-aws-ec2-metadata-token") != "TOK" {
			http.Error(w, "no token", 401)
			return
		}
		_, _ = w.Write([]byte("203.0.113.9\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if ip := imdsPublicIP(context.Background(), srv.Client(), srv.URL); ip != "203.0.113.9" {
		t.Fatalf("ip %q", ip)
	}
	if ip := imdsPublicIP(context.Background(), srv.Client(), "http://127.0.0.1:1"); ip != "" {
		t.Fatalf("unreachable IMDS gave %q", ip)
	}
}

func TestDNSRecordsAndPorts(t *testing.T) {
	cfg := config.Default()
	if dnsRecords(cfg, "1.2.3.4") != nil {
		t.Fatal("sslip.io needs no records")
	}
	cfg.Domain = "example.com"
	recs := strings.Join(dnsRecords(cfg, "1.2.3.4"), "\n")
	for _, want := range []string{"api.example.com", "studio.example.com", "pooler.example.com", "*.api.example.com", "1.2.3.4"} {
		if !strings.Contains(recs, want) {
			t.Errorf("records lack %q:\n%s", want, recs)
		}
	}
	if got := publicPorts(cfg); len(got) != 4 || got[0] != 5432 || got[1] != 6543 || got[2] != 80 || got[3] != 443 {
		t.Fatalf("ports %v", got)
	}
}

func TestCheckPolkitVersion(t *testing.T) {
	for _, tc := range []struct {
		out string
		ok  bool
	}{
		{"pkaction version 124\n", true},
		{"pkaction version 122", true},
		{"pkaction version 121", true},
		{"pkaction version 0.105\n", false},
		{"pkaction version 120", false},
		{"", false},
		{"polkit unknown", false},
	} {
		if err := checkPolkitVersion(tc.out); (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.out, err)
		}
	}
}

func TestExeStale(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "supavise")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hard link stands in for /proc/<pid>/exe: it keeps the inode the process runs.
	running := filepath.Join(dir, "exe")
	if err := os.Link(bin, running); err != nil {
		t.Fatal(err)
	}
	if exeStale(running, bin) {
		t.Fatal("the same file reads as stale")
	}
	// install.sh replaces the binary by rename: the path gets a new inode.
	next := filepath.Join(dir, "supavise.new")
	if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, bin); err != nil {
		t.Fatal(err)
	}
	if !exeStale(running, bin) {
		t.Fatal("a daemon on the replaced inode does not read as stale")
	}
	// A re-run of the installer with the same release replaces the file with identical bytes
	// (same size, new inode): the daemon is not stale.
	same := filepath.Join(dir, "supavise.same")
	if err := os.WriteFile(same, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(same, bin); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(running); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(bin, running); err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(dir, "supavise.again")
	if err := os.WriteFile(again, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(again, bin); err != nil {
		t.Fatal(err)
	}
	if exeStale(running, bin) {
		t.Fatal("identical bytes under a new inode read as stale: every re-run would restart the daemon")
	}
	// Same size, different bytes.
	other := filepath.Join(dir, "supavise.other")
	if err := os.WriteFile(other, []byte("nex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, bin); err != nil {
		t.Fatal(err)
	}
	if !exeStale(running, bin) {
		t.Fatal("same size, different bytes did not read as stale")
	}
	if exeStale(filepath.Join(dir, "missing"), bin) || exeStale(running, filepath.Join(dir, "missing")) {
		t.Fatal("an unreadable side must not force a restart")
	}
}

func TestSummaryKeepsTheTokenOutOfTheLogWhenItGoesToAFile(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"
	const tok = "sbc_0123456789abcdef"
	var withFile, without bytes.Buffer
	printSummary(&withFile, cfg, "203.0.113.7", tok, false, installOptions{ClaimTokenFile: "/root/claim-token"})
	printSummary(&without, cfg, "203.0.113.7", tok, false, installOptions{})
	if strings.Contains(withFile.String(), tok) || !strings.Contains(withFile.String(), "/root/claim-token") {
		t.Fatalf("summary with --claim-token-file:\n%s", withFile.String())
	}
	if !strings.Contains(without.String(), tok) {
		t.Fatalf("summary without a file must show the token:\n%s", without.String())
	}
}

// A fake ufw on PATH: `ufw status` answers with the given first line.
func fakeUFW(t *testing.T, status string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then echo '" + status + "'; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ufw"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// With ufw installed but off (the cloud-image default) and no --firewall flag, the installer
// stops and says what to choose: carrying on would leave Supavisor's API and shard listeners
// open on every interface.
func TestFirewallAutoStopsWhenUFWIsInactive(t *testing.T) {
	fakeUFW(t, "Status: inactive")
	var out, errb bytes.Buffer
	in := &installer{ctx: context.Background(), out: &out, err: &errb, cfg: config.Default()}
	err := in.firewall("auto", false)
	if err == nil || !strings.Contains(err.Error(), "--firewall ufw") || !strings.Contains(err.Error(), "--firewall none") {
		t.Fatalf("auto with ufw inactive = %v; want an error naming both choices", err)
	}
	// The explicit choice that leaves the host alone still works.
	if err := in.firewall("none", false); err != nil {
		t.Fatalf("--firewall none: %v", err)
	}
	// A re-run of an installed node warns: the choice was made at the first install.
	if err := in.firewall("auto", true); err != nil || !strings.Contains(out.String(), "WARNING") {
		t.Fatalf("re-run with ufw inactive: %v, output %q", err, out.String())
	}
}

// With ufw active, auto opens the public ports.
func TestFirewallAutoWithActiveUFWOpensThePublicPorts(t *testing.T) {
	fakeUFW(t, "Status: active")
	var out, errb bytes.Buffer
	in := &installer{ctx: context.Background(), out: &out, err: &errb, cfg: config.Default()}
	if err := in.firewall("auto", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "opening TCP") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestInstallTurnsFunctionsOnForANewNode(t *testing.T) {
	cfg := config.Default()
	if err := applyInstall(cfg, installOptions{Fresh: true}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if !cfg.Functions.Enabled {
		t.Fatal("a new install must turn Edge Functions on")
	}
	// A re-run keeps the operator's choice: off stays off without --no-functions.
	cfg.Functions.Enabled = false
	if err := applyInstall(cfg, installOptions{}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if cfg.Functions.Enabled {
		t.Fatal("a re-run turned Edge Functions back on")
	}
	cfg = config.Default()
	if err := applyInstall(cfg, installOptions{Fresh: true, NoFunctions: true}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if cfg.Functions.Enabled {
		t.Fatal("--no-functions left Edge Functions on")
	}
}
