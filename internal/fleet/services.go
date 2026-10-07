package fleet

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

// Values the services are configured with that are not secrets.
const (
	// RealtimeAppName is Realtime's APP_NAME (the production config refuses an empty one).
	RealtimeAppName = "sbctl"
	// realtimeCDCRegion is the region of every Realtime tenant's postgres_cdc_rls extension,
	// the value upstream's self-host seed uses; the node's own REGION is "local".
	realtimeCDCRegion = "us-east-1"
	// storageFileBucket is the first path component under the file backend's directory;
	// "stub" is what upstream's compose and the dockerless CLI use, so layouts stay
	// interchangeable.
	storageFileBucket = "stub"
)

// ecto, pg and the other URL builders keep passwords out of fmt verbs that would mangle
// them: every secret here is alphanumeric, but url.UserPassword escapes anyway.
func ectoURL(l Login, port int) string {
	u := url.URL{Scheme: "ecto", User: url.UserPassword(l.User, l.Password), Host: "127.0.0.1:" + strconv.Itoa(port), Path: "/" + l.Database}
	return u.String()
}

func pgURL(user, password string, port int, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: "127.0.0.1:" + strconv.Itoa(port), Path: "/" + db}
	return u.String()
}

// storageHostRegexp matches <ref>.api.<domain> and captures the ref. Refs are 20 lowercase
// letters; the system project has no Storage tenant.
func storageHostRegexp(cfg *config.Config) string {
	return `^([a-z]{20})\.api\.` + regexpQuote(cfg.BaseDomain()) + `$`
}

func regexpQuote(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return string(b)
}

// spec builds the unit spec of svc.
func (m *Manager) spec(svc string, c *creds) (units.Spec, error) {
	cfg := m.cfg()
	art, err := m.d.Artifacts.Dir(svc)
	if err != nil {
		return units.Spec{}, err
	}
	s := units.Spec{
		Service:     svc,
		ArtifactDir: art,
		WorkDir:     cfg.Paths().System(svc),
		Limits:      cfg.Defaults,
	}
	switch svc {
	case config.SvcPGMeta:
		s.Env = pgmetaEnv(cfg, c)
	case config.SvcSupavisor:
		s.Env = supavisorEnv(cfg, c)
		s.PreStart = [][]string{{"bin/prepare"}} // schema migrations of _supavisor
	case config.SvcRealtime:
		s.Env = realtimeEnv(cfg, c)
		s.PreStart = [][]string{{"bin/prepare"}} // schema migrations of _realtime (no self-host seeding)
	case config.SvcStorage:
		env, err := storageEnv(cfg, c)
		if err != nil {
			return units.Spec{}, err
		}
		s.Env = env // the server runs the multi-tenant migrations itself at start
	case config.SvcStudio:
		s.Env = studioEnv(cfg)
	default:
		return units.Spec{}, fmt.Errorf("fleet: no unit definition for %q", svc)
	}
	return s, nil
}

// pgmetaEnv: postgres-meta is stateless. It takes the connection string of each request,
// encrypted with CRYPTO_KEY by the Management API, so it holds no database credentials.
// Its second listener (admin app) defaults to PG_META_PORT+1 and is pinned to it here.
func pgmetaEnv(cfg *config.Config, c *creds) map[string]string {
	return map[string]string{
		"PG_META_HOST":       "127.0.0.1",
		"PG_META_PORT":       strconv.Itoa(cfg.Ports.PGMeta),
		"PG_META_ADMIN_PORT": strconv.Itoa(cfg.Ports.PGMeta + 1),
		"CRYPTO_KEY":         c.pgmetaKey,
	}
}

