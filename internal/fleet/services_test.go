package fleet

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

func testCreds(t *testing.T, n *testNode) *creds {
	t.Helper()
	c, err := loadCreds(context.Background(), n.deps(), true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSpecsCoverEveryService(t *testing.T) {
	n := newTestNode(t)
	d := n.deps()
	d.Artifacts = allArtifacts()
	d.Supervisor = newFakeSupervisor()
	m, err := NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := m.Specs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range specs {
		names = append(names, s.Service)
		if s.Ref != "" || s.ArtifactDir != "/art/"+s.Service {
			t.Errorf("%s: ref %q artifact %q", s.Service, s.Ref, s.ArtifactDir)
		}
		if want := n.cfg.Paths().System(s.Service); s.WorkDir != want {
			t.Errorf("%s: workdir %q, want %q (the templates bind system/<svc>)", s.Service, s.WorkDir, want)
		}
		if s.Limits != n.cfg.Defaults {
			t.Errorf("%s: limits %+v", s.Service, s.Limits)
		}
		if got := s.Unit(); got != "supavise-"+s.Service+".service" {
			t.Errorf("unit %q", got)
		}
		// Every service must render a runnable launcher script.
		if _, err := units.FormatRun(s); err != nil {
			t.Errorf("%s: %v", s.Service, err)
		}
		if _, err := units.FormatEnv(s.Env); err != nil {
			t.Errorf("%s: env: %v", s.Service, err)
		}
	}
	if strings.Join(names, ",") != "pgmeta,supavisor,realtime,storage,studio" {
		t.Fatalf("order = %v", names)
	}
	// bin/prepare runs migrations first for the two Elixir services only.
	for _, s := range specs {
		want := s.Service == config.SvcSupavisor || s.Service == config.SvcRealtime
		if got := len(s.PreStart) == 1 && s.PreStart[0][0] == "bin/prepare"; got != want {
			t.Errorf("%s: prepare = %v", s.Service, s.PreStart)
		}
	}
}

func TestSpecsReportMissingArtifactAndKeepTheRest(t *testing.T) {
	n := newTestNode(t)
	d := n.deps()
	arts := allArtifacts()
	delete(arts, config.SvcStudio)
	d.Artifacts = arts
	d.Supervisor = newFakeSupervisor()
	m, _ := NewManager(d)
	specs, err := m.Specs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "studio") {
		t.Fatalf("err = %v", err)
	}
	if len(specs) != 4 {
		t.Fatalf("%d specs, want the four that can start", len(specs))
	}
	d.Skip = []string{config.SvcStudio}
	m, _ = NewManager(d)
	if _, err := m.Specs(context.Background()); err != nil {
		t.Fatalf("skipped service still reported: %v", err)
	}
}

func TestSupavisorEnv(t *testing.T) {
	n := newTestNode(t)
	n.cfg.Ports.SystemPostgres, n.cfg.Ports.SupavisorSession, n.cfg.Ports.SupavisorTransaction = 37001, 37010, 37011
	n.cfg.Fleet.SupavisorAPIPort = 37012
	c := testCreds(t, n)
	env := supavisorEnv(n.cfg, c)
	want := map[string]string{
		"DATABASE_URL":            "ecto://supavise_supavisor:pw-supavisor@127.0.0.1:37001/_supavisor",
		"PORT":                    "37012",
		"PROXY_PORT_SESSION":      "37010",
		"PROXY_PORT_TRANSACTION":  "37011",
		"PROXY_PORT":              "0",
		"SESSION_PROXY_PORTS":     "0",
		"TRANSACTION_PROXY_PORTS": "0",
		"API_JWT_SECRET":          c.supavisorAPIJWT,
		"METRICS_JWT_SECRET":      c.supavisorMetricsJWT,
		"VAULT_ENC_KEY":           c.supavisorVaultKey,
		"SECRET_KEY_BASE":         c.supavisorSecretBase,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if env["RELEASE_TMP"] != n.cfg.Paths().System("supavisor")+"/tmp" {
		t.Errorf("RELEASE_TMP = %q", env["RELEASE_TMP"])
	}
	crt, key := DownstreamCertPaths(n.cfg)
	if env["GLOBAL_DOWNSTREAM_CERT_PATH"] != crt || env["GLOBAL_DOWNSTREAM_KEY_PATH"] != key {
		t.Errorf("TLS paths = %q, %q", env["GLOBAL_DOWNSTREAM_CERT_PATH"], env["GLOBAL_DOWNSTREAM_KEY_PATH"])
	}
	if _, ok := env["SUPAVISOR_BIND_IP"]; ok {
		t.Error("SUPAVISOR_BIND_IP would make the pooler ports loopback-only")
	}
	if _, ok := env["CLUSTER_POSTGRES"]; ok {
		t.Error("CLUSTER_POSTGRES needs a region and is for multi-node setups")
	}
	if env["API_JWT_SECRET"] == env["METRICS_JWT_SECRET"] {
		t.Error("the metrics secret must not be the API secret")
	}
}

func TestRealtimeEnv(t *testing.T) {
	n := newTestNode(t)
	n.cfg.Ports.SystemPostgres, n.cfg.Ports.Realtime = 37001, 37020
	c := testCreds(t, n)
	env := realtimeEnv(n.cfg, c)
	want := map[string]string{
		"PORT":                    "37020",
		"PHX_HTTP_IP":             "127.0.0.1",
		"APP_NAME":                "supavise",
		"DB_HOST":                 "127.0.0.1",
		"DB_PORT":                 "37001",
		"DB_USER":                 "supavise_realtime",
		"DB_PASSWORD":             "pw-realtime",
		"DB_NAME":                 "_realtime",
		"DB_AFTER_CONNECT_QUERY":  "SET search_path TO _realtime",
		"DB_IP_VERSION":           "ipv4",
		"GEN_RPC_SOCKET_IP":       "127.0.0.1",
		"GEN_RPC_TCP_SERVER_PORT": "38389",
		"GEN_RPC_TCP_CLIENT_PORT": "38389",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if env["RELEASE_TMP"] != n.cfg.Paths().System("realtime")+"/tmp" {
		t.Errorf("RELEASE_TMP = %q", env["RELEASE_TMP"])
	}
	if _, seeded := env["SEED_SELF_HOST"]; seeded {
		t.Error("a multi-tenant Realtime must not seed the self-host tenant")
	}
	if got := RealtimeGenRPCPort(config.Default()); got != 5369 {
		t.Errorf("default gen_rpc port = %d, want upstream's 5369", got)
	}
	if len(env["DB_ENC_KEY"]) != 16 || len(env["DB_ENC_KEY_GCM"]) != 32 || env["DB_ENC_WRITE_GCM"] != "true" {
		t.Errorf("encryption keys: %q %q %q", env["DB_ENC_KEY"], env["DB_ENC_KEY_GCM"], env["DB_ENC_WRITE_GCM"])
	}
}

func TestStorageEnvFile(t *testing.T) {
	n := newTestNode(t)
	n.cfg.Ports.SystemPostgres, n.cfg.Ports.Storage, n.cfg.Ports.StorageAdmin = 37001, 37030, 37031
	n.cfg.Domain = "Example.COM"
	c := testCreds(t, n)
	env, err := storageEnv(n.cfg, c, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"MULTI_TENANT":                   "true",
		"DATABASE_MULTITENANT_URL":       "postgres://supavise_storage:pw-storage@127.0.0.1:37001/_storage",
		"SERVER_HOST":                    "127.0.0.1",
		"SERVER_PORT":                    "37030",
		"SERVER_ADMIN_PORT":              "37031",
		"SERVER_ADMIN_API_KEYS":          c.storageAdminKey,
		"AUTH_ENCRYPTION_KEY":            c.storageEncKey,
		"REQUEST_ALLOW_X_FORWARDED_PATH": "true",
		// The host an S3 client signed (a custom hostname has no ref in it) comes in this header.
		"S3_PROTOCOL_NON_CANONICAL_HOST_HEADER": config.StorageClientHostHeader,
		"STORAGE_BACKEND":                       "file",
		"STORAGE_FILE_BACKEND_PATH":             n.cfg.Paths().System("storage") + "/objects",
		"UPLOAD_FILE_SIZE_LIMIT":                "52428800",
		"ADMIN_RETURN_TENANT_SENSITIVE_DATA":    "false",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	re := regexp.MustCompile(env["REQUEST_X_FORWARDED_HOST_REGEXP"])
	for host, wantRef := range map[string]string{
		testRef + ".api.example.com":         testRef,
		"studio.api.example.com":             "",
		testRef + ".api.example.com.evil.io": "",
		"x" + testRef + ".api.example.com":   "",
		testRef + ".apiXexample.com":         "",
		testRef + ".realtime.internal":       "",
	} {
		m := re.FindStringSubmatch(host)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != wantRef {
			t.Errorf("host %q matched %q, want %q", host, got, wantRef)
		}
	}
}

func TestStorageEnvS3AndValidation(t *testing.T) {
	n := newTestNode(t)
	c := testCreds(t, n)
	n.cfg.Fleet = config.Fleet{StorageBackend: "s3"}
	if _, err := storageEnv(n.cfg, c, ""); err == nil || !strings.Contains(err.Error(), "storage_s3_bucket") {
		t.Fatalf("s3 without a bucket: %v", err)
	}
	n.cfg.Fleet = config.Fleet{StorageBackend: "tape"}
	if _, err := storageEnv(n.cfg, c, ""); err == nil {
		t.Fatal("unknown backend accepted")
	}
	n.cfg.Fleet = config.Fleet{StorageBackend: "s3", StorageS3Bucket: "objs", StorageS3Endpoint: "http://minio:9000", StorageS3ForcePathStyle: true, StorageS3AccessKeyID: "AK", StorageS3SecretAccessKey: "SK", StorageFileSizeLimit: 123}
	env, err := storageEnv(n.cfg, c, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"STORAGE_BACKEND": "s3", "STORAGE_S3_BUCKET": "objs", "STORAGE_S3_ENDPOINT": "http://minio:9000", "STORAGE_S3_FORCE_PATH_STYLE": "true",
		"STORAGE_S3_REGION": "us-east-1", "AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "UPLOAD_FILE_SIZE_LIMIT": "123",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["STORAGE_FILE_BACKEND_PATH"]; ok {
		t.Error("the s3 backend must not set the file path")
	}
	// Without static keys the AWS default chain applies where it exists (the exec backend)...
	n.cfg.Fleet = config.Fleet{StorageBackend: "s3", StorageS3Bucket: "objs"}
	n.cfg.Backup.S3Region = "eu-west-1"
	env, _ = storageEnv(n.cfg, c, "")
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok || env["STORAGE_S3_REGION"] != "eu-west-1" {
		t.Errorf("env = %v", env)
	}
	// ... but not under systemd, where supavise-storage cannot reach the instance role: static
	// keys are required, and the error says why.
	n.cfg.Supervisor = config.SupervisorSystemd
	if _, err := storageEnv(n.cfg, c, ""); err == nil || !strings.Contains(err.Error(), "IMDS") {
		t.Fatalf("s3 without keys under systemd = %v; want an error that names IMDS", err)
	}
	n.cfg.Fleet.StorageS3AccessKeyID, n.cfg.Fleet.StorageS3SecretAccessKey = "AK", "SK"
	if _, err := storageEnv(n.cfg, c, ""); err != nil {
		t.Fatalf("s3 with keys under systemd: %v", err)
	}
}

func TestStudioEnv(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"
	cfg.Ports.Studio = 37040
	env := studioEnv(cfg, false)
	want := map[string]string{
		"HOSTNAME":                      "127.0.0.1",
		"PORT":                          "37040",
		"NEXT_PUBLIC_API_URL":           "https://api.example.com/platform",
		"NEXT_PUBLIC_GOTRUE_URL":        "https://api.example.com/auth/v1",
		"NEXT_PUBLIC_SITE_URL":          "https://studio.example.com",
		"CSP_EXTRA_PROJECT_HOSTS":       "*.api.example.com",
		"NEXT_PUBLIC_MCP_URL":           "https://api.example.com/mcp",
		"SUPAVISE_MANAGEMENT_API_URL":   "http://127.0.0.1:7000",
		"SUPAVISE_PROJECT_URL_TEMPLATE": "https://{ref}.api.example.com",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["NEXT_PUBLIC_HCAPTCHA_SITE_KEY"]; ok {
		t.Error("no captcha key configured, none must be passed")
	}
	cfg.Studio.HCaptchaSiteKey = "site-key"
	cfg.TLS.Mode = "off"
	env = studioEnv(cfg, false)
	if env["NEXT_PUBLIC_HCAPTCHA_SITE_KEY"] != "site-key" || env["NEXT_PUBLIC_API_URL"] != "http://api.example.com/platform" {
		t.Errorf("env = %v", env)
	}
	if env["NEXT_PUBLIC_MCP_URL"] != "http://api.example.com/mcp" || env["SUPAVISE_PROJECT_URL_TEMPLATE"] != "http://{ref}.api.example.com" {
		t.Errorf("without TLS the MCP URL and the project URL template must be http: %v", env)
	}
	// The runtime substitution only accepts this alphabet (studio/README.md).
	safe := regexp.MustCompile(`^[A-Za-z0-9._~:/@%+,*=-]*$`)
	for k, v := range env {
		if strings.HasPrefix(k, "NEXT_PUBLIC_") || k == "CSP_EXTRA_PROJECT_HOSTS" {
			if !safe.MatchString(v) {
				t.Errorf("%s = %q has characters the launcher refuses", k, v)
			}
		}
	}
}

func TestPGMetaEnv(t *testing.T) {
	n := newTestNode(t)
	n.cfg.Ports.PGMeta = 37050
	env := pgmetaEnv(n.cfg, testCreds(t, n))
	if env["PG_META_HOST"] != "127.0.0.1" || env["PG_META_PORT"] != "37050" || env["PG_META_ADMIN_PORT"] != "37051" || env["CRYPTO_KEY"] == "" {
		t.Fatalf("env = %v", env)
	}
	if _, ok := env["PG_META_CRYPTO_KEY"]; ok {
		t.Error("pg-meta reads CRYPTO_KEY; PG_META_CRYPTO_KEY is Studio's name for it")
	}
}

// With a dashboard SSO provider Studio is started with its default list of disabled features
// minus the SSO sign-in; without one the variable is left to the build's default.
func TestStudioEnvOffersSSOOnlyWithAProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"
	if _, ok := studioEnv(cfg, false)["NEXT_PUBLIC_DISABLED_FEATURES"]; ok {
		t.Error("without a provider the build's default list must apply")
	}
	got := studioEnv(cfg, true)["NEXT_PUBLIC_DISABLED_FEATURES"]
	if got == "" || strings.Contains(got, "sign_in_with_sso") {
		t.Fatalf("with a provider the list is %q, which must not disable the SSO sign-in", got)
	}
	for _, f := range []string{"dashboard_auth:sign_up", "dashboard_auth:sign_in_with_github", "dashboard_auth:show_tos"} {
		if !strings.Contains(got, f) {
			t.Errorf("%s is no longer disabled: %q", f, got)
		}
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9._~:/@%+,*=-]*$`)
	if !safe.MatchString(got) {
		t.Errorf("%q has characters the launcher refuses", got)
	}
}

// The Go copy of the build's default list stays equal to studio/placeholders.json.
func TestStudioDisabledFeaturesMatchThePlaceholderFile(t *testing.T) {
	b, err := os.ReadFile("../../studio/placeholders.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Values []struct {
			Env     string `json:"env"`
			Default string `json:"default"`
		} `json:"values"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, v := range doc.Values {
		if v.Env == "NEXT_PUBLIC_DISABLED_FEATURES" {
			if want := strings.Join(StudioDisabledFeatures(false), ","); v.Default != want {
				t.Fatalf("studio/placeholders.json lists %q, fleet.StudioDisabledFeatures %q", v.Default, want)
			}
			return
		}
	}
	t.Fatal("NEXT_PUBLIC_DISABLED_FEATURES is not in studio/placeholders.json")
}

// The MCP variables follow the node's own addresses: a public URL for the browser, the admin
// listener for the server side, and the scheme of the public listeners for project URLs.
func TestStudioEnvMCP(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"

	for _, tc := range []struct{ name, admin, publicURL, api, mgmt string }{
		{"default", "", "", "https://api.example.com/mcp", "http://127.0.0.1:7000"},
		{"public url", "", "https://api.example.org/", "https://api.example.org/mcp", "http://127.0.0.1:7000"},
		{"other loopback port", "127.0.0.1:7123", "", "https://api.example.com/mcp", "http://127.0.0.1:7123"},
		{"all addresses", "0.0.0.0:7000", "", "https://api.example.com/mcp", "http://127.0.0.1:7000"},
		{"no host", ":7001", "", "https://api.example.com/mcp", "http://127.0.0.1:7001"},
		{"ipv6 unspecified", "[::]:7000", "", "https://api.example.com/mcp", "http://127.0.0.1:7000"},
		{"ipv6 loopback", "[::1]:7000", "", "https://api.example.com/mcp", "http://[::1]:7000"},
		{"not an address", "garbage", "", "https://api.example.com/mcp", "http://127.0.0.1:7000"},
	} {
		c := *cfg
		if tc.admin != "" {
			c.Listen.Admin = tc.admin
		}
		c.API.PublicURL = tc.publicURL
		env := studioEnv(&c, false)
		if env["NEXT_PUBLIC_MCP_URL"] != tc.api || env["SUPAVISE_MANAGEMENT_API_URL"] != tc.mgmt {
			t.Errorf("%s: MCP URL %q, Management API URL %q; want %q and %q", tc.name,
				env["NEXT_PUBLIC_MCP_URL"], env["SUPAVISE_MANAGEMENT_API_URL"], tc.api, tc.mgmt)
		}
		// Projects keep their own hosts whatever the public API URL is.
		if env["SUPAVISE_PROJECT_URL_TEMPLATE"] != "https://{ref}.api.example.com" {
			t.Errorf("%s: project URL template %q", tc.name, env["SUPAVISE_PROJECT_URL_TEMPLATE"])
		}
	}
}

// [api] disable_oauth removes the MCP endpoint, so Studio is not told about one: the Connect
// panel keeps the URL the build defaults to, and the route has no Management API to call.
func TestStudioEnvOAuthOff(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"
	cfg.API.DisableOAuth = true
	env := studioEnv(cfg, true)
	for _, k := range []string{"NEXT_PUBLIC_MCP_URL", "SUPAVISE_MANAGEMENT_API_URL", "SUPAVISE_PROJECT_URL_TEMPLATE"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q with OAuth disabled; want it unset", k, v)
		}
	}
	if env["NEXT_PUBLIC_API_URL"] == "" || env["NEXT_PUBLIC_DISABLED_FEATURES"] == "" {
		t.Errorf("the rest of the environment must not change: %v", env)
	}
}

// Every required placeholder of the build is set by studioEnv, and so is every optional one but the
// two a node sets only when it departs from the default. An optional value that a unit forgets
// would otherwise fall back to the build's default without any sign of it (an old MCP URL).
func TestStudioEnvCoversPlaceholders(t *testing.T) {
	b, err := os.ReadFile("../../studio/placeholders.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Values []struct {
			Env      string `json:"env"`
			Required bool   `json:"required"`
			Default  string `json:"default"`
		} `json:"values"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Domain = "example.com"
	env := studioEnv(cfg, false)
	// Placeholders a node sets only when it differs from the build's default.
	optional := map[string]bool{"NEXT_PUBLIC_HCAPTCHA_SITE_KEY": true, "NEXT_PUBLIC_DISABLED_FEATURES": true}
	seen := map[string]bool{}
	for _, v := range doc.Values {
		seen[v.Env] = true
		if _, ok := env[v.Env]; !ok && (v.Required || !optional[v.Env]) {
			t.Errorf("%s is in studio/placeholders.json (required: %v) and studioEnv does not set it", v.Env, v.Required)
		}
	}
	if !seen["NEXT_PUBLIC_MCP_URL"] {
		t.Error("NEXT_PUBLIC_MCP_URL is not in studio/placeholders.json")
	}
	// Server-side variables of patch 0004: not placeholders (read per request), always set.
	for _, k := range []string{"SUPAVISE_MANAGEMENT_API_URL", "SUPAVISE_PROJECT_URL_TEMPLATE"} {
		if env[k] == "" {
			t.Errorf("%s is not set", k)
		}
		if seen[k] {
			t.Errorf("%s is read at request time and must not be a placeholder", k)
		}
	}
	// The launcher refuses placeholder values outside this alphabet; {ref} is only for the
	// server-side template.
	safe := regexp.MustCompile(`^[A-Za-z0-9._~:/@%+,*=-]*$`)
	for _, v := range doc.Values {
		if val, ok := env[v.Env]; ok && !safe.MatchString(val) {
			t.Errorf("%s = %q has characters the launcher refuses", v.Env, val)
		}
	}
}
