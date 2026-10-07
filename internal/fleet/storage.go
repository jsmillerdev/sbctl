package fleet

import (
	"context"
	"fmt"
	"net/url"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Storage runs once with MULTI_TENANT=true. It takes the tenant of a request from the
// x-forwarded-host header (the proxy sets it to <ref>.api.<domain>), and everything it
// needs to serve that tenant (database URL, JWT secret, the legacy anon and service keys)
// lives in an encrypted row of its own tenants table. Tenants are managed on a second port,
// the admin API, with an `apikey` header from SERVER_ADMIN_API_KEYS.
//
// Upstream's create route (POST /tenants/<tenant>) inserts and fails on a second call, while
// PUT /tenants/<tenant> upserts, so EnsureTenant uses PUT. Either route runs the tenant's storage
// schema migrations in the project's database straight away, which is why the HTTP
// timeout is long.
const (
	// storageTenantConnections is the size of the pool Storage keeps per tenant database.
	// The artifact's default (2) is a low-footprint development value.
	storageTenantConnections = 5
	// storageRole is the database role Storage connects as, as in upstream's compose file:
	// supabase_storage_admin owns the storage schema that the init scripts created.
	storageRole = "supabase_storage_admin"
)

type storageTenant struct {
	cl       *apiClient
	store    tenantStore
	base     string // admin API: http://127.0.0.1:<storage_admin>
	adminKey string
	fileSize int64
	// adminPassword finds the supabase_storage_admin password of a project when the
	// TenantSpec does not carry it.
	adminPassword func(ctx context.Context, ref string) (string, error)
}

func (t *storageTenant) Service() string { return config.SvcStorage }

func (t *storageTenant) headers() map[string]string { return map[string]string{"apikey": t.adminKey} }

func (t *storageTenant) tenantURL(ref string) string {
	return t.base + "/tenants/" + url.PathEscape(ref)
}

// storageBody is PUT /tenants/<tenant>, the `schema` of src/http/routes/admin/tenants.ts: anonKey,
// databaseUrl, jwtSecret and serviceKey are required.
func storageBody(spec TenantSpec, dbURL string, fileSize int64) map[string]any {
	return map[string]any{
		"anonKey":        spec.AnonKey,
		"serviceKey":     spec.ServiceRoleKey,
		"jwtSecret":      spec.JWTSecret,
		"databaseUrl":    dbURL,
		"maxConnections": storageTenantConnections,
		"fileSizeLimit":  fileSize,
		"features": map[string]any{
			"imageTransformation": map[string]any{"enabled": false},
			"purgeCache":          map[string]any{"enabled": false},
			"s3Protocol":          map[string]any{"enabled": true},
		},
	}
}

// EnsureTenant implements Tenant.
func (t *storageTenant) EnsureTenant(ctx context.Context, spec TenantSpec) error {
	if err := validTenantRef(spec.Ref); err != nil {
		return err
	}
	if spec.DBPort <= 0 || spec.JWTSecret == "" || spec.AnonKey == "" || spec.ServiceRoleKey == "" {
		return fmt.Errorf("fleet: storage tenant %s needs the database port, the JWT secret and the anon and service_role keys", spec.Ref)
	}
	pw := spec.StorageAdminPassword
	if pw == "" {
		var err error
		if pw, err = t.adminPassword(ctx, spec.Ref); err != nil {
			return fmt.Errorf("fleet: storage tenant %s: password of %s: %w", spec.Ref, storageRole, err)
		}
	}
	db := spec.DBName
	if db == "" {
		db = "postgres"
	}
	body := storageBody(spec, pgURL(storageRole, pw, spec.DBPort, db), t.fileSize)
	fp, err := fingerprintOf(body)
	if err != nil {
		return err
	}
	got, err := t.cl.do(ctx, "GET", t.tenantURL(spec.Ref), t.headers(), nil)
	if err != nil {
		return err
	}
	switch {
	case got.ok():
		if t.store.get(ctx, spec.Ref, config.SvcStorage) == fp {
			return nil
		}
	case got.Status == 404:
	default:
		return t.cl.apiError("get tenant "+spec.Ref, got)
	}
	put, err := t.cl.do(ctx, "PUT", t.tenantURL(spec.Ref), t.headers(), body)
	if err != nil {
		return err
	}
	if !put.ok() {
		return t.cl.apiError("put tenant "+spec.Ref, put)
	}
	if err := t.store.put(ctx, spec.Ref, config.SvcStorage, fp); err != nil {
		t.cl.log.Warn("fleet: could not record the storage tenant fingerprint", "ref", spec.Ref, "error", err)
	}
	return nil
}

// RemoveTenant implements Tenant: DELETE /tenants/<tenant> removes the tenant row (its objects
// stay where the backend keeps them: <ref>/ under the file directory or the bucket).
func (t *storageTenant) RemoveTenant(ctx context.Context, ref string) error {
	if err := validTenantRef(ref); err != nil {
		return err
	}
	del, err := t.cl.do(ctx, "DELETE", t.tenantURL(ref), t.headers(), nil)
	if err != nil {
		return err
	}
	if !del.ok() && del.Status != 404 {
		return t.cl.apiError("delete tenant "+ref, del)
	}
	t.store.forget(ctx, ref, config.SvcStorage)
	return nil
}

// registryAdminPassword reads supabase_storage_admin's password from the project's sealed
// secrets.
func registryAdminPassword(reg registry.Registry, sec secrets.Secrets) func(context.Context, string) (string, error) {
	return func(ctx context.Context, ref string) (string, error) {
		sealed, err := reg.GetSecret(ctx, ref, secrets.NameStorageAdminPassword)
		if err != nil {
			return "", err
		}
		pt, err := sec.Open(sealed)
		if err != nil {
			return "", err
		}
		return string(pt), nil
	}
}