// RELEASE_TMP, set for both Elixir services, is where an Elixir release writes its
// runtime files if it needs any (the artifact directory is read-only under systemd).
//
// supavisorEnv follows the dockerless CLI's recipe (supabase/cli packages/stack
// services/Pooler.ts), the upstream compose file, and config/runtime.exs at the pinned
// version. Supavisor has no bind-address variable: its API port and the pooler ports
// listen on every interface. The internal shard listeners (PROXY_PORT,
// SESSION_PROXY_PORTS, TRANSACTION_PROXY_PORTS) take ephemeral ports. The metadata
// database is _supavisor of the system cluster, as its own owner role. Metrics are
// unreachable: their JWT secret is random and never shown to anyone.
func supavisorEnv(cfg *config.Config, c *creds) map[string]string {
	return map[string]string{
		"DATABASE_URL":            ectoURL(c.logins[config.SvcSupavisor], cfg.Ports.SystemPostgres),
		"DB_POOL_SIZE":            "5",
		"PORT":                    strconv.Itoa(cfg.Fleet.SupavisorAPI()),
		"PROXY_PORT_SESSION":      strconv.Itoa(cfg.Ports.SupavisorSession),
		"PROXY_PORT_TRANSACTION":  strconv.Itoa(cfg.Ports.SupavisorTransaction),
		"PROXY_PORT":              "0",
		"SESSION_PROXY_PORTS":     "0",
		"TRANSACTION_PROXY_PORTS": "0",
		"RELEASE_TMP":             filepath.Join(cfg.Paths().System(config.SvcSupavisor), "tmp"),
		"API_JWT_SECRET":          c.supavisorAPIJWT,
		"METRICS_JWT_SECRET":      c.supavisorMetricsJWT,
		"SECRET_KEY_BASE":         c.supavisorSecretBase,
		"VAULT_ENC_KEY":           c.supavisorVaultKey,
		"REGION":                  "local",
		"NODE_IP":                 "127.0.0.1",
	}
}

// RealtimeGenRPCPort is the loopback port of Realtime's gen_rpc server: the default 5369
// when Ports.Realtime is the default 4000.
func RealtimeGenRPCPort(cfg *config.Config) int { return cfg.Ports.Realtime + 1369 }

// realtimeEnv follows the CLI's Realtime.ts recipe and the upstream compose file, minus
// SEED_SELF_HOST: tenants are created through the API, never seeded. Tenants resolve
// from the first label of the Host header (<ref>.realtime.internal). The HTTP listener
// is pinned to loopback with PHX_HTTP_IP (a slim-services patch; upstream binds every
// interface) and so is gen_rpc.
func realtimeEnv(cfg *config.Config, c *creds) map[string]string {
	l := c.logins[config.SvcRealtime]
	return map[string]string{
		"PORT":                        strconv.Itoa(cfg.Ports.Realtime),
		"RELEASE_TMP":                 filepath.Join(cfg.Paths().System(config.SvcRealtime), "tmp"),
		"PHX_HTTP_IP":                 "127.0.0.1",
		"APP_NAME":                    RealtimeAppName,
		"DB_HOST":                     "127.0.0.1",
		"DB_PORT":                     strconv.Itoa(cfg.Ports.SystemPostgres),
		"DB_USER":                     l.User,
		"DB_PASSWORD":                 l.Password,
		"DB_NAME":                     l.Database,
		"DB_AFTER_CONNECT_QUERY":      "SET search_path TO _realtime",
		"DB_IP_VERSION":               "ipv4",
		"DB_ENC_KEY":                  c.realtimeDBEncKey,
		"DB_ENC_KEY_GCM":              c.realtimeDBEncKeyGCM,
		"DB_ENC_WRITE_GCM":            "true",
		"API_JWT_SECRET":              c.realtimeAPIJWT,
		"METRICS_JWT_SECRET":          c.realtimeMetricsJWT,
		"SECRET_KEY_BASE":             c.realtimeSecretBase,
		"DNS_NODES":                   "''",
		"ERL_AFLAGS":                  "-proto_dist inet_tcp",
		"RUN_JANITOR":                 "true",
		"DISABLE_HEALTHCHECK_LOGGING": "true",
		"LOG_LEVEL":                   "warning",
		"GEN_RPC_SOCKET_IP":           "127.0.0.1",
		"GEN_RPC_TCP_SERVER_PORT":     strconv.Itoa(RealtimeGenRPCPort(cfg)),
		"GEN_RPC_TCP_CLIENT_PORT":     strconv.Itoa(RealtimeGenRPCPort(cfg)),
	}
}

