// Package fleet registers projects as tenants of the shared multi-tenant services
// (Supavisor, Realtime, Storage) through their admin APIs.
package fleet

import "context"

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
	JWTSecret        string
	AnonKey          string
	ServiceRoleKey   string
	// Host is the project API host (<ref>.api.<domain>); Storage matches it via x-forwarded-host.
	Host string
	// PoolSize and MaxClients for Supavisor; zero means service default.
	PoolSize   int
	MaxClients int
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
