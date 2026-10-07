package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Resizer is the optional Manager capability that changes a project's compute size. *Engine
// implements it; the Management API's add-on routes and `supavise projects resize` use it.
type Resizer interface {
	// BeginResize validates a change to size (a name ParseSize takes) under the project's lock,
	// records the new size and moves the project to RESIZING, and returns the handle whose Run
	// carries it out. It refuses with ErrInvalidState while another operation runs on the
	// project or the project is not running or paused, and with *CapacityError when the node
	// cannot honor the size. Nothing has changed when it returns an error.
	BeginResize(ctx context.Context, ref, size string) (*ResizeRun, error)
	// Offers lists every size with whether the node can give it to ref (ref "": a new project).
	Offers(ctx context.Context, ref string) ([]Offer, error)
}

var _ Resizer = (*Engine)(nil)

// Event kinds of a resize.
const (
	EventResizeStarted = "project.resize_started"
	EventResized       = "project.resized"
	EventResizeFailed  = "project.resize_failed"
)

// ResizeRun is a resize that has begun. The project is RESIZING and its lock is held until Run
// or Close returns.
type ResizeRun struct {
	e        *Engine
	p        *registry.Project // the project as it was
	from, to Class
	prev     registry.Status
	unlock   func()
	noop     bool // nothing to restart: the size is unchanged, or the project is paused
	once     sync.Once
	// From and To are the names of the sizes; Changed is false when the project already had To.
	From, To string
	Changed  bool
}

// Close releases the project without running the resize. It undoes the record BeginResize wrote
// when Run has not been called; after Run it does nothing.
func (r *ResizeRun) Close() {
	r.once.Do(func() {
		if !r.noop {
			ctx, cancel := cleanupCtx(context.Background())
			defer cancel()
			r.e.restoreRecord(ctx, r.p)
		}
		r.unlock()
	})
}

// BeginResize implements Resizer.
func (e *Engine) BeginResize(ctx context.Context, ref, size string) (*ResizeRun, error) {
	if ref == config.SystemRef {
		return nil, fmt.Errorf("%w: the system project has no compute size", ErrInvalidState)
	}
	if size == "" || size == ClassSystem {
		return nil, fmt.Errorf("lifecycle: choose a size (%v)", ClassNames())
	}
	to, err := ClassFor(size)
	if err != nil {
		return nil, err
	}
	unlock, err := e.tryLock(ctx, ref)
	if err != nil {
		return nil, err
	}
	run, err := e.beginResize(ctx, ref, to, unlock)
	if err != nil {
		unlock()
		return nil, err
	}
	return run, nil
}

func (e *Engine) beginResize(ctx context.Context, ref string, to Class, unlock func()) (*ResizeRun, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !(active(p.Status) || p.Status == registry.StatusInactive) {
		return nil, invalidState(p, "resize")
	}
	from, ferr := ClassFor(p.Class)
	if ferr != nil {
		// A class an earlier version wrote that no migration renamed: judge the change from
		// the memory the project holds.
		from = Class{Name: p.Class, MemoryBytes: projectMemory(p)}
	}
	run := &ResizeRun{e: e, p: p, from: from, to: to, prev: p.Status, unlock: unlock, From: from.Name, To: to.Name}
	if ferr == nil && from.Name == to.Name && p.Limits == to.Limits() {
		run.noop = true
		return run, nil
	}
	run.Changed = true

	// The capacity check and the registry write that makes it true are one step, so two
	// resizes or creates cannot both take the last room.
	e.capacity.mu.Lock()
	defer e.capacity.mu.Unlock()
	cp, known, err := e.Capacity(ctx, ref)
	if err != nil {
		return nil, err
	}
	if known {
		if err := cp.FitsChange(from, to); err != nil {
			return nil, err
		}
	}
	np := *p
	np.Class, np.Limits = to.Name, to.Limits()
	if p.Status == registry.StatusInactive {
		// Nothing runs: the record is all there is to change, and Resume renders the units from it.
		if err := e.reg.UpdateProject(ctx, &np); err != nil {
			return nil, fmt.Errorf("lifecycle: resize %s: %w", ref, err)
		}
		e.event(ctx, ref, EventResized, map[string]any{"from": from.Name, "to": to.Name, "paused": true})
		run.noop = true
		return run, nil
	}
	np.Status = registry.StatusResizing
	if err := e.reg.UpdateProject(ctx, &np); err != nil {
		return nil, fmt.Errorf("lifecycle: resize %s: %w", ref, err)
	}
	e.event(ctx, ref, EventResizeStarted, map[string]any{"from": from.Name, "to": to.Name, "status": string(p.Status)})
	return run, nil
}

