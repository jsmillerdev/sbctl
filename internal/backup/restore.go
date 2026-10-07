package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// ErrForceRequired is returned when a restore would replace a project's own data and
// RestoreOptions.Force is not set.
var ErrForceRequired = errors.New("backup: restoring in place replaces the project's data with an older copy; pass Force to confirm")

// Restore targets. A time target stops at the first commit or abort record after the
// time, so it needs a transaction that ended after it in the archive; the other two do not.
const (
	RestoreToTime   = "time"   // replay to a point in time
	RestoreToLatest = "latest" // replay every archived WAL file, then promote
	RestoreToBackup = "backup" // stop at the end of the chosen base backup (recovery_target = immediate)
)

// RestoreOptions tunes RestoreWith.
type RestoreOptions struct {
	// Force allows restoring over the source project itself (newRef empty or equal to ref).
	// The old data directory is kept next to it as <dir>.pre-restore-<time>.
	Force bool
	// BackupID pins the base backup instead of choosing the newest one before the target.
	BackupID string
	// Latest restores to the end of the archive and ignores the target time. It is the
	// way to restore a deleted project's last state from its final backup, and to
	// restore "now" on a running project (its newest WAL is archived first).
	Latest bool
	// ToBackup restores exactly the state of a base backup (BackupID, default the
	// newest) and ignores the target time. It needs no WAL beyond the backup's own.
	ToBackup bool
}

func (o RestoreOptions) mode() (string, error) {
	switch {
	case o.Latest && o.ToBackup:
		return "", errors.New("backup: Latest and ToBackup are exclusive")
	case o.Latest:
		return RestoreToLatest, nil
	case o.ToBackup:
		return RestoreToBackup, nil
	}
	return RestoreToTime, nil
}

// RestorePlan is the outcome of choosing a base backup for a target.
type RestorePlan struct {
	Source    string    // ref whose archive is replayed
	TargetRef string    // ref the restored cluster archives to (Source for in-place)
	Mode      string    // RestoreToTime (default), RestoreToLatest or RestoreToBackup
	Target    time.Time // for RestoreToTime
	Manifest  Manifest
}

// PlanRestore picks the newest base backup of ref that finished at or before target
// (or backupID) and checks that the WAL it starts from is in the archive.
func (s *Service) PlanRestore(ctx context.Context, ref string, target time.Time, backupID string) (*RestorePlan, error) {
	return s.PlanRestoreWith(ctx, ref, target, RestoreOptions{BackupID: backupID})
}

// farFuture stands in for "no upper bound" when choosing the newest base backup.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// PlanRestoreWith is PlanRestore for any restore target. Only base backups on the
// archive's current timeline history qualify: after an in-place restore forked timeline
// 2 off at T, a backup of timeline 1 taken after T is in the archive but cannot be the
// base of a restore that follows timeline 2.
func (s *Service) PlanRestoreWith(ctx context.Context, ref string, target time.Time, opts RestoreOptions) (*RestorePlan, error) {
	mode, err := opts.mode()
	if err != nil {
		return nil, err
	}
	all, err := s.ListBackups(ctx, ref)
	if err != nil {
		return nil, err
	}
	hist, err := s.latestHistory(ctx, ref)
	if err != nil {
		return nil, err
	}
	var eligible []Manifest
	for i := range all {
		if hist.onHistory(&all[i]) {
			eligible = append(eligible, all[i])
		} else if all[i].ID == opts.BackupID {
			return nil, fmt.Errorf("backup: backup %s (timeline %d, ended at %s) is not on the history of timeline %d: a restore from it cannot follow the later timeline",
				all[i].ID, all[i].Timeline, all[i].StopLSN, hist.Latest)
		}
	}
	if len(all) > 0 && len(eligible) == 0 {
		return nil, fmt.Errorf("backup: none of the %d base backups of %s is on the history of timeline %d; take a new base backup", len(all), ref, hist.Latest)
	}
	pick := target
	if mode != RestoreToTime {
		pick = farFuture
	}
	m, err := pickBackup(eligible, pick, opts.BackupID)
	if err != nil {
		return nil, err
	}
	if _, err := s.opt.Store.Stat(ctx, walKey(ref, m.StartWAL)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("backup: WAL file %s that backup %s starts from is not in the archive", m.StartWAL, m.ID)
		}
		return nil, err
	}
	return &RestorePlan{Source: ref, TargetRef: ref, Mode: mode, Target: target, Manifest: *m}, nil
}

