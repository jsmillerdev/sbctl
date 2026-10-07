package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// stopTimeout bounds pg_backup_stop, which waits for the WAL of the backup to reach
// the archive. It only runs long when archive_command is failing.
const stopTimeout = 15 * time.Minute

// BackupOptions tunes one base backup.
type BackupOptions struct {
	// Reason is recorded in the manifest: ReasonManual (default), ReasonScheduled, ReasonFinal or ReasonRestore.
	Reason string
}

// BaseBackup implements Backup: a manual base backup of ref.
func (s *Service) BaseBackup(ctx context.Context, ref string) (*registry.Backup, error) {
	return s.BaseBackupWith(ctx, ref, BackupOptions{})
}

// FinalBackup is the delete-time hook: lifecycle.Manager.Delete calls it while the
// project's cluster is still running and aborts the delete if it fails. It also
// satisfies lifecycle.DataPlane.Snapshot's shape.
//
// The project's Storage objects and function deployments are snapshotted too (BackupFiles):
// the delete removes them with everything else, and a restore of the final state wants them back.
func (s *Service) FinalBackup(ctx context.Context, ref string) (*registry.Backup, error) {
	rec, err := s.BaseBackupWith(ctx, ref, BackupOptions{Reason: ReasonFinal})
	if err != nil {
		return rec, err
	}
	if _, err := s.BackupFiles(ctx, ref, FilesOptions{Reason: ReasonFinal}); err != nil {
		return rec, fmt.Errorf("backup: final backup of the files of %s failed: %w", ref, err)
	}
	return rec, nil
}

// BaseBackupWith takes a base backup of ref's running cluster.
//
// Method: pg_backup_start() on a superuser connection, a copy of the data directory
// straight into a tar.zst object in the backend, then pg_backup_stop(), which waits
// until the WAL covering the backup is archived. Files are read directly because
// supavise runs on the same host as the cluster; no replication connection, HBA entry
// or max_wal_senders is needed (the Supabase Postgres artifact ships none of them
// ready, and has no pg_basebackup binary). The manifest is written last and marks
// the backup complete; a failed backup leaves no manifest and its objects are removed.
func (s *Service) BaseBackupWith(ctx context.Context, ref string, bo BackupOptions) (*registry.Backup, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := s.need("registry", s.opt.Registry != nil); err != nil {
		return nil, err
	}
	if err := s.need("database access", s.opt.Access != nil); err != nil {
		return nil, err
	}
	if bo.Reason == "" {
		bo.Reason = ReasonManual
	}
	reg := s.opt.Registry
	proj, err := reg.GetProject(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("backup: project %s: %w", ref, err)
	}
	if never, why := s.neverRecovered(ctx, ref); never {
		return nil, fmt.Errorf("backup: %s has no restorable state: %s: %w", ref, why, lifecycle.ErrNoRestorableState)
	}

	started := s.opt.Now().UTC().Truncate(time.Second)
	id := newBackupID(started)
	rec := &registry.Backup{Ref: ref, Kind: "base", Status: registry.BackupRunning, StartedAt: started,
		Location: s.opt.Store.URL(baseDir(ref) + id)}
	if err := reg.CreateBackup(ctx, rec); err != nil {
		return nil, err
	}

	m, err := s.runBase(ctx, proj, id, started, bo.Reason)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	now := s.opt.Now()
	rec.FinishedAt = &now
	if err != nil {
		rec.Status, rec.Error = registry.BackupFailed, err.Error()
		s.cleanupBackup(fctx, ref, id)
		_ = reg.UpdateBackup(fctx, rec)
		_ = reg.AppendEvent(fctx, ref, "backup.failed", map[string]any{"id": id, "reason": bo.Reason, "error": err.Error()})
		return rec, fmt.Errorf("backup: base backup of %s failed: %w", ref, err)
	}
	rec.Status, rec.Timeline, rec.StartLSN, rec.StopLSN, rec.SizeBytes = registry.BackupCompleted, m.Timeline, m.StartLSN, m.StopLSN, m.StoredBytes
	if err := reg.UpdateBackup(fctx, rec); err != nil {
		return rec, err
	}
	_ = reg.AppendEvent(fctx, ref, "backup.completed", map[string]any{"id": id, "reason": bo.Reason, "stored_bytes": m.StoredBytes, "start_lsn": m.StartLSN, "stop_lsn": m.StopLSN})
	return rec, nil
}