// Run restarts the project on the new size: the shared services are asked to let go of its
// database, PostgreSQL, GoTrue and PostgREST stop and start again on units rendered from the new
// size (its MemoryMax and CPUQuota, its server arguments), each answers a real request, and the
// project's Supavisor tenant is updated with the size's pool. Only this project restarts.
//
// A failure at any step puts the previous size back: the record, the units and the tenant. The
// project then runs as before, and Run returns the cause. If the old size does not come back
// either, the project is left ACTIVE_UNHEALTHY and the error says so.
func (r *ResizeRun) Run(ctx context.Context) error {
	defer r.Close()
	if r.noop {
		return nil
	}
	e, p := r.e, r.p
	np := *p
	np.Class, np.Limits = r.to.Name, r.to.Limits()
	np.Status = registry.StatusResizing
	keys, err := e.loadKeys(ctx, p.Ref)
	if err != nil {
		return r.fail(ctx, fmt.Errorf("load credentials: %w", err), false)
	}
	e.quiesce(ctx, p.Ref)
	if err := e.plane.Stop(ctx, p.Ref); err != nil {
		return r.fail(ctx, fmt.Errorf("stop: %w", err), true)
	}
	if err := e.plane.Start(ctx, &np, keys); err != nil {
		return r.fail(ctx, fmt.Errorf("start on %s: %w", r.to.Title, err), true)
	}
	if len(e.opts.Fleet) > 0 {
		spec, err := e.tenantSpec(ctx, &np, keys)
		if err == nil {
			err = e.opts.Fleet.EnsureTenant(ctx, spec)
		}
		if err != nil {
			return r.fail(ctx, fmt.Errorf("update the pooler tenant: %w", err), true)
		}
	}
	if err := e.reg.SetProjectStatus(ctx, p.Ref, registry.StatusActiveHealthy); err != nil {
		return r.fail(ctx, fmt.Errorf("record the status: %w", err), true)
	}
	e.startTimer(ctx, p.Ref)
	e.event(ctx, p.Ref, EventResized, map[string]any{"from": r.from.Name, "to": r.to.Name})
	r.noop = true // settled: Close must not undo it
	return nil
}

// fail puts the previous size back and returns the cause. restart says whether the units were
// touched (and so must be started again on the old size).
func (r *ResizeRun) fail(ctx context.Context, cause error, restart bool) error {
	e, p := r.e, r.p
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	r.noop = true // from here Close only releases the lock
	rolled := true
	var rerr error
	e.restoreRecord(cctx, p)
	if restart {
		keys, err := e.loadKeys(cctx, p.Ref)
		if err == nil {
			e.quiesce(cctx, p.Ref)
			if serr := e.plane.Stop(cctx, p.Ref); serr != nil {
				e.log.Warn("resize rollback: stop", "ref", p.Ref, "error", serr)
			}
			old := *p
			err = e.plane.Start(cctx, &old, keys)
			if err == nil && len(e.opts.Fleet) > 0 {
				spec, serr := e.tenantSpec(cctx, &old, keys)
				if serr == nil {
					serr = e.opts.Fleet.EnsureTenant(cctx, spec)
				}
				if serr != nil {
					e.log.Warn("resize rollback: pooler tenant", "ref", p.Ref, "error", serr)
				}
			}
		}
		if err != nil {
			rolled, rerr = false, err
		}
	}
	status := r.prev
	if !active(status) {
		status = registry.StatusActiveHealthy
	}
	if !rolled {
		status = registry.StatusActiveUnhealthy
	}
	if err := e.reg.SetProjectStatus(cctx, p.Ref, status); err != nil {
		e.log.Error("resize: could not record the status after a failure", "ref", p.Ref, "error", err)
	}
	payload := map[string]any{"from": r.from.Name, "to": r.to.Name, "error": cause.Error(), "rolled_back": rolled}
	if rerr != nil {
		payload["rollback_error"] = rerr.Error()
	}
	e.event(cctx, p.Ref, EventResizeFailed, payload)
	if !rolled {
		return fmt.Errorf("lifecycle: resize %s to %s failed (%w) and %s did not come back (%v); the project is ACTIVE_UNHEALTHY, see `supavise projects health %s`",
			p.Ref, r.to.Title, cause, r.from.Title, rerr, p.Ref)
	}
	return fmt.Errorf("lifecycle: resize %s to %s failed and the project is back on %s: %w", p.Ref, r.to.Title, r.from.Title, cause)
}