// Restore implements Backup. With newRef set it creates a new project; with newRef
// empty it refuses, because restoring in place needs RestoreWith and Force.
func (s *Service) Restore(ctx context.Context, ref string, target time.Time, newRef string) (*registry.Project, error) {
	return s.RestoreWith(ctx, ref, target, newRef, RestoreOptions{})
}

// RestoreWith restores ref to target.
//
// New project (newRef set and different from ref): lifecycle.Manager.Create builds
// project newRef with a DataSeeder that unpacks the base backup instead of running
// initdb and writes the recovery settings. The restored cluster already contains the
// source's role passwords and Vault data, so the source's database passwords and
// pgsodium root key are reused. Everything an API client can hold is new: a fresh
// JWT secret, legacy keys signed for newRef, and new publishable and secret keys.
//
// In place (newRef empty or equal to ref, Force set): the project is paused, its
// data directory moved aside, the seeder fills a fresh one, and the project resumes.
func (s *Service) RestoreWith(ctx context.Context, ref string, target time.Time, newRef string, opts RestoreOptions) (*registry.Project, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if ref == config.SystemRef {
		return nil, errors.New("backup: the system cluster holds the registry that a restore needs to run; restore it by hand with the control plane stopped (README, \"Disaster recovery of the system cluster\")")
	}
	mode, err := opts.mode()
	if err != nil {
		return nil, err
	}
	for _, c := range []struct {
		what string
		ok   bool
	}{{"registry", s.opt.Registry != nil}, {"lifecycle manager", s.opt.Manager != nil}, {"secrets", s.opt.Secrets != nil}, {"config", s.opt.Config != nil}} {
		if err := s.need(c.what, c.ok); err != nil {
			return nil, err
		}
	}
	inPlace := newRef == "" || newRef == ref
	if inPlace && !opts.Force {
		return nil, ErrForceRequired
	}
	if !inPlace {
		if !secrets.ValidRef(newRef) {
			return nil, fmt.Errorf("backup: invalid new project ref %q", newRef)
		}
		if _, err := s.opt.Registry.GetProject(ctx, newRef); err == nil {
			return nil, fmt.Errorf("backup: project %s already exists", newRef)
		} else if !errors.Is(err, registry.ErrNotFound) {
			return nil, err
		}
		// A deleted project keeps its archive. Reusing its ref would mix two histories:
		// the clone's WAL would collide with the old one and archiving would stall.
		if left, err := s.opt.Store.ListDirs(ctx, newRef+"/"); err != nil {
			return nil, err
		} else if len(left) > 0 {
			return nil, fmt.Errorf("backup: the archive already holds data for ref %s (%s); a deleted project's backups are kept, so restore under a different ref", newRef, strings.Join(left, ", "))
		}
	}

	// A running source holds its newest transactions in a WAL segment that is not archived
	// until it fills or archive_timeout passes. A time target needs a commit after it in the
	// archive, and "latest" wants everything, so archive that segment first. A backup target
	// needs only the backup's own WAL.
	if mode != RestoreToBackup {
		s.flushArchive(ctx, ref)
	}
	plan, err := s.PlanRestoreWith(ctx, ref, target, opts)
	if err != nil {
		return nil, err
	}

	var p *registry.Project
	if inPlace {
		p, err = s.restoreInPlace(ctx, plan)
	} else {
		var keys *secrets.ProjectKeys
		if keys, err = s.sourceKeys(ctx, &plan.Manifest); err != nil {
			return nil, err
		}
		plan.TargetRef = newRef
		p, err = s.restoreAsNew(ctx, plan, keys)
	}
	if err != nil {
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.failed", map[string]any{"mode": mode, "target": target, "as": newRef, "error": err.Error()})
		return nil, err
	}
	_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.completed", map[string]any{"mode": mode, "target": target, "as": p.Ref, "backup": plan.Manifest.ID})
	return p, nil
}