// cleanupBackup removes whatever a failed backup uploaded.
func (s *Service) cleanupBackup(ctx context.Context, ref, id string) {
	objs, err := s.opt.Store.List(ctx, baseDir(ref)+id+"/")
	if err != nil {
		return
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	_ = s.opt.Store.Delete(ctx, keys...)
}

type serverFacts struct {
	DataDir        string
	VersionNum     int
	ArchiveMode    string
	ArchiveCommand string
	FullPageWrites string
	Tablespaces    int
	WALSegmentSize int64
	InRecovery     bool
}

func (s *Service) runBase(ctx context.Context, proj *registry.Project, id string, started time.Time, reason string) (*Manifest, error) {
	ref := proj.Ref
	dsn, err := s.opt.Access.ConnString(ctx, ref, roleAdmin)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", ref, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		conn.Close(cctx)
	}()

	var f serverFacts
	err = conn.QueryRow(ctx, `
		select current_setting('data_directory'), current_setting('server_version_num')::int,
		       current_setting('archive_mode'), current_setting('archive_command'), current_setting('full_page_writes'),
		       (select count(*) from pg_tablespace where spcname not in ('pg_default', 'pg_global')),
		       (select bytes_per_wal_segment from pg_control_init()), pg_is_in_recovery()`).
		Scan(&f.DataDir, &f.VersionNum, &f.ArchiveMode, &f.ArchiveCommand, &f.FullPageWrites, &f.Tablespaces, &f.WALSegmentSize, &f.InRecovery)
	if err != nil {
		return nil, fmt.Errorf("inspect cluster: %w", err)
	}
	switch {
	case f.InRecovery:
		return nil, errors.New("cluster is in recovery; base backups are taken from a primary")
	case f.VersionNum < 150000:
		return nil, fmt.Errorf("PostgreSQL %d is too old (pg_backup_start needs 15)", f.VersionNum)
	case f.ArchiveMode != "on" && f.ArchiveMode != "always":
		return nil, errors.New("WAL archiving is off (archive_mode): a base backup is useless without archived WAL; configure archive_mode and archive_command (backup.ArchiveSettings)")
	case strings.TrimSpace(f.ArchiveCommand) == "" || f.ArchiveCommand == "(disabled)":
		return nil, errors.New("archive_command is empty: configure it with backup.ArchiveSettings")
	case f.FullPageWrites != "on":
		return nil, errors.New("full_page_writes is off, which makes file-level backups unsafe")
	case f.Tablespaces > 0:
		return nil, ErrTablespaces
	}

	var locked bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtext('supavise.basebackup'))`).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another base backup of this project is running")
	}

	var startLSN, startWAL string
	var timeline int
	if err := conn.QueryRow(ctx, `select pg_backup_start($1, true)::text`, "supavise "+id).Scan(&startLSN); err != nil {
		return nil, fmt.Errorf("pg_backup_start: %w", err)
	}
	if err := conn.QueryRow(ctx, `select pg_walfile_name($1::pg_lsn), (select timeline_id from pg_control_checkpoint())`, startLSN).Scan(&startWAL, &timeline); err != nil {
		return nil, err
	}

	var (
		stats                    tarStats
		stopLSN, stopWAL         string
		stopTime                 time.Time
		labelFile, tablespaceMap string
	)
	dir := baseDir(ref) + id
	stored, err := s.streamTar(ctx, dir+"/"+dataName, func(tw *tar.Writer) error {
		var err error
		if stats, err = writeDataDirTar(ctx, f.DataDir, tw); err != nil {
			return err
		}
		sctx, cancel := context.WithTimeout(ctx, stopTimeout)
		defer cancel()
		if err := conn.QueryRow(sctx, `select lsn::text, labelfile, spcmapfile, pg_walfile_name(lsn), clock_timestamp() from pg_backup_stop(true)`).
			Scan(&stopLSN, &labelFile, &tablespaceMap, &stopWAL, &stopTime); err != nil {
			return fmt.Errorf("pg_backup_stop (waits for WAL archiving): %w", err)
		}
		if err := writeTarFile(tw, "backup_label", []byte(labelFile)); err != nil {
			return err
		}
		if tablespaceMap != "" {
			return writeTarFile(tw, "tablespace_map", []byte(tablespaceMap))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	m := &Manifest{
		Version: manifestVersion, ID: id, Ref: ref, Reason: reason,
		Timeline: timeline, StartLSN: startLSN, StopLSN: stopLSN, StartWAL: startWAL, StopWAL: stopWAL,
		StartTime: started, StopTime: stopTime.UTC(),
		PGVersionNum: f.VersionNum, WALSegmentSize: f.WALSegmentSize,
		Data: dataName, SizeBytes: stats.Bytes, StoredBytes: stored, Files: stats.Files,
		SupaviseVersion: s.opt.Version,
	}
	if m.Project, err = s.projectMeta(ctx, proj); err != nil {
		return nil, err
	}
	sealed, err := s.opt.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := s.writeSecretsFile(ctx, m, sealed); err != nil {
		return nil, err
	}
	if err := s.writeManifest(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) projectMeta(ctx context.Context, p *registry.Project) (*ManifestProject, error) {
	mp := &ManifestProject{Name: p.Name, Region: p.Region, Class: p.Class, Engine: string(p.Engine), Versions: p.Versions, Limits: p.Limits}
	if p.OrgID != 0 {
		o, err := s.opt.Registry.GetOrganizationByID(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		mp.OrgSlug = o.Slug
	}
	return mp, nil
}

func writeTarFile(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// streamTar runs produce against a tar writer whose output is zstd-compressed and
// streamed into the store under key, and returns the stored (compressed) size.
// If produce fails nothing is stored.
func (s *Service) streamTar(ctx context.Context, key string, produce func(*tar.Writer) error) (int64, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.opt.Store.Put(ctx, key, pr)
		pr.CloseWithError(err)
		done <- err
	}()

	cw := &countingWriter{w: pw}
	err := func() error {
		enc, err := newEncoder(cw, 2)
		if err != nil {
			return err
		}
		tw := tar.NewWriter(enc)
		if err := produce(tw); err != nil {
			enc.Close()
			return err
		}
		if err := tw.Close(); err != nil {
			enc.Close()
			return err
		}
		return enc.Close()
	}()
	pw.CloseWithError(err) // a nil err closes normally; a real one makes Put abort
	if perr := <-done; err == nil {
		err = perr
	}
	return cw.n, err
}

// writeSecretsFile stores the project's sealed secrets next to the data and records
// them in m. They are the credentials the cluster inside the backup was running
// with, which restore must reuse. They stay sealed with the node's master key.
func (s *Service) writeSecretsFile(ctx context.Context, m *Manifest, sealed map[string][]byte) error {
	if len(sealed) == 0 {
		return nil
	}
	b, err := json.Marshal(sealed) // []byte values are base64 in JSON
	if err != nil {
		return err
	}
	if err := s.opt.Store.Put(ctx, m.Dir()+"/"+secretsName, strings.NewReader(string(b))); err != nil {
		return err
	}
	m.Secrets = secretsName
	m.StoredBytes += int64(len(b))
	return nil
}

// Restore states recorded by the restore events, newest event wins.
const (
	restoreStateNone     = ""
	restoreStateFinished = "finished"
	restoreStateFailed   = "failed"
	restoreStatePending  = "pending"
)

// restoreState is what the newest restore event says about ref's recovery: finished
// (restore.recovery_finished), failed (restore.recovery_failed), pending
// (restore.cleanup_pending: recovery outlasted RecoveryTimeout) or none.
func (s *Service) restoreState(ctx context.Context, ref string) string {
	evs, err := s.opt.Registry.ListEvents(ctx, ref, 200) // newest first
	if err != nil {
		return restoreStateNone
	}
	for _, e := range evs {
		switch e.Kind {
		case eventRecoveryFinished:
			return restoreStateFinished
		case "restore.recovery_failed":
			return restoreStateFailed
		case "restore.cleanup_pending":
			return restoreStatePending
		}
	}
	return restoreStateNone
}

// neverRecovered reports whether ref is a restore-as-new clone that holds no completed
// base backup of its own and whose cluster cannot be backed up: its recovery failed
// (restore.recovery_failed) or had not finished when the restore returned
// (restore.cleanup_pending), no later restore.recovery_finished exists, and the live
// cluster is unreachable or still replaying. Such a project cannot be backed up (a
// cluster in recovery refuses, a dead one cannot be reached), and
// lifecycle.Manager.Delete treats the resulting lifecycle.ErrNoRestorableState as "skip
// the final backup". Without this the delete contract (abort when the final backup
// fails) would make the project impossible to remove.
//
// The events alone are not enough: restore reports success when recovery outlasts
// RecoveryTimeout, and the seeded recovery_target_action = 'promote' then makes the
// cluster writable on its own. So the live cluster decides. One that has left recovery
// is a working database: its recovery settings are cleared, restore.recovery_finished is
// recorded and the backup goes ahead.
func (s *Service) neverRecovered(ctx context.Context, ref string) (bool, string) {
	bs, err := s.opt.Registry.ListBackups(ctx, ref)
	if err != nil {
		return false, ""
	}
	for _, b := range bs {
		if b.Status == registry.BackupCompleted {
			return false, ""
		}
	}
	var why string
	switch s.restoreState(ctx, ref) {
	case restoreStateFailed:
		why = "its restore failed during recovery and it holds no completed base backup"
	case restoreStatePending:
		why = "its restore has not finished recovery and it holds no completed base backup"
	default:
		return false, ""
	}
	if s.settleRecovered(ctx, ref) {
		return false, ""
	}
	return true, why
}

// settleRecovered asks ref's live cluster whether it has left recovery. If so it clears
// the recovery settings (a failure is logged; the cluster is usable regardless) and
// records restore.recovery_finished, and reports true. An unreachable or still
// replaying cluster reports false.
func (s *Service) settleRecovered(ctx context.Context, ref string) bool {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inRec, err := s.probe(pctx, ref)
	if err != nil || inRec {
		return false
	}
	if err := s.alter(pctx, ref, recoveryGUCs); err != nil {
		s.opt.Log.Warn("restored cluster left recovery but its recovery settings could not be cleared", "ref", ref, "err", err)
		return true
	}
	_ = s.opt.Registry.AppendEvent(context.WithoutCancel(ctx), ref, eventRecoveryFinished, nil)
	return true
}

// PendingRestores lists the projects whose newest restore event is
// restore.cleanup_pending: restored clones whose recovery outlasted RecoveryTimeout.
func (s *Service) PendingRestores(ctx context.Context) []string {
	if s.opt.Registry == nil {
		return nil
	}
	ps, err := s.opt.Registry.ListProjects(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		if p.Ref != config.SystemRef && s.restoreState(ctx, p.Ref) == restoreStatePending {
			out = append(out, p.Ref)
		}
	}
	return out
}

// FinishPendingRestores completes every active project whose restore reported
// restore.cleanup_pending: it waits (up to RecoveryTimeout each) for the cluster to leave
// recovery, clears the recovery settings and records restore.recovery_finished. The
// daemon runs it after it starts the projects and repeats it while any stays pending, so
// a slow restore-as-new does not depend on someone running `supavise backups
// finish-restore`. It returns the refs it finished.
func (s *Service) FinishPendingRestores(ctx context.Context) []string {
	if s.opt.Access == nil {
		return nil
	}
	var done []string
	for _, ref := range s.PendingRestores(ctx) {
		p, err := s.opt.Registry.GetProject(ctx, ref)
		if err != nil || (p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy) {
			continue
		}
		if err := s.FinishRestore(ctx, ref); err != nil {
			s.opt.Log.Warn("restore still not finished", "ref", ref, "err", err)
			continue
		}
		done = append(done, ref)
	}
	return done
}