// storageEnv: Storage in multi-tenant mode. Tenants resolve from x-forwarded-host,
// matched by REQUEST_X_FORWARDED_HOST_REGEXP; the proxy sets that header and
// X-Forwarded-Prefix, which REQUEST_ALLOW_X_FORWARDED_PATH makes Storage honor (as the
// upstream compose file does). The tenant table lives in _storage of the system cluster;
// each tenant's own database is reached with the credentials its tenant record carries.
// The admin API (tenant management) is on its own port, behind an API key.
func storageEnv(cfg *config.Config, c *creds) (map[string]string, error) {
	l := c.logins[config.SvcStorage]
	env := map[string]string{
		"MULTI_TENANT":                       "true",
		"DATABASE_MULTITENANT_URL":           pgURL(l.User, l.Password, cfg.Ports.SystemPostgres, l.Database),
		"SERVER_HOST":                        "127.0.0.1",
		"SERVER_PORT":                        strconv.Itoa(cfg.Ports.Storage),
		"SERVER_ADMIN_PORT":                  strconv.Itoa(cfg.Ports.StorageAdmin),
		"SERVER_ADMIN_API_KEYS":              c.storageAdminKey,
		"AUTH_ENCRYPTION_KEY":                c.storageEncKey,
		"REQUEST_X_FORWARDED_HOST_REGEXP":    storageHostRegexp(cfg),
		"REQUEST_ALLOW_X_FORWARDED_PATH":     "true",
		"UPLOAD_FILE_SIZE_LIMIT":             strconv.FormatInt(cfg.Fleet.FileSizeLimit(), 10),
		"REGION":                             "local",
		"LOG_LEVEL":                          "warn",
		"ADMIN_RETURN_TENANT_SENSITIVE_DATA": "false",
	}
	f := cfg.Fleet
	switch f.StorageBackend {
	case "", "file":
		env["STORAGE_BACKEND"] = "file"
		env["STORAGE_FILE_BACKEND_PATH"] = filepath.Join(cfg.Paths().System(config.SvcStorage), "objects")
		env["STORAGE_S3_BUCKET"] = storageFileBucket
		env["STORAGE_S3_REGION"] = "local"
	case "s3":
		if f.StorageS3Bucket == "" {
			return nil, fmt.Errorf("fleet: [fleet] storage_backend = \"s3\" needs storage_s3_bucket")
		}
		env["STORAGE_BACKEND"] = "s3"
		env["STORAGE_S3_BUCKET"] = f.StorageS3Bucket
		region := f.StorageS3Region
		if region == "" {
			region = cfg.Backup.S3Region
		}
		if region == "" {
			region = "us-east-1"
		}
		env["STORAGE_S3_REGION"] = region
		if f.StorageS3Endpoint != "" {
			env["STORAGE_S3_ENDPOINT"] = f.StorageS3Endpoint
		}
		if f.StorageS3ForcePathStyle {
			env["STORAGE_S3_FORCE_PATH_STYLE"] = "true"
		}
		if f.StorageS3AccessKeyID != "" || f.StorageS3SecretAccessKey != "" {
			env["AWS_ACCESS_KEY_ID"] = f.StorageS3AccessKeyID
			env["AWS_SECRET_ACCESS_KEY"] = f.StorageS3SecretAccessKey
		}
	default:
		return nil, fmt.Errorf("fleet: [fleet] storage_backend %q must be \"file\" or \"s3\"", f.StorageBackend)
	}
	return env, nil
}

// studioEnv is the runtime configuration of our platform-mode Studio build (see
// studio/README.md). The browser reaches the Management API and the system GoTrue
// through the proxy on api.<domain>; project API hosts are allowed in the CSP.
func studioEnv(cfg *config.Config) map[string]string {
	env := map[string]string{
		"HOSTNAME":                "127.0.0.1",
		"PORT":                    strconv.Itoa(cfg.Ports.Studio),
		"NEXT_PUBLIC_API_URL":     cfg.APIURL() + "/platform",
		"NEXT_PUBLIC_GOTRUE_URL":  cfg.APIURL() + "/auth/v1",
		"NEXT_PUBLIC_SITE_URL":    cfg.DashboardURL(),
		"CSP_EXTRA_PROJECT_HOSTS": "*.api." + cfg.BaseDomain(),
	}
	if k := cfg.Studio.HCaptchaSiteKey; k != "" {
		env["NEXT_PUBLIC_HCAPTCHA_SITE_KEY"] = k
	}
	return env
}