func (s *Service) restoreAsNew(ctx context.Context, plan *RestorePlan, src *secrets.ProjectKeys) (*registry.Project, error) {
	k := *src
	k.JWTSecret = secrets.NewJWTSecret()
	k.PublishableKey = secrets.NewPublishableKey()
	k.SecretKey = secrets.NewSecretKey()
	if err := k.ResignLegacy(plan.TargetRef, s.opt.Now()); err != nil {
		return nil, err
	}
	req := lifecycle.CreateRequest{
		Name: "restore of " + plan.Source, Region: "local", Ref: plan.TargetRef,
		DBPassword: k.DBPassword, Seed: s.Seeder(plan), Keys: &k,
	}
	if mp := plan.Manifest.Project; mp != nil {
		req.Name = mp.Name + " (restored)"
		req.OrgSlug, req.Region, req.Class = mp.OrgSlug, mp.Region, mp.Class
		lim := mp.Limits
		req.Limits = &lim
	}
	p, err := s.opt.Manager.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("backup: create restored project %s: %w", plan.TargetRef, err)
	}
	if _, err := s.resetRecoverySettings(ctx, plan.TargetRef); err != nil {
		return nil, fmt.Errorf("backup: project %s was created but its recovery did not finish, so it holds no usable data (it is registered so that its postgres log can be read; `sbctl projects delete` removes it without a final backup, since there is nothing to back up): %w", plan.TargetRef, err)
	}
	return p, nil
}

func (s *Service) restoreInPlace(ctx context.Context, plan *RestorePlan) (*registry.Project, error) {
	ref := plan.Source
	proj, err := s.opt.Registry.GetProject(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("backup: project %s: %w", ref, err)
	}
	if s.opt.DataDir == nil {
		return nil, errors.New("backup: no data directory resolver configured")
	}
	dataDir := s.opt.DataDir(ref)
	if err := s.checkDataDir(ctx, ref, dataDir); err != nil {
		return nil, err
	}
	s.warnPasswordDrift(ctx, ref, &plan.Manifest)
	if err := s.opt.Manager.Pause(ctx, ref); err != nil {
		return nil, fmt.Errorf("backup: stop %s: %w", ref, err)
	}
	stamp := s.opt.Now().UTC().Format("20060102T150405Z")
	aside := dataDir + ".pre-restore-" + stamp
	if err := os.Rename(dataDir, aside); err != nil {
		_ = s.opt.Manager.Resume(ctx, ref)
		return nil, fmt.Errorf("backup: move data directory aside: %w", err)
	}
	if err := s.Seeder(plan)(ctx, proj, dataDir); err != nil {
		os.RemoveAll(dataDir)
		rerr := os.Rename(aside, dataDir)
		if rerr == nil {
			rerr = s.opt.Manager.Resume(ctx, ref)
		}
		if rerr != nil {
			return nil, fmt.Errorf("backup: seed failed (%v) and the original could not be put back (%v); original data is in %s", err, rerr, aside)
		}
		return nil, fmt.Errorf("backup: seed data directory (original restored): %w", err)
	}
	failed := dataDir + ".failed-restore-" + stamp
	if err := s.opt.Manager.Resume(ctx, ref); err != nil {
		// The restored cluster did not come up. The usual cause is the fatal "recovery
		// ended before configured recovery target was reached", which kills the postmaster
		// before the lifecycle readiness check sees it accept connections, so it surfaces
		// here and not below. Same treatment as a recovery that fails later: put the
		// original back, or the production project stays down on a failed restore.
		if rerr := s.rollbackInPlace(ctx, ref, dataDir, aside, failed); rerr != nil {
			return nil, fmt.Errorf("backup: start %s after restore failed (%v) and the original could not be put back (%v); original data is in %s, the failed restore in %s", ref, err, rerr, aside, dataDir)
		}
		return nil, fmt.Errorf("backup: start %s after restore failed; the original data is back in place and the project is running on it again, the failed restore is kept in %s: %w", ref, failed, err)
	}
	done, err := s.resetRecoverySettings(ctx, ref)
	if err != nil {
		// Recovery ended in a fatal error (typically: no commit after the target time in
		// the archive). Put the original data back rather than leave a dead project.
		if rerr := s.rollbackInPlace(ctx, ref, dataDir, aside, failed); rerr != nil {
			return nil, fmt.Errorf("backup: recovery of %s did not finish (%v) and the original could not be put back (%v); original data is in %s, the failed restore in %s", ref, err, rerr, aside, dataDir)
		}
		return nil, fmt.Errorf("backup: recovery of %s did not finish; the original data is back in place and the failed restore is kept in %s: %w", ref, failed, err)
	}
	if done {
		// The new timeline has no base backup yet, and the ones of the old timeline taken
		// after the fork are unusable for it. Take one now rather than at the next timer run.
		if _, err := s.BaseBackupWith(ctx, ref, BackupOptions{Reason: ReasonRestore}); err != nil {
			s.opt.Log.Warn("base backup after in-place restore failed; run `sbctl backups create`", "ref", ref, "err", err)
			_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.backup_pending", map[string]any{"error": err.Error()})
		}
	} else {
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.backup_pending", map[string]any{"error": "recovery had not finished"})
	}
	s.opt.Log.Info("restored in place", "ref", ref, "original_data", aside)
	return s.opt.Registry.GetProject(ctx, ref)
}

