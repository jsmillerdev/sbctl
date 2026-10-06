// Package lifecycle creates, pauses, resumes, deletes and re-keys projects: registry
// rows, sealed secrets, data-plane units and fleet tenants, in that order.
package lifecycle

import (
	"context"
	"errors"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

var ErrInvalidState = errors.New("lifecycle: operation not allowed in the project's current status")

// DataSeeder fills an empty Postgres data directory instead of initdb (restore and
// clone). It runs before the cluster's first start; recovery settings it writes
// (recovery.signal, restore_command, recovery_target_time) are honored on that start.
type DataSeeder func(ctx context.Context, p *registry.Project, dataDir string) error

type CreateRequest struct {
	Name    string
	OrgSlug string
	Region  string // informational; default "local"
	Class   string
	// Ref forces the ref (restore --as, tests); empty generates one.
	Ref string
	// DBPassword sets the "postgres" role password; empty generates one.
	DBPassword string
	// Limits override config.Defaults.
	Limits *config.Limits
	// Seed replaces initdb. Keys are still generated fresh unless Keys is set.
	Seed DataSeeder
	// Keys reuses an existing credential set (restore keeps the source project's keys
	// because the restored cluster already contains roles with those passwords).
	Keys *secrets.ProjectKeys
}

type ServiceHealth struct {
	Name    string // config.Svc*
	Healthy bool
	Status  string // "ACTIVE_HEALTHY", "COMING_UP", "UNHEALTHY"
	Error   string
}

// Manager is the project lifecycle used by the API server and the CLI.
type Manager interface {
	// Create provisions a project and returns it ACTIVE_HEALTHY, or INIT_FAILED with an error.
	Create(ctx context.Context, req CreateRequest) (*registry.Project, error)
	Pause(ctx context.Context, ref string) error
	Resume(ctx context.Context, ref string) error
	// Delete takes a final base backup, removes tenants, units, data and the registry row.
	Delete(ctx context.Context, ref string) error
	// RotateKeys issues a new JWT secret and API keys and restarts the services that read them.
	RotateKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	// Keys returns the decrypted credentials of ref (works for "system").
	Keys(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	Health(ctx context.Context, ref string) ([]ServiceHealth, error)
	// ConnString is a loopback DSN straight to ref's Postgres as role ("postgres" or
	// "supabase_admin"), database "postgres". It bypasses Supavisor.
	ConnString(ctx context.Context, ref, role string) (string, error)
}

// Upstreams are a project's loopback service addresses ("127.0.0.1:port").
type Upstreams struct {
	Postgres  string
	GoTrue    string
	PostgREST string
}

type Usage struct {
	DiskBytes   int64
	MemoryBytes uint64
}

// DataPlane is the per-engine half of Manager (HANDOFF.md section 1). The Postgres
// engine runs one cluster plus GoTrue and PostgREST per project.
type DataPlane interface {
	Create(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, seed DataSeeder) error
	Delete(ctx context.Context, ref string) error
	Snapshot(ctx context.Context, ref string) (*registry.Backup, error)
	Route(ctx context.Context, ref string) (Upstreams, error)
	Usage(ctx context.Context, ref string) (Usage, error)
}
