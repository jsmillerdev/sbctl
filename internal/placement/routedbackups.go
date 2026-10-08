package placement

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// BackupService is the part of the leader's backup service that RoutedBackups uses (*backup.Service).
type BackupService interface {
	BaseBackupWith(ctx context.Context, ref string, bo backup.BackupOptions) (*registry.Backup, error)
	BackupFiles(ctx context.Context, ref string, fo backup.FilesOptions) (*backup.FilesResult, error)
	RecordBase(ctx context.Context, ref string, b backup.RemoteBase) (*registry.Backup, error)
}

// RoutedBackups takes the base backup of a project on the node the project is homed on. A base backup
// reads the project's data directory, which only its home has; the leader's backup service has none
// for a project homed elsewhere. The home takes the backup (BackupOps.BaseBackup, without writing the
// registry, whose copy there is read-only) and the leader records it (backup.Service.RecordBase),
// so the backup is in the registry, in the store and in the events as one taken here.
//
// Its TakeBase is backup.Options.TakeBase, which EnsureBase calls to seed a replica from a recent
// base backup; the Engine's final backup of a delete uses it through SetRemoteBackups.
type RoutedBackups struct {
	Self     func() string
	Resolver Resolver
	Ops      BackupOps
	Local    BackupService
}

var _ lifecycle.RemoteBackups = (*RoutedBackups)(nil)

// TakeBase takes a base backup of ref wherever ref is homed. It has the signature of
// backup.Options.TakeBase.
func (r *RoutedBackups) TakeBase(ctx context.Context, ref string) (*registry.Backup, error) {
	return r.BaseBackupWith(ctx, ref, backup.BackupOptions{})
}

// BaseBackupWith is TakeBase with options.
func (r *RoutedBackups) BaseBackupWith(ctx context.Context, ref string, bo backup.BackupOptions) (*registry.Backup, error) {
	home, err := r.Resolver.HomeOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	return r.at(ctx, home, ref, bo)
}

// at takes the backup on node (this node when node is empty or its id).
func (r *RoutedBackups) at(ctx context.Context, node, ref string, bo backup.BackupOptions) (*registry.Backup, error) {
	if node == "" || node == r.Self() {
		return r.Local.BaseBackupWith(ctx, ref, bo)
	}
	res, err := r.Ops.BaseBackup(ctx, node, ref, peerapi.BackupRequest{Reason: bo.Reason})
	if err != nil {
		return nil, fmt.Errorf("base backup of %s on node %s: %w", ref, node, err)
	}
	if res.ID == "" {
		return nil, fmt.Errorf("node %s took a base backup of %s and named no backup", node, ref)
	}
	return r.Local.RecordBase(ctx, ref, backup.RemoteBase{ID: res.ID, Reason: bo.Reason, Timeline: res.Timeline,
		StartLSN: res.StartLSN, StopLSN: res.StopLSN, SizeBytes: res.SizeBytes})
}

// FinalBackup implements lifecycle.RemoteBackups: the delete-time backup of p, which is homed on
// another node. Its Storage objects and Edge Functions belong to the shared services here, so the
// leader snapshots them (backup.Service.FinalBackup does the same first for a project homed here),
// and the base backup is taken on the home.
func (r *RoutedBackups) FinalBackup(ctx context.Context, p *registry.Project) (*registry.Backup, error) {
	if p.NodeID == "" || p.NodeID == r.Self() {
		return nil, errors.New("placement: FinalBackup is for a project homed on another node")
	}
	if _, err := r.Local.BackupFiles(ctx, p.Ref, backup.FilesOptions{Reason: backup.ReasonFinal}); err != nil {
		return nil, fmt.Errorf("final backup of the files of %s failed: %w", p.Ref, err)
	}
	return r.at(ctx, p.NodeID, p.Ref, backup.BackupOptions{Reason: backup.ReasonFinal})
}