// rollbackInPlace undoes a failed in-place restore: stops the project (it may already be
// dead), moves the restored directory to failed, puts the original back and resumes.
func (s *Service) rollbackInPlace(ctx context.Context, ref, dataDir, aside, failed string) error {
	_ = s.opt.Manager.Pause(ctx, ref)
	if err := os.Rename(dataDir, failed); err != nil {
		return err
	}
	if err := os.Rename(aside, dataDir); err != nil {
		return err
	}
	return s.opt.Manager.Resume(ctx, ref)
}

// checkDataDir refuses an in-place restore unless dataDir is the cluster's data
// directory: it must hold PG_VERSION and, when the cluster is reachable, be the
// directory the server itself reports. Base backups read the server's data_directory,
// so a resolver that points at a parent directory would otherwise move the wrong tree.
func (s *Service) checkDataDir(ctx context.Context, ref, dataDir string) error {
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err != nil {
		return fmt.Errorf("backup: %s is not a PostgreSQL data directory (no PG_VERSION): %w; refusing to replace it", dataDir, err)
	}
	if s.opt.Access == nil {
		return nil
	}
	conn, err := s.adminConn(ctx, ref)
	if err != nil {
		return nil // not running: the PG_VERSION check is all there is
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var actual string
	if err := conn.QueryRow(ctx, "select current_setting('data_directory')").Scan(&actual); err != nil {
		return nil
	}
	if !samePath(actual, dataDir) {
		return fmt.Errorf("backup: the server of %s runs on %s, not on %s; refusing to replace the wrong directory", ref, actual, dataDir)
	}
	return nil
}

// samePath compares two directories after resolving symlinks where possible.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// warnPasswordDrift records when the registry's database passwords differ from the ones
// the backup holds. An in-place restore brings the old passwords back inside the
// cluster while the registry, GoTrue and PostgREST keep the current ones, so the services
// would fail to log in. Only a password reset between backup and restore causes this
// (RotateKeys does not touch database passwords).
func (s *Service) warnPasswordDrift(ctx context.Context, ref string, m *Manifest) {
	old, err := s.sourceKeys(ctx, m)
	if err != nil {
		return
	}
	sealed, err := s.opt.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return
	}
	plain := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := s.opt.Secrets.Open(blob)
		if err != nil {
			return
		}
		plain[name] = string(pt)
	}
	cur := secrets.KeysFromMap(plain)
	if cur.DBPassword == old.DBPassword && cur.AdminPassword == old.AdminPassword &&
		cur.AuthenticatorPassword == old.AuthenticatorPassword && cur.AuthAdminPassword == old.AuthAdminPassword &&
		cur.StorageAdminPassword == old.StorageAdminPassword && cur.ReplicationPassword == old.ReplicationPassword {
		return
	}
	s.opt.Log.Warn("database passwords changed since the backup; the restored cluster has the old ones and the services will fail to log in until they are reset", "ref", ref, "backup", m.ID)
	_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.password_drift", map[string]any{"backup": m.ID})
}

// errStillRecovering means the cluster did not leave recovery within the wait.
var errStillRecovering = errors.New("cluster is still in recovery")

// errRecoveryFailed means the cluster stopped answering after it had been in recovery, or
// never answered at all: PostgreSQL ended recovery with a fatal error (most often "recovery
// ended before configured recovery target was reached") and shut down.
var errRecoveryFailed = errors.New("cluster stopped before recovery finished")

