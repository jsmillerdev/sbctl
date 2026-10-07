package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// RestoreRequest says what an in-place restore brings back.
type RestoreRequest struct {
	// Target is the point in time to restore to. BackupID overrides it.
	Target time.Time
	// BackupID restores exactly the state at the end of that base backup (the backup
	// service's manifest id).
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
	BeginRestore(ctx context.Context, ref string) (*Restore, error)
}

var _ DatabaseRestorer = (*Engine)(nil)

// ErrNoRestorer is returned by BeginRestore on a node whose backups are not configured.
var ErrNoRestorer = errors.New("lifecycle: this node has no backup service, so there is nothing to restore from")

// EventRestoreRequested records that a restore began through the Engine (the Management
// API). The backup service writes restore.completed or restore.failed when it ends.
const EventRestoreRequested = "restore.requested"

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

// Restore is one in-place restore in progress: the project is RESTORING until Run returns.
type Restore struct {
	e   *Engine
	ref string
}

// BeginRestore moves an active project to RESTORING and returns the handle that runs the
// restore. It refuses (ErrInvalidState) a project that is paused, starting, being deleted or
// already being restored, so two operations never overlap: every other operation checks the
// status under the same per-project lock. The caller must call Run; until then the project
// stays RESTORING.
func (e *Engine) BeginRestore(ctx context.Context, ref string) (*Restore, error) {
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
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !active(p.Status) {
		return nil, invalidState(p, "restore")
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusRestoring); err != nil {
		return nil, err
	}
	e.restoring.Store(ref, struct{}{})
	return &Restore{e: e, ref: ref}, nil
}

// Run restores the project in place and settles its status: ACTIVE_HEALTHY when the restore
// worked; after a failure ACTIVE_UNHEALTHY until a health check finds the project running on
// its original data again (the backup service puts the original back when it can). The
// outcome is the backup service's error, if any.
func (r *Restore) Run(ctx context.Context, req RestoreRequest) error {
	e := r.e
	defer e.restoring.Delete(r.ref)
	e.event(ctx, r.ref, EventRestoreRequested, map[string]any{"target": req.Target, "backup": req.BackupID})
	err := e.restorer().RestoreInPlace(withRestoring(ctx), r.ref, req)

	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if serr := e.settleRestore(cctx, r.ref, err == nil); serr != nil {
		e.log.Warn("restore: could not settle the project status", "ref", r.ref, "error", serr)
	}
	if err != nil {
		_, _ = e.Health(cctx, r.ref)
		return fmt.Errorf("lifecycle: restore %s: %w", r.ref, err)
	}
	return nil
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
		want = registry.StatusActiveUnhealthy
	}
	return e.reg.SetProjectStatus(ctx, ref, want)
}
