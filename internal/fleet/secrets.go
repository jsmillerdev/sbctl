package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Names of the sealed secrets the shared services share with supavise. All of them belong
// to the system project (ref "system") in supavise.project_secrets and are generated once:
// a service reads them from its env file on every start, and some of them encrypt data
// at rest (Supavisor's vault key, Realtime's tenant encryption keys, Storage's
// encryption key), so replacing one would make the stored tenants unreadable.
const (
	// SecretPGMetaCryptoKey is the passphrase shared with supavise-pgmeta (its CRYPTO_KEY). The
	// Management API builds x-connection-encrypted with it, under the same name
	// (api.SecretPGMetaCryptoKey; a test checks that the two agree).
	SecretPGMetaCryptoKey = "pgmeta_crypto_key"

	SecretSupavisorAPIJWT      = "fleet_supavisor_api_jwt_secret"
	SecretSupavisorMetricsJWT  = "fleet_supavisor_metrics_jwt_secret"
	SecretSupavisorSecretBase  = "fleet_supavisor_secret_key_base"
	SecretSupavisorVaultKey    = "fleet_supavisor_vault_enc_key"
	SecretRealtimeAPIJWT       = "fleet_realtime_api_jwt_secret"
	SecretRealtimeMetricsJWT   = "fleet_realtime_metrics_jwt_secret"
	SecretRealtimeSecretBase   = "fleet_realtime_secret_key_base"
	SecretRealtimeDBEncKey     = "fleet_realtime_db_enc_key"
	SecretRealtimeDBEncKeyGCM  = "fleet_realtime_db_enc_key_gcm"
	SecretStorageAdminAPIKey   = "fleet_storage_admin_api_key"
	SecretStorageEncryptionKey = "fleet_storage_encryption_key"
)

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Login is a database login on the system cluster.
type Login struct {
	User     string
	Password string
	Database string
}

// loginDefs are the system cluster logins of the fleet services, created by
// lifecycle.InitSystem (lifecycle.FleetRoles) with their passwords sealed as
// fleet_<service>_password. fleet cannot import lifecycle (lifecycle imports fleet), so the
// names are repeated here and a test in package fleet_test checks them against lifecycle.
var loginDefs = []struct{ Service, User, Database string }{
	{config.SvcSupavisor, "supavise_supavisor", "_supavisor"},
	{config.SvcRealtime, "supavise_realtime", "_realtime"},
	{config.SvcStorage, "supavise_storage", "_storage"},
}

func loginSecretName(service string) string { return "fleet_" + service + "_password" }

// creds are the decrypted secrets of the shared services.
type creds struct {
	// dashboardSSO is true while at least one SAML identity provider of the dashboard is
	// registered: Studio then shows "Continue with SSO". It is read with the credentials
	// because rendering Studio's unit needs the registry like they do.
	dashboardSSO bool

	supavisorAPIJWT, supavisorMetricsJWT, supavisorSecretBase, supavisorVaultKey string
	realtimeAPIJWT, realtimeMetricsJWT, realtimeSecretBase                       string
	realtimeDBEncKey, realtimeDBEncKeyGCM                                        string
	storageAdminKey, storageEncKey                                               string
	pgmetaKey                                                                    string
	logins                                                                       map[string]Login
}

// ensureMu serializes ensureSecret within the process.
var ensureMu sync.Mutex

// ensureSecret returns the system secret name, generating and sealing it when absent.
// It inserts only if absent and reads back, so two processes that race (the daemon and
// the CLI) end up with one value.
func ensureSecret(ctx context.Context, reg registry.Registry, sec secrets.Secrets, name string, gen func() string) (string, error) {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	read := func() (string, error) {
		sealed, err := reg.GetSecret(ctx, config.SystemRef, name)
		if err != nil {
			return "", err
		}
		plain, err := sec.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("fleet: open secret %s: %w", name, err)
		}
		return string(plain), nil
	}
	v, err := read()
	if !errors.Is(err, registry.ErrNotFound) {
		return v, err
	}
	sealed, err := sec.Seal([]byte(gen()))
	if err != nil {
		return "", err
	}
	if pg, ok := reg.(interface{ Pool() *pgxpool.Pool }); ok && pg.Pool() != nil {
		_, err = pg.Pool().Exec(ctx, `insert into supavise.project_secrets (ref, name, ciphertext) values ($1, $2, $3) on conflict (ref, name) do nothing`,
			config.SystemRef, name, sealed)
	} else {
		err = reg.PutSecret(ctx, config.SystemRef, name, sealed)
	}
	if err != nil {
		return "", fmt.Errorf("fleet: store secret %s: %w (is the system project initialized? run `supavise system init`)", name, err)
	}
	return read()
}

