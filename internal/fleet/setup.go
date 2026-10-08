package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmillerdev/supavise/internal/artifacts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// tenantTimeout is the HTTP timeout of one tenant call. Creating a Realtime or Storage
// tenant runs that service's migrations in the project's database first.
const tenantTimeout = 2 * time.Minute

// Setup builds the Tenant implementations for Supavisor, Realtime and Storage, in the order
// EnsureTenant calls them, and returns them as a Fleet that lifecycle.Options.Fleet takes.
// It generates the services' sealed secrets in the registry if this is the first call (the
// tenants authenticate to the admin APIs with them), so the system project must be
// initialized. With Deps.Start it also renders and starts the services (see Manager.Start).
//
// The returned Fleet is usable even when the error is not nil: a service that does not
// start is reported in the error, but the tenants of the others (and of this one, once it
// runs) work, so a caller logs the error and keeps going instead of booting with an empty
// Fleet, which would silently skip every tenant call. The Fleet is nil only when Setup
// could not build it at all (missing Deps, uninitialized system project). Studio is
// optional: its failure to start is logged and shows in Manager.Status, but is not an
// error here.
//
// Setup needs Cfg, Registry and Secrets, and Supervisor and Artifacts when Start is set.
// lifecycle.Open builds its Engine before anyone can hand it a Fleet, so a caller that
// wants the Engine to use this Fleet either passes Lazy to lifecycle.Open, or builds the
// Engine again:
//
//	n, _ := lifecycle.Open(ctx, cfg, oo)
//	fl, err := fleet.Setup(ctx, fleet.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
//		Supervisor: n.Supervisor, Artifacts: n.Artifacts, Start: true})
//	if err != nil { log(err) } // fl is still usable unless it is nil
//	eng := lifecycle.NewEngine(cfg, n.Registry, n.Secrets, n.Artifacts, n.Plane, lifecycle.Options{Fleet: fl})
func Setup(ctx context.Context, d Deps) (Fleet, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil {
		return nil, errors.New("fleet: Setup needs Deps.Cfg, Registry and Secrets")
	}
	c, err := loadCreds(ctx, d, true)
	if err != nil {
		return nil, err
	}
	f := newTenants(d, c)
	if d.Start {
		m, err := NewManager(d)
		if err != nil {
			return f, err
		}
		if err := m.Start(ctx); err != nil {
			return f, err
		}
	}
	return f, nil
}

func newTenants(d Deps, c *creds) Fleet {
	cfg := d.Cfg
	hc := d.TenantClient
	if hc == nil {
		hc = &http.Client{Timeout: tenantTimeout}
	}
	client := func(name string) *apiClient {
		return &apiClient{name: name, http: hc, retry: d.Retry, log: d.log()}
	}
	store := tenantStore{reg: d.Registry, sec: d.Secrets}
	now := time.Now
	// The releases this node pins: part of the Realtime and Storage tenants' fingerprints.
	var tags map[string]string
	if v, err := artifacts.LoadVersions(cfg); err == nil {
		tags = map[string]string{config.SvcRealtime: v.Artifacts[config.ArtifactName(config.SvcRealtime)], config.SvcStorage: v.Artifacts[config.ArtifactName(config.SvcStorage)]}
	}
	return Fleet{
		&supavisorTenant{
			cl: client(config.SvcSupavisor), store: store, base: "http://" + Addr(cfg, config.SvcSupavisor),
			secret: c.supavisorAPIJWT, now: now, setManager: setManagerPassword,
		},
		&realtimeTenant{
			cl: client(config.SvcRealtime), store: store, base: "http://" + Addr(cfg, config.SvcRealtime),
			secret: c.realtimeAPIJWT, now: now, release: tags[config.SvcRealtime],
		},
		&storageTenant{
			cl: client(config.SvcStorage), store: store, base: fmt.Sprintf("http://127.0.0.1:%d", cfg.Ports.StorageAdmin),
			adminKey: c.storageAdminKey, fileSize: cfg.Fleet.FileSizeLimit(), release: tags[config.SvcStorage],
			adminPassword: registryAdminPassword(d.Registry, d.Secrets),
		},
	}
}

// TenantSpecFor describes project p to the shared services: its Postgres on loopback,
// supabase_admin as the role the services connect as (Storage uses
// supabase_storage_admin, Supavisor its pgbouncer role, see their tenants), and its keys.
// It is what lifecycle.Engine passes to Fleet.EnsureTenant, plus StorageAdminPassword.
func TenantSpecFor(cfg *config.Config, p *registry.Project, k *secrets.ProjectKeys) TenantSpec {
	return TenantSpec{
		Ref: p.Ref, DBHost: "127.0.0.1", DBPort: cfg.PortsFor(p.Ref, p.Seq).Postgres, DBName: "postgres",
		DBUser: "supabase_admin", DBPassword: k.AdminPassword, PostgresPassword: k.DBPassword,
		StorageAdminPassword: k.StorageAdminPassword,
		JWTSecret:            k.JWTSecret, AnonKey: k.AnonKey, ServiceRoleKey: k.ServiceRoleKey,
		Host: cfg.ProjectHost(p.Ref),
	}
}

// LoadTenantSpec reads project ref and its sealed credentials from the registry and
// returns its TenantSpec.
func LoadTenantSpec(ctx context.Context, d Deps, ref string) (TenantSpec, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil {
		return TenantSpec{}, errors.New("fleet: LoadTenantSpec needs Deps.Cfg, Registry and Secrets")
	}
	if err := validTenantRef(ref); err != nil {
		return TenantSpec{}, err
	}
	p, err := d.Registry.GetProject(ctx, ref)
	if err != nil {
		return TenantSpec{}, fmt.Errorf("fleet: project %s: %w", ref, err)
	}
	sealed, err := d.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return TenantSpec{}, err
	}
	m := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := d.Secrets.Open(blob)
		if err != nil {
			return TenantSpec{}, fmt.Errorf("fleet: open secret %s of %s: %w", name, ref, err)
		}
		m[name] = string(pt)
	}
	k := secrets.KeysFromMap(m)
	if k.JWTSecret == "" || k.AdminPassword == "" {
		return TenantSpec{}, fmt.Errorf("fleet: project %s has no stored credentials", ref)
	}
	spec := TenantSpecFor(d.Cfg, p, k)
	// The saved Storage and Realtime settings belong to the tenant: `supavise fleet
	// ensure-tenant` must not send the defaults over them.
	if pg, ok := d.Registry.(interface{ Pool() *pgxpool.Pool }); ok && ref != config.SystemRef {
		m := projectconfig.NewManager(projectconfig.NewPGStore(pg.Pool()), d.Secrets, projectconfig.Options{})
		if spec.Storage, err = m.StorageSettings(ctx, ref); err != nil {
			return TenantSpec{}, fmt.Errorf("fleet: storage settings of %s: %w", ref, err)
		}
		if spec.Realtime, err = m.RealtimeSettings(ctx, ref); err != nil {
			return TenantSpec{}, fmt.Errorf("fleet: realtime settings of %s: %w", ref, err)
		}
		pool, err := m.PoolerSettings(ctx, ref)
		if err != nil {
			return TenantSpec{}, fmt.Errorf("fleet: pooler settings of %s: %w", ref, err)
		}
		spec.PoolSize, spec.MaxClients = pool.PoolSize, pool.MaxClients
	}
	return spec, nil
}