// resetRecoverySettings waits for the restored cluster to finish recovery, then removes
// the recovery settings the seeder put in postgresql.auto.conf, so a later start never
// replays another project's archive. It reports whether the settings were cleared.
//
// Clearing restore_command while recovery still runs is not safe: it is a reloadable
// setting, recovery would run out of WAL before its target, and the cluster would stop
// with a fatal error and refuse to start again (recovery.signal present, no
// restore_command). Manager.Create and Resume only promise a running cluster, which
// with hot_standby on is also a cluster that is still replaying. So nothing is reset
// until pg_is_in_recovery() is false. If that does not happen in RecoveryTimeout the
// settings stay and a restore.cleanup_pending event is recorded; FinishRestore finishes
// the job later. A slow recovery is logged and recorded, not returned: the project is
// restored. A cluster that died during recovery is returned as an error wrapping
// errRecoveryFailed: the restore did not produce a project.
func (s *Service) resetRecoverySettings(ctx context.Context, ref string) (bool, error) {
	err := s.finishRecovery(ctx, ref, s.opt.RecoveryTimeout)
	switch {
	case err == nil:
		_ = s.opt.Registry.AppendEvent(ctx, ref, eventRecoveryFinished, nil)
		return true, nil
	case errors.Is(err, errRecoveryFailed):
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.recovery_failed", map[string]any{"error": err.Error()})
		return false, err
	case errors.Is(err, errStillRecovering):
		s.opt.Log.Warn("restored cluster is still in recovery; recovery settings left in place (run `sbctl backups finish-restore`)", "ref", ref, "err", err)
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.cleanup_pending", map[string]any{"error": err.Error()})
	default:
		s.opt.Log.Warn("could not clear recovery settings after restore", "ref", ref, "err", err)
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.cleanup_failed", map[string]any{"error": err.Error()})
	}
	return false, nil
}

// FinishRestore waits up to RecoveryTimeout for ref's cluster to finish recovery and
// then clears the recovery settings. It completes a restore that reported
// restore.cleanup_pending, and is safe to run on a cluster that is already done.
func (s *Service) FinishRestore(ctx context.Context, ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if err := s.need("database access", s.opt.Access != nil); err != nil {
		return err
	}
	if err := s.finishRecovery(ctx, ref, s.opt.RecoveryTimeout); err != nil {
		return err
	}
	if s.opt.Registry != nil {
		_ = s.opt.Registry.AppendEvent(ctx, ref, eventRecoveryFinished, nil)
	}
	return nil
}

// eventRecoveryFinished is recorded when a restored cluster has left recovery and its
// recovery settings are cleared. It supersedes restore.recovery_failed and
// restore.cleanup_pending in neverRecovered.
const eventRecoveryFinished = "restore.recovery_finished"

