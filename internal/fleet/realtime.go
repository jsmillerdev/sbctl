package fleet

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/OWNER/sbctl/internal/config"
)

// Realtime is one shared server for every project. It finds the tenant of a request in
// the first label of the Host header (the proxy sends <ref>.realtime.internal), checks the
// client's JWT against the tenant's jwt_secret, and reaches the tenant's database with the
// settings of the tenant's postgres_cdc_rls extension row. Upstream requires a superuser
// there ("DB_USER ... Requires a superuser" in ENVS.md); hosted and self-hosted both use
// supabase_admin, so does this. Tenants are managed through POST /api/tenants (create or
// update), authenticated with a JWT signed with the server's API_JWT_SECRET. Creating a
// tenant also runs the 88 tenant migrations in the project's database, which takes a few
// seconds, so the HTTP timeout is long.
const (
	realtimeSlotName    = "supabase_realtime_replication_slot"
	realtimePublication = "supabase_realtime"
)

type realtimeTenant struct {
	cl     *apiClient
	store  tenantStore
	base   string // http://127.0.0.1:<port>
	secret string // API_JWT_SECRET
	now    func() time.Time
}

func (t *realtimeTenant) Service() string { return config.SvcRealtime }

func (t *realtimeTenant) headers() (map[string]string, error) {
	tok, err := signToken(t.secret, t.now())
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + tok}, nil
}

func (t *realtimeTenant) tenantURL(ref string) string {
	return t.base + "/api/tenants/" + url.PathEscape(ref)
}

// realtimeBody is POST /api/tenants: {"tenant": {...}} as TenantParams in
// lib/realtime_web/open_api_schemas.ex describes it. Extension settings use string keys,
// and db_port must be a string (the required-settings checker is is_binary/1 for it).
// db_host, db_port, db_name, db_user and db_password are stored encrypted. The limits are
// left to the server's TENANT_MAX_* defaults.
func realtimeBody(spec TenantSpec) map[string]any {
	host := spec.DBHost
	if host == "" {
		host = "127.0.0.1"
	}
	db := spec.DBName
	if db == "" {
		db = "postgres"
	}
	user := spec.DBUser
	if user == "" {
		user = "supabase_admin"
	}
	return map[string]any{"tenant": map[string]any{
		"external_id":          spec.Ref,
		"name":                 spec.Ref,
		"jwt_secret":           spec.JWTSecret,
		"postgres_cdc_default": "postgres_cdc_rls",
		"extensions": []map[string]any{{
			"type":               "postgres_cdc_rls",
			"tenant_external_id": spec.Ref,
			"settings": map[string]any{
				"region":                realtimeCDCRegion,
				"db_host":               host,
				"db_port":               strconv.Itoa(spec.DBPort),
				"db_name":               db,
				"db_user":               user,
				"db_password":           spec.DBPassword,
				"slot_name":             realtimeSlotName,
				"publication":           realtimePublication,
				"poll_interval_ms":      100,
				"poll_max_changes":      100,
				"poll_max_record_bytes": 1048576,
				"ssl_enforced":          false,
			},
		}},
	}}
}

// EnsureTenant implements Tenant.
func (t *realtimeTenant) EnsureTenant(ctx context.Context, spec TenantSpec) error {
	if err := validTenantRef(spec.Ref); err != nil {
		return err
	}
	if spec.DBPort <= 0 || spec.DBPassword == "" || spec.JWTSecret == "" {
		return fmt.Errorf("fleet: realtime tenant %s needs the database port and password and the JWT secret", spec.Ref)
	}
	body := realtimeBody(spec)
	fp, err := fingerprintOf(body)
	if err != nil {
		return err
	}
	h, err := t.headers()
	if err != nil {
		return err
	}
	got, err := t.cl.do(ctx, "GET", t.tenantURL(spec.Ref), h, nil)
	if err != nil {
		return err
	}
	switch {
	case got.ok():
		if t.store.get(ctx, spec.Ref, config.SvcRealtime) == fp {
			return nil // an unchanged update would still re-encrypt the secret and disconnect the tenant's sockets
		}
	case got.Status == 404:
	default:
		return t.cl.apiError("get tenant "+spec.Ref, got)
	}
	if h, err = t.headers(); err != nil {
		return err
	}
	post, err := t.cl.do(ctx, "POST", t.base+"/api/tenants", h, body)
	if err != nil {
		return err
	}
	if !post.ok() {
		return t.cl.apiError("create tenant "+spec.Ref, post)
	}
	if err := t.store.put(ctx, spec.Ref, config.SvcRealtime, fp); err != nil {
		t.cl.log.Warn("fleet: could not record the realtime tenant fingerprint", "ref", spec.Ref, "error", err)
	}
	return nil
}

// RemoveTenant implements Tenant: DELETE /api/tenants/<ref> disconnects the tenant's
// sockets, deletes the row, and stops its replication connections. 404 counts as removed.
func (t *realtimeTenant) RemoveTenant(ctx context.Context, ref string) error {
	if err := validTenantRef(ref); err != nil {
		return err
	}
	h, err := t.headers()
	if err != nil {
		return err
	}
	del, err := t.cl.do(ctx, "DELETE", t.tenantURL(ref), h, nil)
	if err != nil {
		return err
	}
	if !del.ok() && del.Status != 404 {
		return t.cl.apiError("delete tenant "+ref, del)
	}
	t.store.forget(ctx, ref, config.SvcRealtime)
	return nil
}
