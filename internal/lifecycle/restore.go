package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// RestoreRequest says what an in-place restore brings back.
type RestoreRequest struct {
	// Target is the point in time to restore to.
	Target time.Time
	// BackupID is the base backup (the backup service's manifest id) the restore starts from.
	// Alone, it restores exactly the state at the end of that backup; with Target, it replays
	// WAL from that backup up to Target.
	BackupID string
}

// InPlaceRestorer restores a project's database over itself. backup.Service implements it;
// the Engine finds it through its BaseBackuper, as it does FinalBackuper.
type InPlaceRestorer interface {
	RestoreInPlace(ctx context.Context, ref string, req RestoreRequest) error
}

// DatabaseRestorer is the optional Manager capability behind the dashboard's restore and
// the Management API's restore routes. The Engine has it; the API asks the Manager for it.
type DatabaseRestorer interface {
	BeginRestore(ctx context.Context, ref string) (RestoreRun, error)
}

// RestoreRun is a restore that BeginRestore started: the project is RESTORING until Run returns.
type RestoreRun interface {
	Run(ctx context.Context, req RestoreRequest) error
}

var _ DatabaseRestorer = (*Engine)(nil)

// ErrNoRestorer is returned by BeginRestore on a node whose backups are not configured.
var ErrNoRestorer = errors.New("lifecycle: this node has no backup service, so there is nothing to restore from")

// EventRestoreRequested records that a restore began through the Engine (the Management
// API). The backup service writes restore.completed or restore.failed when it ends.
const EventRestoreRequested = "restore.requested"

// EventRestorePasswordsFailed records that the role passwords of a restored cluster could not
// be set to the registry's (see reapplyRolePasswords).
const EventRestorePasswordsFailed = "restore.passwords_failed"

// restoringKey marks a context as belonging to an in-place restore, whose Pause and Resume
// calls must leave the project RESTORING instead of walking it through PAUSING, INACTIVE and
// COMING_UP.
type restoringKey struct{}

func withRestoring(ctx context.Context) context.Context {
	return context.WithValue(ctx, restoringKey{}, true)
}

func restoring(ctx context.Context) bool {
	v, _ := ctx.Value(restoringKey{}).(bool)
	return v
}

// inRestore reports whether ctx is the context of the restore that holds p in RESTORING.
func inRestore(ctx context.Context, p *registry.Project) bool {
	return restoring(ctx) && p.Status == registry.StatusRestoring
}

// setStatus is SetProjectStatus except inside a restore, where the status stays RESTORING
// until Restore.Run settles it.
func (e *Engine) setStatus(ctx context.Context, ref string, s registry.Status) error {
	if restoring(ctx) {
		return nil
	}
	return e.reg.SetProjectStatus(ctx, ref, s)
}

// restoreChecker is implemented by a BaseBackuper that builds its backup service late and
// can therefore say, before a restore starts, whether it will be able to run one.
type restoreChecker interface{ CanRestore() error }

// restorer returns the in-place restore capability of the Engine's backup service.
func (e *Engine) restorer() InPlaceRestorer {
	r, _ := e.opts.Backup.(InPlaceRestorer)
	return r
}

// Restore is the Engine's RestoreRun.
type Restore struct {
	e   *Engine
	ref string
}

// BeginRestore moves an active project, or one whose last restore failed, to RESTORING and
// returns the handle that runs the restore. It refuses (ErrInvalidState) a project that is
// paused, starting, being deleted or already being restored, so two operations never overlap: every other operation checks the
// status under the same per-project lock. It refuses with ErrInsufficientDisk when the disk
// cannot hold the restored copy next to the current data. The caller must call Run; until
// then the project stays RESTORING.
func (e *Engine) BeginRestore(ctx context.Context, ref string) (RestoreRun, error) {
	if e.restorer() == nil {
		return nil, ErrNoRestorer
	}
	if c, ok := e.opts.Backup.(restoreChecker); ok {
		if err := c.CanRestore(); err != nil {
			return nil, err
		}
	}
	if ref == config.SystemRef {
		return nil, fmt.Errorf("%w: the system project holds the registry and cannot be restored this way", ErrInvalidState)
	}
	if err := e.upgradeBusy(ref, "restore"); err != nil {
		return nil, err
	}
	// The data directory is measured before the lock, so a large one does not hold up the
	// project's other operations; the status checks below still come first in the answer.
	spaceErr := e.checkRestoreSpace(ref)
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := e.onHome(p, "restore", "a restore replaces the data directory of its home; move the project to this node first"); err != nil {
		return nil, err
	}
	if !active(p.Status) && p.Status != registry.StatusRestoreFailed {
		return nil, invalidState(p, "restore")
	}
	if spaceErr != nil {
		return nil, spaceErr
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusRestoring); err != nil {
		return nil, err
	}
	e.restoring.Store(ref, struct{}{})
	return &Restore{e: e, ref: ref}, nil
}