// restoreRecord writes back the size, limits and status a project had before BeginResize changed
// the record, unless the record is no longer RESIZING (something else moved it on).
func (e *Engine) restoreRecord(ctx context.Context, old *registry.Project) {
	cur, err := e.reg.GetProject(ctx, old.Ref)
	if err != nil {
		e.log.Warn("resize: could not read the project to restore its size", "ref", old.Ref, "error", err)
		return
	}
	if cur.Status != registry.StatusResizing {
		return
	}
	cur.Class, cur.Limits, cur.Status = old.Class, old.Limits, old.Status
	if !active(cur.Status) {
		cur.Status = registry.StatusActiveHealthy
	}
	if err := e.reg.UpdateProject(ctx, cur); err != nil {
		e.log.Error("resize: could not restore the project's size in the registry", "ref", old.Ref, "error", err)
	}
}

// Resize changes the size of ref and waits for the project to be healthy on it (BeginResize
// and Run in one call). It returns the project as it is afterwards.
func (e *Engine) Resize(ctx context.Context, ref, size string) (*registry.Project, error) {
	run, err := e.BeginResize(ctx, ref, size)
	if err != nil {
		return nil, err
	}
	if err := run.Run(ctx); err != nil {
		return nil, err
	}
	return e.reg.GetProject(ctx, ref)
}

// errBusy is what a resize gets while another operation holds the project.
func errBusy(ref string) error {
	return fmt.Errorf("%w: another operation is running on %s; try again when it has finished", ErrInvalidState, ref)
}

// tryLock is lock that gives up at once instead of waiting: a resize is refused while another
// operation (a restore, a pause, an earlier resize, a settings apply) runs on the project, in
// this process or, with the Postgres registry, in another.
func (e *Engine) tryLock(ctx context.Context, ref string) (func(), error) {
	m, _ := e.locks.LoadOrStore(ref, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, errBusy(ref)
	}
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil {
		return mu.Unlock, nil
	}
	conn, err := pgx.ConnectConfig(ctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtext('supavise:' || $1::text))`, ref).Scan(&got); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	if !got {
		_ = conn.Close(context.WithoutCancel(ctx))
		mu.Unlock()
		return nil, errBusy(ref)
	}
	return func() {
		cctx, cancel := cleanupCtx(context.Background())
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
		mu.Unlock()
	}, nil
}

// IsCapacity reports whether err is the node refusing a size, and returns the refusal.
func IsCapacity(err error) (*CapacityError, bool) {
	var ce *CapacityError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// poolDefaults fills the pool size and client limit a project's Supavisor tenant runs with
// when none is saved: the size's (hosted's pooler client limit per size, a pool of 40% of
// max_connections), with the client limit held under the node's per-project ceiling.
func (e *Engine) poolDefaults(spec *fleet.TenantSpec, p *registry.Project) {
	if p.Ref == config.SystemRef {
		return
	}
	cl, err := ClassFor(p.Class)
	if err != nil {
		return
	}
	if spec.PoolSize == 0 {
		spec.PoolSize = cl.PoolSize
	}
	if spec.MaxClients == 0 {
		spec.MaxClients = min(cl.PoolerMaxClients, e.cfg.Fleet.PoolerMaxClients())
	}
}
