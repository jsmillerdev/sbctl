// Package registry is supavise's control-plane state: organizations, projects, sealed
// project secrets, access tokens, host routes, backups and events. It lives in the
// "supavise" schema of the "supavise" database in the system cluster (Postgres), with an
// in-memory implementation for tests.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

var (
	ErrNotFound = errors.New("registry: not found")
	ErrConflict = errors.New("registry: already exists")
)

// Engine is the data-plane engine of a project. Only "postgres" is implemented.
type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineFile     Engine = "file"
)

// Status uses the Management API project status values.
type Status string

const (
	StatusComingUp        Status = "COMING_UP"
	StatusActiveHealthy   Status = "ACTIVE_HEALTHY"
	StatusActiveUnhealthy Status = "ACTIVE_UNHEALTHY"
	StatusPausing         Status = "PAUSING"
	StatusInactive        Status = "INACTIVE" // paused
	StatusRestoring       Status = "RESTORING"
	StatusRestarting      Status = "RESTARTING"
	StatusUpgrading       Status = "UPGRADING"
	StatusGoingDown       Status = "GOING_DOWN"
	StatusRemoved         Status = "REMOVED"
	StatusInitFailed      Status = "INIT_FAILED"
	StatusUnknown         Status = "UNKNOWN"
)

type Organization struct {
	ID        int64
	Slug      string
	Name      string
	CreatedAt time.Time
}

type Project struct {
	Ref    string
	OrgID  int64 // 0 for the system project
	Seq    int   // port sequence; see config.Config.PortsFor
	Name   string
	Region string
	Engine Engine
	Class  string
	Status Status
	// Versions maps service name (config.Svc*) to the artifact tag the project runs.
	Versions  map[string]string
	Limits    config.Limits
	CreatedAt time.Time
	UpdatedAt time.Time
	// Branch is set when the project is a branch of another project (workstream I).
	Branch *BranchInfo
}

// BranchState is the (deprecated but still decoded) status of the Management API's branch
// object: it tracks the last long operation, while the project's own Status tracks its units.
type BranchState string

const (
	BranchCreatingProject  BranchState = "CREATING_PROJECT"
	BranchRunningMigration BranchState = "RUNNING_MIGRATIONS"
	BranchMigrationsPassed BranchState = "MIGRATIONS_PASSED"
	BranchMigrationsFailed BranchState = "MIGRATIONS_FAILED"
)

// BranchInfo is what makes a project a branch.
type BranchInfo struct {
	ID         string // UUID, stable across reset
	ParentRef  string // immutable
	Name       string // unique per parent
	GitBranch  string
	Persistent bool
	WithData   bool
	// ExpiresAt is when the sweeper deletes a non-persistent branch; nil never.
	ExpiresAt *time.Time
	// DeletionScheduledAt is a soft delete: the sweeper removes the branch after it,
	// whatever Persistent says, and restore clears it.
	DeletionScheduledAt *time.Time
	NotifyURL           string
	State               BranchState
	// Detail is the outcome of the last long operation (an error text on failure).
	Detail string
	// CloneMethod records how the data was obtained: "schema", "clonefile", "reflink",
	// or "base-backup".
	CloneMethod       string
	ReviewRequestedAt *time.Time
	// Egress is the branch's outbound network policy (the Egress* constants). It is chosen
	// when the branch is created and kept through reset; empty on a schema-only branch.
	Egress string
}

// Outbound network policy of a branch cloned from its parent's data. The clone carries
// every outbound integration of the parent (webhooks, pg_net, pg_cron, foreign servers), so
// by default the branch's Postgres unit may reach loopback only.
const (
	// EgressPending: deny non-loopback egress once the first-start isolation is done. The
	// first start (a base-backup restore may fetch WAL from the backup backend) runs open
	// because the first-start settings already silence the integrations.
	EgressPending = "pending"
	// EgressDenied: the unit is rendered with every non-loopback address denied.
	EgressDenied = "denied"
	// EgressAllowed: the branch was created with the explicit opt-out.
	EgressAllowed = "allowed"
	// EgressUnenforced: denial was wanted but the supervisor cannot enforce it (exec backend).
	EgressUnenforced = "unenforced"
)

