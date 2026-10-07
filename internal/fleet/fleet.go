// Package fleet registers projects as tenants of the shared multi-tenant services
// (Supavisor, Realtime, Storage) through their admin APIs.
package fleet

import (
	"context"

	"github.com/OWNER/sbctl/internal/projectconfig"
)

// TenantSpec is what a shared service needs to serve one project.
type TenantSpec struct {
	Ref string
	// Project Postgres on loopback.
	DBHost string
	DBPort int
	DBName string // "postgres"
	// DBUser/DBPassword is the role the service connects as (supabase_admin unless a
	// service documents otherwise).
	DBUser     string
	DBPassword string
	// Pooler: the "postgres" role password users log in with as postgres.<ref>.
	PostgresPassword string
	// StorageAdminPassword is the password of supabase_storage_admin, the role Storage
	// connects as (upstream's compose does the same). Empty means the Storage tenant
	// looks it up in the registry by Ref.
	StorageAdminPassword string
	JWTSecret            string
	AnonKey              string
	ServiceRoleKey       string
	// Host is the project API host (<ref>.api.<domain>); Storage matches it via x-forwarded-host.
	Host string
	// PoolSize and MaxClients for Supavisor; zero means service default.
	PoolSize   int
	MaxClients int
	// Storage and Realtime carry the project's saved settings for those services (the
	// zero values mean "defaults"). They are part of the tenant fingerprint, so a changed
	// setting makes EnsureTenant send an update.
	Storage  projectconfig.StorageSettings
	Realtime projectconfig.RealtimeSettings
}

// Quiescer is an optional Tenant capability: let go of everything the service holds open in a
// project's database (Realtime's replication connections and pool) and disconnect its clients,
// so that the project's PostgreSQL can stop. A logical walsender that stays connected keeps a
// fast shutdown from finishing until systemd kills the cluster. The service reconnects when a
// client next asks for the tenant.
type Quiescer interface {
	QuiesceTenant(ctx context.Context, ref string) error
}

// QuiesceTenant calls QuiesceTenant on every tenant that implements Quiescer.
func (f Fleet) QuiesceTenant(ctx context.Context, ref string) error {
	var first error
	for _, t := range f {
		if q, ok := t.(Quiescer); ok {
			if err := q.QuiesceTenant(ctx, ref); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// Refresher is an optional Tenant capability: drop whatever the service cached about a
// project's database logins (Supavisor's pools and credential cache), after a role password
// changed. A service that caches nothing does not implement it.
type Refresher interface {
	RefreshTenant(ctx context.Context, ref string) error
}

// RefreshTenant calls RefreshTenant on every tenant that implements Refresher.
func (f Fleet) RefreshTenant(ctx context.Context, ref string) error {
	var first error
	for _, t := range f {
		if r, ok := t.(Refresher); ok {
			if err := r.RefreshTenant(ctx, ref); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// Tenant is one shared service's tenant registry. Both calls are idempotent and
// retried internally on transient errors.
type Tenant interface {
	Service() string // config.SvcSupavisor, config.SvcRealtime, config.SvcStorage
	EnsureTenant(ctx context.Context, spec TenantSpec) error
	RemoveTenant(ctx context.Context, ref string) error
}

// Fleet fans tenant operations out to every configured service.
type Fleet []Tenant

func (f Fleet) EnsureTenant(ctx context.Context, spec TenantSpec) error {
	for _, t := range f {
		if err := t.EnsureTenant(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}

func (f Fleet) RemoveTenant(ctx context.Context, ref string) error {
	var first error
	for _, t := range f {
		if err := t.RemoveTenant(ctx, ref); err != nil && first == nil {
			first = err
		}
	}
	return first
}
