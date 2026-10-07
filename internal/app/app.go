// Package app composes the merged pieces into one working product: it wires the backup
// service into the lifecycle engine (and the engine into the backup service's restore),
// the archive command and settings into every cluster the engine renders, and runs the
// daemon (`sbctl serve`): Management API, edge proxy, project start at boot and
// graceful shutdown. The packages it joins cannot import each other (backup needs the
// lifecycle Manager, lifecycle needs a base backuper), so the wiring lives here.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Options are what the binary tells the composition about itself.
type Options struct {
	Log *slog.Logger
	// ConfigPath is the config file this process loaded ("" when there is none). It is
	// exported to every Postgres unit as SBCTL_CONFIG and put into archive_command and
	// restore_command, so the children read the same backend settings.
	ConfigPath string
	// Version is recorded in backup manifests.
	Version string
	// Fleet registers projects with the shared services (Supavisor, Realtime, Storage).
	Fleet fleet.Fleet
	// Artifacts replaces the artifact store (tests with unpacked artifacts); nil means the
	// store under state_dir.
	Artifacts lifecycle.Artifacts
	// StopBudget overrides StopBudget, how long Serve waits for running lifecycle
	// operations when it is told to stop (tests).
	StopBudget time.Duration
}

func (o Options) log() *slog.Logger {
	if o.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o.Log
}

// LifecycleOptions is lifecycle.OpenOptions with the backup package wired in: every
// cluster archives WAL through `sbctl wal push` (backup.ArchiveCommand and the
// configured archive_timeout), deleting a project takes a final base backup through the
// backup service, and a restore reaches the Engine. The backup service is built on first
// use, so commands that never back up do not open the backend.
func LifecycleOptions(cfg *config.Config, o Options) lifecycle.OpenOptions {
	timeout := cfg.Backup.ArchiveTimeoutSeconds
	if timeout <= 0 {
		timeout = backup.DefaultArchiveTimeout
	}
	return lifecycle.OpenOptions{
		Log:               o.log(),
		ConfigPath:        o.ConfigPath,
		Fleet:             o.Fleet,
		Artifacts:         o.Artifacts,
		ArchiveCommandFor: func(ref string) string { return backup.ArchiveCommand(cfg.BinPath, ref, o.ConfigPath) },
		ArchiveTimeout:    timeout,
		BackupFactory: func(n *lifecycle.Node) (lifecycle.BaseBackuper, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return NewBackupService(ctx, cfg, n.Registry, n.Secrets, o)
		},
	}
}

// NewBackupService builds the backup service over the configured backend and the
// registry. It has no Manager yet: lifecycle hands it the Engine (SetManager) when the
// service is created through LifecycleOptions, and the CLI sets it for `backups restore`.
func NewBackupService(ctx context.Context, cfg *config.Config, reg registry.Registry, sec secrets.Secrets, o Options) (*backup.Service, error) {
	if err := backup.ValidateOnCalendar(cfg.Backup.BaseBackupOnCalendar); err != nil {
		return nil, fmt.Errorf("config backup.base_backup_on_calendar: %w", err)
	}
	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		return nil, err
	}
	return backup.New(backup.Options{
		Config: cfg, Registry: reg, Store: store, Secrets: sec,
		Access:     backup.AccessFromRegistry(cfg, reg, sec),
		ConfigPath: o.ConfigPath, Version: o.Version, Log: o.log(),
	})
}

// PGMetaCryptoKey is the passphrase shared with sb-pgmeta (its CRYPTO_KEY variable) and the
// Management API (the x-connection-encrypted header): [api] pgmeta_crypto_key when set,
// else the random key kept sealed in the registry as the system secret pgmeta_crypto_key,
// created on first use. Whoever renders the sb-pgmeta unit must pass exactly this value;
// a mismatch makes every pg-meta call fail.
func PGMetaCryptoKey(ctx context.Context, cfg *config.Config, reg registry.Registry, sec secrets.Secrets) (string, error) {
	if k := cfg.API.PGMetaCryptoKey; k != "" {
		return k, nil
	}
	return api.EnsurePGMetaCryptoKey(ctx, reg, sec)
}