// finishRecovery polls until the cluster has left recovery (connecting as often as
// needed, since the server may still be starting), then resets recoveryGUCs. A server
// that was seen in recovery and then stays unreachable for RecoveryFailGrace has died,
// and one that never answers within wait never started: both are errRecoveryFailed.
func (s *Service) finishRecovery(ctx context.Context, ref string, wait time.Duration) error {
	if s.opt.Access == nil {
		return errors.New("no database access configured")
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var (
		last      error = errStillRecovering
		seenUp    bool
		downSince time.Time
	)
	for {
		inRec, err := s.probe(wctx, ref)
		switch {
		case err != nil:
			last = err
			if seenUp {
				if downSince.IsZero() {
					downSince = time.Now()
				} else if time.Since(downSince) >= s.opt.RecoveryFailGrace {
					return fmt.Errorf("%w (unreachable for %s while recovering: %v)", errRecoveryFailed, s.opt.RecoveryFailGrace, err)
				}
			}
		case inRec:
			seenUp, downSince, last = true, time.Time{}, errStillRecovering
		default:
			return s.alter(ctx, ref, recoveryGUCs)
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !seenUp {
				return fmt.Errorf("%w (never accepted a connection in %s: %v)", errRecoveryFailed, wait, last)
			}
			return fmt.Errorf("%w after %s (last: %v)", errStillRecovering, wait, last)
		case <-time.After(s.opt.RecoveryPoll):
		}
	}
}

// pgInRecovery asks ref's cluster whether it is still replaying WAL.
func (s *Service) pgInRecovery(ctx context.Context, ref string) (bool, error) {
	conn, err := s.adminConn(ctx, ref)
	if err != nil {
		return true, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var rec bool
	err = conn.QueryRow(ctx, "select pg_is_in_recovery()").Scan(&rec)
	return rec, err
}

// pgResetSettings runs ALTER SYSTEM RESET for each setting and reloads the configuration.
func (s *Service) pgResetSettings(ctx context.Context, ref string, gucs []string) error {
	conn, err := s.adminConn(ctx, ref)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	for _, g := range gucs {
		if _, err := conn.Exec(ctx, "alter system reset "+g); err != nil {
			return err
		}
	}
	_, err = conn.Exec(ctx, "select pg_reload_conf()")
	return err
}

func (s *Service) adminConn(ctx context.Context, ref string) (*pgx.Conn, error) {
	dsn, err := s.opt.Access.ConnString(ctx, ref, roleAdmin)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pgx.Connect(cctx, dsn)
}

// flushArchive makes a running source archive its newest WAL before a restore to a time
// or to the end of the archive: it switches to a new segment and waits for the archiver to
// catch up. Best effort: a source that is down or gone (a deleted project) has nothing
// to flush, and a restore must not fail because of it.
func (s *Service) flushArchive(ctx context.Context, ref string) {
	if s.opt.Access == nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, s.opt.ArchiveFlushTimeout)
	defer cancel()
	conn, err := s.adminConn(fctx, ref)
	if err != nil {
		s.opt.Log.Debug("source cluster not reachable; restoring what is archived", "ref", ref, "err", err)
		return
	}
	defer conn.Close(context.WithoutCancel(ctx))
	// A time target needs a commit record after it in the archive, and an idle source has
	// none. Forcing an xid and committing it (autocommit) writes one: it costs one xid, and
	// makes every target before now reachable.
	if _, err := conn.Exec(fctx, "select pg_current_xact_id()"); err != nil {
		s.opt.Log.Debug("could not write a commit record on the source", "ref", ref, "err", err)
	}
	// pg_switch_wal() returns the end of the segment it closed, or the start of the current
	// one when nothing was written since the last switch; minus one byte is the last
	// segment that must be archived either way.
	var seg string
	if err := conn.QueryRow(fctx, "select pg_walfile_name(pg_switch_wal() - 1)").Scan(&seg); err != nil {
		s.opt.Log.Warn("could not switch WAL on the source", "ref", ref, "err", err)
		return
	}
	for {
		var ok bool
		if err := conn.QueryRow(fctx, "select coalesce(last_archived_wal >= $1, false) from pg_stat_archiver", seg).Scan(&ok); err == nil && ok {
			return
		}
		select {
		case <-fctx.Done():
			s.opt.Log.Warn("the source's newest WAL is not archived yet; the restore may end earlier than the last transaction", "ref", ref, "segment", seg)
			return
		case <-time.After(s.opt.RecoveryPoll):
		}
	}
}

// recoveryGUCs are the settings the seeder writes for the recovery itself.
var recoveryGUCs = []string{"restore_command", "recovery_target", "recovery_target_time", "recovery_target_action", "recovery_target_timeline"}

// sourceKeys opens the credentials the restored cluster was running with: the sealed
// copy stored in the backup, or, for backups without one, the source project's
// current registry secrets.
func (s *Service) sourceKeys(ctx context.Context, m *Manifest) (*secrets.ProjectKeys, error) {
	sealed := map[string][]byte{}
	if m.Secrets != "" {
		rc, err := s.opt.Store.Get(ctx, m.Dir()+"/"+m.Secrets)
		if err != nil {
			return nil, fmt.Errorf("backup: read secrets of backup %s: %w", m.ID, err)
		}
		defer rc.Close()
		if err := json.NewDecoder(rc).Decode(&sealed); err != nil {
			return nil, fmt.Errorf("backup: secrets of backup %s: %w", m.ID, err)
		}
	} else if sealedReg, err := s.opt.Registry.GetSecrets(ctx, m.Ref); err == nil {
		sealed = sealedReg
	}
	plain := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := s.opt.Secrets.Open(blob)
		if err != nil {
			return nil, fmt.Errorf("backup: open secret %s (is this node's master key the one that sealed the backup?): %w", name, err)
		}
		plain[name] = string(pt)
	}
	k := secrets.KeysFromMap(plain)
	if k.DBPassword == "" || k.AdminPassword == "" || k.PGSodiumRootKey == "" {
		return nil, fmt.Errorf("backup: no database credentials available for %s; they are needed to open the restored cluster", m.Ref)
	}
	return k, nil
}

// Seeder returns the lifecycle.DataSeeder that turns an empty directory into the
// restored cluster: it unpacks the base backup, writes recovery.signal and the
// recovery settings, and points the cluster's own archiving at plan.TargetRef so a
// restored clone never writes into the source's archive.
func (s *Service) Seeder(plan *RestorePlan) lifecycle.DataSeeder {
	return func(ctx context.Context, _ *registry.Project, dataDir string) error {
		return s.seed(ctx, plan, dataDir)
	}
}