// loadCreds returns the fleet secrets. With create it generates the ones that do not
// exist yet; without, a missing secret is an error.
func loadCreds(ctx context.Context, d Deps, create bool) (*creds, error) {
	c := &creds{logins: map[string]Login{}}
	get := func(dst *string, name string, gen func() string) error {
		var v string
		var err error
		if create {
			v, err = ensureSecret(ctx, d.Registry, d.Secrets, name, gen)
		} else {
			v, err = readSecret(ctx, d.Registry, d.Secrets, name)
		}
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
	rnd := func(n int) func() string { return func() string { return secrets.RandomString(n, alnum) } }
	steps := []struct {
		dst  *string
		name string
		gen  func() string
	}{
		{&c.supavisorAPIJWT, SecretSupavisorAPIJWT, secrets.NewJWTSecret},
		{&c.supavisorMetricsJWT, SecretSupavisorMetricsJWT, secrets.NewJWTSecret},
		{&c.supavisorSecretBase, SecretSupavisorSecretBase, rnd(64)},
		{&c.supavisorVaultKey, SecretSupavisorVaultKey, rnd(32)}, // Cloak AES-256-GCM: exactly 32 bytes
		{&c.realtimeAPIJWT, SecretRealtimeAPIJWT, secrets.NewJWTSecret},
		{&c.realtimeMetricsJWT, SecretRealtimeMetricsJWT, secrets.NewJWTSecret},
		{&c.realtimeSecretBase, SecretRealtimeSecretBase, rnd(64)},
		{&c.realtimeDBEncKey, SecretRealtimeDBEncKey, rnd(16)},       // legacy AES-128-ECB key: 16 bytes
		{&c.realtimeDBEncKeyGCM, SecretRealtimeDBEncKeyGCM, rnd(32)}, // AES-256-GCM key: 32 bytes
		{&c.storageAdminKey, SecretStorageAdminAPIKey, secrets.NewJWTSecret},
		{&c.storageEncKey, SecretStorageEncryptionKey, rnd(32)},
	}
	for _, s := range steps {
		if err := get(s.dst, s.name, s.gen); err != nil {
			return nil, err
		}
	}
	if k := d.Cfg.API.PGMetaCryptoKey; k != "" {
		c.pgmetaKey = k
	} else if err := get(&c.pgmetaKey, SecretPGMetaCryptoKey, secrets.NewPassword); err != nil {
		return nil, err
	}
	for _, ld := range loginDefs {
		pw, err := readSecret(ctx, d.Registry, d.Secrets, loginSecretName(ld.Service))
		if err != nil {
			return nil, fmt.Errorf("fleet: login of %s: %w (run `supavise system init`)", ld.User, err)
		}
		c.logins[ld.Service] = Login{User: ld.User, Password: pw, Database: ld.Database}
	}
	var err error
	if c.dashboardSSO, err = d.dashboardSSO(ctx); err != nil {
		return nil, fmt.Errorf("fleet: are there dashboard SSO providers: %w", err)
	}
	return c, nil
}

// dashboardSSO reports whether a SAML identity provider of the dashboard is registered:
// Deps.DashboardSSO when given, else the registry's own answer (the Postgres registry has
// one; the in-memory registry of tests has none and so reports false).
func (d Deps) dashboardSSO(ctx context.Context) (bool, error) {
	if d.DashboardSSO != nil {
		return d.DashboardSSO(ctx)
	}
	if r, ok := d.Registry.(interface {
		HasDashboardSSO(ctx context.Context) (bool, error)
	}); ok {
		return r.HasDashboardSSO(ctx)
	}
	return false, nil
}

func readSecret(ctx context.Context, reg registry.Registry, sec secrets.Secrets, name string) (string, error) {
	sealed, err := reg.GetSecret(ctx, config.SystemRef, name)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	plain, err := sec.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("fleet: open secret %s: %w", name, err)
	}
	return string(plain), nil
}
