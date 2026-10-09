// Package fleet registers projects as tenants of the shared multi-tenant services
// (Supavisor, Realtime, Storage) through their admin APIs.
package fleet

import (
	"context"

	"github.com/supavise/supavise/internal/projectconfig"
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
	// ReplicaID is the identifier of a project's replica when the spec describes the replica's
	// Supavisor tenant (TenantSpecForReplica): the tenant's external id, with DBPort the replica's
	// port. Ref stays the project's, which owns the credentials. Empty for the project's own tenant.
	ReplicaID string
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
	return forAll(f, func(q Quiescer) error { return q.QuiesceTenant(ctx, ref) })
}

// Refresher is an optional Tenant capability: drop whatever the service cached about a
// project's database logins (Supavisor's pools and credential cache), after a role password
// changed. A service that caches nothing does not implement it.
type Refresher interface {
	RefreshTenant(ctx context.Context, ref string) error
}

// RefreshTenant calls RefreshTenant on every tenant that implements Refresher.
func (f Fleet) RefreshTenant(ctx context.Context, ref string) error {
	return forAll(f, func(r Refresher) error { return r.RefreshTenant(ctx, ref) })
}

// ReplicaTenanter is an optional Tenant capability: pool a project's read replica under a tenant
// of its own (Supavisor's, with the replica's identifier as the external id), so that
// postgres.<identifier> logs in to the standby. Services that serve no replica do not implement it.
type ReplicaTenanter interface {
	EnsureReplicaTenant(ctx context.Context, spec TenantSpec) error
	RemoveReplicaTenant(ctx context.Context, identifier string) error
}

// EnsureReplicaTenant calls EnsureReplicaTenant on every tenant that implements ReplicaTenanter.
func (f Fleet) EnsureReplicaTenant(ctx context.Context, spec TenantSpec) error {
	return forEach(f, func(r ReplicaTenanter) error { return r.EnsureReplicaTenant(ctx, spec) })
}

// RemoveReplicaTenant calls RemoveReplicaTenant on every tenant that implements ReplicaTenanter.
func (f Fleet) RemoveReplicaTenant(ctx context.Context, identifier string) error {
	return forAll(f, func(r ReplicaTenanter) error { return r.RemoveReplicaTenant(ctx, identifier) })
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
	return forEach(f, func(t Tenant) error { return t.EnsureTenant(ctx, spec) })
}

func (f Fleet) RemoveTenant(ctx context.Context, ref string) error {
	return forAll(f, func(t Tenant) error { return t.RemoveTenant(ctx, ref) })
}

// forEach calls do on every tenant of f that implements C, in order, and stops at the first error.
func forEach[C any](f Fleet, do func(C) error) error {
	for _, t := range f {
		if c, ok := t.(C); ok {
			if err := do(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// forAll calls do on every tenant of f that implements C, in order, whatever the others return, and
// reports the first error.
func forAll[C any](f Fleet, do func(C) error) error {
	var first error
	for _, t := range f {
		if c, ok := t.(C); ok {
			if err := do(c); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
