package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// DefaultArchiveTimeout is the archive_timeout, in seconds, used when
// config.Backup.ArchiveTimeoutSeconds is zero.
const DefaultArchiveTimeout = 300

// Role names used for loopback connections to a project's cluster.
const (
	roleAdmin = "supabase_admin"
)

// Access gives backups a superuser connection to a project's cluster. lifecycle.Manager
// satisfies it; AccessFromRegistry is the adapter for processes without a Manager.
type Access interface {
	// ConnString is a loopback DSN for ref's Postgres as role, database "postgres".
	ConnString(ctx context.Context, ref, role string) (string, error)
}

// Options configures a Service. Only Store is required for WAL push and fetch.
type Options struct {
	Config   *config.Config
	Registry registry.Registry
	Store    Store
	// Access is required for base backups and post-restore cleanup.
	Access Access
	// Secrets opens the sealed secrets stored in backups; required for Restore.
	Secrets secrets.Secrets
	// Manager is required for Restore; workstream D implements it.
	Manager lifecycle.Manager
	// ConfigPath is embedded as --config in archive_command and restore_command when
	// it is not the default, so Postgres children find the same configuration.
	ConfigPath string
	// DataDir returns a project's Postgres data directory, used by in-place restore.
	// Default: config.Paths.ProjectService(ref, "postgres").
	DataDir func(ref string) string
	// Version is recorded in backup manifests.
	Version string
	Now     func() time.Time
	Log     *slog.Logger
	// RecoveryTimeout bounds the wait for a restored cluster to finish recovery before
	// its recovery settings are cleared (default 30 minutes). RecoveryPoll is the poll
	// interval (default 1 second).
	RecoveryTimeout time.Duration
	RecoveryPoll    time.Duration
	// RecoveryFailGrace is how long a cluster that was replaying WAL may stay unreachable
	// before the restore counts as failed: PostgreSQL shuts down on a fatal recovery error
	// (default 15 seconds).
	RecoveryFailGrace time.Duration
	// ArchiveFlushTimeout bounds how long a restore to the end of the archive waits for a
	// running source to archive its newest WAL before a time or latest restore (default 60 seconds).
	ArchiveFlushTimeout time.Duration
}

// Service implements Backup over a Store and the registry.
type Service struct {
	opt Options
	// probe and alter are the database calls of the post-restore wait; tests replace them.
	probe func(ctx context.Context, ref string) (inRecovery bool, err error)
	alter func(ctx context.Context, ref string, gucs []string) error
}

var _ Backup = (*Service)(nil)

// New returns a Service. It does not connect to anything.
func New(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("backup: Options.Store is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.DataDir == nil && o.Config != nil {
		paths := o.Config.Paths()
		o.DataDir = func(ref string) string { return paths.ProjectService(ref, config.SvcPostgres) }
	}
	if o.RecoveryTimeout <= 0 {
		o.RecoveryTimeout = 30 * time.Minute
	}
	if o.RecoveryPoll <= 0 {
		o.RecoveryPoll = time.Second
	}
	if o.RecoveryFailGrace <= 0 {
		o.RecoveryFailGrace = 15 * time.Second
	}
	if o.ArchiveFlushTimeout <= 0 {
		o.ArchiveFlushTimeout = time.Minute
	}
	s := &Service{opt: o}
	s.probe, s.alter = s.pgInRecovery, s.pgResetSettings
	return s, nil
}

// Store returns the backend the service writes to.
func (s *Service) Store() Store { return s.opt.Store }

func (s *Service) need(what string, ok bool) error {
	if !ok {
		return fmt.Errorf("backup: %s is not configured for this operation", what)
	}
	return nil
}

// validRef accepts a user project ref or "system"; it is also what keeps ref safe as a key prefix.
func validRef(ref string) error {
	if ref == config.SystemRef || secrets.ValidRef(ref) {
		return nil
	}
	return fmt.Errorf("backup: invalid project ref %q", ref)
}

func walDir(ref string) string  { return ref + "/wal/" }
func baseDir(ref string) string { return ref + "/base/" }

// archiveTimeout is the archive_timeout in seconds for project clusters.
func archiveTimeout(c *config.Config) int {
	if c != nil && c.Backup.ArchiveTimeoutSeconds > 0 {
		return c.Backup.ArchiveTimeoutSeconds
	}
	return DefaultArchiveTimeout
}

// RegistryRows lists ref's backup rows in the registry, newest first.
func (s *Service) RegistryRows(ctx context.Context, ref string) ([]registry.Backup, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := s.need("registry", s.opt.Registry != nil); err != nil {
		return nil, err
	}
	return s.opt.Registry.ListBackups(ctx, ref)
}