// Run restores the project in place and settles its status: ACTIVE_HEALTHY when the restore
// worked, RESTORE_FAILED when it did not. RESTORE_FAILED stays whether or not the backup
// service managed to put the original data back, because a status that returned to
// ACTIVE_HEALTHY would look to the dashboard and to API clients like a restore that worked.
// The project leaves RESTORE_FAILED through another restore, a pause (then a resume), or a
// delete; health checks leave it alone. After a restore that worked it also sets the cluster's role passwords to the registry's, and it
// bounds the old data directories the restores left (pruneRestoreLeftovers). The outcome is
// the backup service's error, if any.
func (r *Restore) Run(ctx context.Context, req RestoreRequest) error {
	e := r.e
	defer e.restoring.Delete(r.ref)
	e.event(ctx, r.ref, EventRestoreRequested, map[string]any{"target": req.Target, "backup": req.BackupID})
	err := e.restorer().RestoreInPlace(withRestoring(ctx), r.ref, req)

	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err == nil {
		e.reapplyRolePasswords(cctx, r.ref)
	}
	e.pruneRestoreLeftovers(r.ref, err == nil)
	if serr := e.settleRestore(cctx, r.ref, err == nil); serr != nil {
		e.log.Warn("restore: could not settle the project status", "ref", r.ref, "error", serr)
	}
	if err != nil {
		return fmt.Errorf("lifecycle: restore %s: %w", r.ref, err)
	}
	return nil
}

// rolePasswordPlane is implemented by a data plane that can set every service role's password
// in a running cluster from the project's keys; PostgresPlane has it.
type rolePasswordPlane interface {
	SetRolePasswords(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error
}

// reapplyRolePasswords gives the restored cluster the role passwords the registry holds. A
// restore to a time before a database password reset brings the old password back inside the
// cluster, and the registry, GoTrue and PostgREST keep the current ones: this puts the
// cluster back in line, the invariant SetDatabasePassword keeps. A failure is logged and
// recorded as a restore.passwords_failed event; the restore itself stands.
func (e *Engine) reapplyRolePasswords(ctx context.Context, ref string) {
	pp, ok := e.plane.(rolePasswordPlane)
	if !ok {
		return
	}
	fail := func(err error) {
		e.log.Warn("restore: could not set the role passwords of the restored cluster to the current ones; the services may fail to log in until the database password is reset", "ref", ref, "error", err)
		e.event(ctx, ref, EventRestorePasswordsFailed, map[string]string{"error": err.Error()})
	}
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		fail(err)
		return
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		fail(err)
		return
	}
	if err := pp.SetRolePasswords(ctx, p, keys); err != nil {
		fail(err)
		return
	}
	if len(e.opts.Fleet) > 0 {
		// The pooler may hold the verifier of the password the restore brought back.
		if err := e.opts.Fleet.RefreshTenant(ctx, ref); err != nil {
			e.log.Warn("restore: the pooler could not drop its cached logins", "ref", ref, "error", err)
		}
		e.refreshPeers(ctx, ref)
	}
}

// settleRestore ends RESTORING. A project that someone else moved on in the meantime (a
// delete that began after the restore did not hold the lock) keeps the status it has.
func (e *Engine) settleRestore(ctx context.Context, ref string, ok bool) error {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.Status != registry.StatusRestoring {
		return nil
	}
	want := registry.StatusActiveHealthy
	if !ok {
		want = registry.StatusRestoreFailed
	}
	return e.reg.SetProjectStatus(ctx, ref, want)
}