func (s *Service) seed(ctx context.Context, plan *RestorePlan, dataDir string) error {
	m := &plan.Manifest
	if ents, err := os.ReadDir(dataDir); err == nil && len(ents) > 0 {
		return fmt.Errorf("backup: refusing to restore into non-empty directory %s", dataDir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	rc, err := s.opt.Store.Get(ctx, m.Dir()+"/"+m.Data)
	if err != nil {
		return fmt.Errorf("backup: read base backup %s: %w", m.ID, err)
	}
	defer rc.Close()
	dec, err := newDecoder(rc)
	if err != nil {
		return err
	}
	defer dec.Close()
	if err := extractTar(ctx, tar.NewReader(dec), dataDir); err != nil {
		return fmt.Errorf("backup: unpack base backup %s: %w", m.ID, err)
	}

	// The Supabase launcher refuses an existing cluster without a non-empty
	// postmaster.opts, and pg_basebackup-style backups omit it. Postgres rewrites it.
	if err := writeSyncFile(filepath.Join(dataDir, "postmaster.opts"), []byte("# recreated by sbctl restore\n"), 0o600); err != nil {
		return err
	}
	conf, err := os.OpenFile(filepath.Join(dataDir, "postgresql.auto.conf"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := conf.WriteString(s.recoveryConf(plan)); err != nil {
		conf.Close()
		return err
	}
	if err := conf.Sync(); err != nil {
		conf.Close()
		return err
	}
	if err := conf.Close(); err != nil {
		return err
	}
	// recovery.signal last: it is what turns the start into a recovery.
	if err := writeSyncFile(filepath.Join(dataDir, "recovery.signal"), nil, 0o600); err != nil {
		return err
	}
	return syncTree(dataDir)
}

// recoveryConf is the block appended to postgresql.auto.conf. Later lines win, so it
// overrides anything the source's own ALTER SYSTEM history left there.
//
// The archive_* lines stay after the restore: the data directory's postgresql.conf is
// the source's, and without them the clone would archive into the source's archive.
func (s *Service) recoveryConf(plan *RestorePlan) string {
	bin, cfgPath := config.DefaultBinPath, s.opt.ConfigPath
	c := s.opt.Config
	if c != nil && c.BinPath != "" {
		bin = c.BinPath
	}
	var b strings.Builder
	switch plan.Mode {
	case RestoreToLatest:
		fmt.Fprintf(&b, "\n# --- sbctl restore of %s (backup %s) to the end of the archive ---\n", plan.Source, plan.Manifest.ID)
	case RestoreToBackup:
		fmt.Fprintf(&b, "\n# --- sbctl restore of %s to the end of backup %s ---\n", plan.Source, plan.Manifest.ID)
	default:
		fmt.Fprintf(&b, "\n# --- sbctl restore of %s (backup %s) to %s ---\n", plan.Source, plan.Manifest.ID, plan.Target.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "archive_mode = on\narchive_command = %s\n", confString(ArchiveCommand(bin, plan.TargetRef, cfgPath)))
	fmt.Fprintf(&b, "restore_command = %s\n", confString(RestoreCommand(bin, plan.Source, cfgPath)))
	switch plan.Mode {
	case RestoreToLatest:
		// No target: recovery replays every archived file and then promotes.
	case RestoreToBackup:
		b.WriteString("recovery_target = 'immediate'\nrecovery_target_action = 'promote'\n")
	default:
		fmt.Fprintf(&b, "recovery_target_time = %s\n", confString(plan.Target.UTC().Format("2006-01-02 15:04:05.999999")+"+00"))
		b.WriteString("recovery_target_action = 'promote'\n")
	}
	b.WriteString("recovery_target_timeline = 'latest'\n")
	return b.String()
}

func writeSyncFile(p string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// extractTar unpacks tr under root. Only directories and regular files are accepted,
// and every path must stay inside root. Files are fsynced as they are written.
func extractTar(ctx context.Context, tr *tar.Reader, root string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", hdr.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		perm := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(dst, perm); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, ctxReader{ctx, tr})
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported entry type %q for %s", hdr.Typeflag, name)
		}
	}
}

// syncTree fsyncs every directory under root, deepest first, so the new tree is durable.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(root))
}