type AccessToken struct {
	ID         int64
	UserID     string // GoTrue user id in supavise-gotrue@system
	Name       string
	Hash       []byte // secrets.HashToken(token)
	Prefix     string // first characters shown in listings, e.g. "sbp_1a2b"
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

type Route struct {
	Host      string
	Ref       string
	Kind      string // "api" (derived <ref>.api.<domain>) or "custom"
	CreatedAt time.Time
}

type BackupStatus string

const (
	BackupRunning   BackupStatus = "running"
	BackupCompleted BackupStatus = "completed"
	BackupFailed    BackupStatus = "failed"
)

type Backup struct {
	ID         int64
	Ref        string
	Kind       string // "base"
	Status     BackupStatus
	Location   string // backend URL of the base backup
	Timeline   int
	StartLSN   string
	StopLSN    string
	SizeBytes  int64
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

type Event struct {
	ID        int64
	Ref       string
	Kind      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Change is one row change, delivered by Subscribe. Table is "projects", "routes" or
// "project_secrets"; Key is the ref (or host for routes); Op is insert|update|delete.
type Change struct {
	Table string `json:"table"`
	Op    string `json:"op"`
	Key   string `json:"key"`
}

// Registry is the control-plane store. Implementations are safe for concurrent use.
// Get* return ErrNotFound; Create* return ErrConflict on duplicates.
type Registry interface {
	CreateOrganization(ctx context.Context, slug, name string) (*Organization, error)
	GetOrganization(ctx context.Context, slug string) (*Organization, error)
	GetOrganizationByID(ctx context.Context, id int64) (*Organization, error)
	ListOrganizations(ctx context.Context) ([]Organization, error)
	UpdateOrganization(ctx context.Context, o *Organization) error

	// CreateProject inserts p. If p.Seq is 0 and p.Ref is not "system", the lowest
	// free seq >= 1 is assigned. CreatedAt and UpdatedAt are set.
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, ref string) (*Project, error)
	// ListProjects returns all projects including "system", ordered by seq.
	ListProjects(ctx context.Context) ([]Project, error)
	// UpdateProject writes name, region, class, status, versions and limits.
	UpdateProject(ctx context.Context, p *Project) error
	SetProjectStatus(ctx context.Context, ref string, s Status) error
	// UpdateBranch writes the mutable fields of b (name, git branch, persistence, expiry,
	// notify URL, state, detail, clone method) of the branch project ref and nothing else,
	// so it cannot overwrite a status change made at the same time. It never writes the
	// egress policy: a caller that read the branch, changed a field and wrote it back (a PATCH,
	// a restore) would otherwise put a stale policy over one set meanwhile. ErrNotFound when
	// ref is not a branch; ErrConflict when the name is taken under the same parent.
	UpdateBranch(ctx context.Context, ref string, b *BranchInfo) error
	// SetBranchEgress is the only writer of a branch's egress policy after creation (the
	// policy of a new branch is part of its row): a compare-and-set from the policy the caller
	// saw to the new one. ErrConflict when the policy is not from any more, ErrNotFound when ref
	// is not a branch.
	SetBranchEgress(ctx context.Context, ref, from, to string) error
	// DeleteProject removes the project, its secrets and routes (cascade).
	DeleteProject(ctx context.Context, ref string) error

	PutSecret(ctx context.Context, ref, name string, sealed []byte) error
	GetSecret(ctx context.Context, ref, name string) ([]byte, error)
	// GetSecrets returns all sealed secrets of ref by name.
	GetSecrets(ctx context.Context, ref string) (map[string][]byte, error)

	CreateAccessToken(ctx context.Context, t *AccessToken) error
	GetAccessTokenByHash(ctx context.Context, hash []byte) (*AccessToken, error)
	ListAccessTokens(ctx context.Context, userID string) ([]AccessToken, error)
	TouchAccessToken(ctx context.Context, id int64, at time.Time) error
	DeleteAccessToken(ctx context.Context, userID string, id int64) error

	PutRoute(ctx context.Context, r Route) error
	DeleteRoute(ctx context.Context, host string) error
	ListRoutes(ctx context.Context) ([]Route, error)

	CreateBackup(ctx context.Context, b *Backup) error
	UpdateBackup(ctx context.Context, b *Backup) error
	// ListBackups returns ref's backups, newest first.
	ListBackups(ctx context.Context, ref string) ([]Backup, error)
	DeleteBackup(ctx context.Context, id int64) error

	AppendEvent(ctx context.Context, ref, kind string, payload any) error
	// ListEvents returns ref's newest events first; ref "" lists all.
	ListEvents(ctx context.Context, ref string, limit int) ([]Event, error)

	// Subscribe delivers changes to projects, routes and project_secrets until ctx
	// ends. Delivery is best effort: consumers must also tolerate a full reload.
	Subscribe(ctx context.Context) (<-chan Change, error)

	Close()
}

// SecretCreator is implemented by registries that can store a secret only when none is stored
// yet, in one atomic step (both of ours do).
type SecretCreator interface {
	// PutSecretIfAbsent stores sealed under (ref, name) unless that secret exists. It reports
	// whether it stored it.
	PutSecretIfAbsent(ctx context.Context, ref, name string, sealed []byte) (bool, error)
}

// PutSecretIfAbsent stores a secret that something else may be creating at the same time (a
// signing key that the daemon and the CLI both create on first use): the first writer wins and
// the others keep reading what it stored. Registries without SecretCreator fall back to a
// check followed by a write, which is only as safe as that race allows.
func PutSecretIfAbsent(ctx context.Context, reg Registry, ref, name string, sealed []byte) (bool, error) {
	if c, ok := reg.(SecretCreator); ok {
		return c.PutSecretIfAbsent(ctx, ref, name, sealed)
	}
	if _, err := reg.GetSecret(ctx, ref, name); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return true, reg.PutSecret(ctx, ref, name, sealed)
}
