package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Options configure an Engine.
type Options struct {
	Log *slog.Logger
	// Fleet registers projects with the shared services after their units are healthy.
	// Empty (the default until the fleet workstream lands) skips tenant calls.
	Fleet fleet.Fleet
	// Backup takes the final base backup before a project is deleted; nil skips it.
	Backup BaseBackuper
	// Now is the clock for key issue times; tests set it.
	Now func() time.Time
}

// Engine is the Manager: it owns the order of registry rows, sealed secrets, units,
// fleet tenants and routes, and the project status each step implies.
type Engine struct {
	cfg   *config.Config
	reg   registry.Registry
	sec   secrets.Secrets
	arts  Artifacts
	plane Plane
	opts  Options
	log   *slog.Logger

	locks sync.Map // ref -> *sync.Mutex
}

var _ Manager = (*Engine)(nil)

// NewEngine builds the Manager over plane.
func NewEngine(cfg *config.Config, reg registry.Registry, sec secrets.Secrets, arts Artifacts, plane Plane, opts Options) *Engine {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{cfg: cfg, reg: reg, sec: sec, arts: arts, plane: plane, opts: opts, log: opts.Log}
}

// advisoryPool is implemented by the Postgres registry; the in-memory one has no
// other process to coordinate with.
type advisoryPool interface{ Pool() *pgxpool.Pool }

// lock serializes mutating operations on one project: a mutex within this process and,
// when the registry is Postgres, a session-level advisory lock so that the CLI and the
// daemon (separate processes) cannot, say, delete and resume the same ref at once. The
// advisory lock uses its own connection rather than the pool, so held locks cannot
// starve registry queries, and it is released when that connection closes, even if the
// process dies. The returned function releases both.
func (e *Engine) lock(ctx context.Context, ref string) (func(), error) {
	m, _ := e.locks.LoadOrStore(ref, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil {
		return mu.Unlock, nil
	}
	conn, err := pgx.ConnectConfig(ctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext('sbctl:' || $1::text))`, ref); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock %s: %w", ref, err)
	}
	return func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
		mu.Unlock()
	}, nil
}

// cleanupCtx outlives a cancelled request: cleanup after a failure must still run.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
}

func (e *Engine) versions() (map[string]string, error) {
	v := map[string]string{}
	for _, svc := range config.ProjectServices {
		tag, err := e.arts.Tag(svc)
		if err != nil {
			return nil, err
		}
		v[svc] = tag
	}
	return v, nil
}

// StoreKeys seals every credential of keys into the registry.
func (e *Engine) storeKeys(ctx context.Context, ref string, keys *secrets.ProjectKeys) error {
	for name, val := range keys.Map() {
		sealed, err := e.sec.Seal([]byte(val))
		if err != nil {
			return err
		}
		if err := e.reg.PutSecret(ctx, ref, name, sealed); err != nil {
			return fmt.Errorf("lifecycle: store secret %s: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) loadKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	sealed, err := e.reg.GetSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := e.sec.Open(blob)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: open secret %s of %s: %w", name, ref, err)
		}
		m[name] = string(pt)
	}
	k := secrets.KeysFromMap(m)
	if k.JWTSecret == "" || k.AdminPassword == "" || k.DBPassword == "" {
		return nil, fmt.Errorf("lifecycle: project %s has no stored credentials", ref)
	}
	return k, nil
}

func (e *Engine) event(ctx context.Context, ref, kind string, payload any) {
	if err := e.reg.AppendEvent(ctx, ref, kind, payload); err != nil {
		e.log.Warn("could not record event", "ref", ref, "kind", kind, "error", err)
	}
}

func (e *Engine) org(ctx context.Context, slug string) (*registry.Organization, error) {
	if slug == "" {
		slug = "default"
	}
	o, err := e.reg.GetOrganization(ctx, slug)
	if errors.Is(err, registry.ErrNotFound) && slug == "default" {
		o, err = e.reg.CreateOrganization(ctx, "default", "Default Organization")
		if errors.Is(err, registry.ErrConflict) {
			o, err = e.reg.GetOrganization(ctx, slug)
		}
	}
	return o, err
}

func (e *Engine) tenantSpec(p *registry.Project, keys *secrets.ProjectKeys) fleet.TenantSpec {
	ports := e.cfg.PortsFor(p.Ref, p.Seq)
	return fleet.TenantSpec{
		Ref: p.Ref, DBHost: "127.0.0.1", DBPort: ports.Postgres, DBName: "postgres",
		DBUser: RoleAdmin, DBPassword: keys.AdminPassword, PostgresPassword: keys.DBPassword,
		JWTSecret: keys.JWTSecret, AnonKey: keys.AnonKey, ServiceRoleKey: keys.ServiceRoleKey,
		Host: e.cfg.ProjectHost(p.Ref),
	}
}

// Create implements Manager.
//
// Order: registry row (COMING_UP), sealed secrets, data plane (cluster, roles, GoTrue,
// PostgREST, all health-checked), fleet tenants, host route, then ACTIVE_HEALTHY. A
// failure after the row exists stops and removes the units and the data and leaves the
// row INIT_FAILED with an event carrying the cause; the secrets stay so that the row
// can be inspected and deleted. Nothing is left running.
func (e *Engine) Create(ctx context.Context, req CreateRequest) (*registry.Project, error) {
	if e.cfg.BaseDomain() == "" {
		return nil, errors.New("lifecycle: set domain (or public_ip) in the config before creating projects")
	}
	class := req.Class
	if class == "" {
		class = DefaultClass
	}
	if class == ClassSystem {
		return nil, fmt.Errorf("lifecycle: class %q is reserved", class)
	}
	if _, err := ClassFor(class); err != nil {
		return nil, err
	}
	if req.Seed != nil && req.Keys == nil {
		// A seeded cluster already holds its role passwords and Create does not reset
		// them, so fresh keys would never match and the units could not authenticate.
		return nil, errors.New("lifecycle: CreateRequest.Seed needs CreateRequest.Keys (the seeded cluster's own credentials)")
	}
	ref := req.Ref
	switch {
	case ref == "":
		ref = secrets.NewRef()
	case !secrets.ValidRef(ref):
		return nil, fmt.Errorf("lifecycle: %q is not a valid project ref (20 lowercase letters)", ref)
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()

	org, err := e.org(ctx, req.OrgSlug)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: organization %q: %w", req.OrgSlug, err)
	}
	versions, err := e.versions()
	if err != nil {
		return nil, err
	}
	limits := e.cfg.Defaults
	if req.Limits != nil {
		limits = *req.Limits
	}
	var keys *secrets.ProjectKeys
	if req.Keys != nil {
		k := *req.Keys
		keys = &k
	} else if keys, err = secrets.NewProjectKeys(ref, e.opts.Now()); err != nil {
		return nil, err
	}
	if req.DBPassword != "" {
		keys.DBPassword = req.DBPassword
	}
	region := req.Region
	if region == "" {
		region = "local"
	}
	name := req.Name
	if name == "" {
		name = ref
	}
	p := &registry.Project{
		Ref: ref, OrgID: org.ID, Name: name, Region: region, Engine: registry.EnginePostgres,
		Class: class, Status: registry.StatusComingUp, Versions: versions, Limits: limits,
	}
	if err := e.reg.CreateProject(ctx, p); err != nil {
		return nil, fmt.Errorf("lifecycle: create project %s: %w", ref, err)
	}
	fail := func(stage string, cause error) (*registry.Project, error) {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if errors.Is(cause, ErrClusterExists) {
			// The data belongs to someone else (an orphan, or a restore target that was
			// not cleared): leave it alone and forget the row this call just made.
			e.cleanup(cctx, p, true)
			if err := e.reg.DeleteProject(cctx, ref); err != nil {
				e.log.Error("could not remove the registry row of a refused create", "ref", ref, "error", err)
			}
			return nil, fmt.Errorf("lifecycle: create %s refused, existing data left untouched (move %s away or choose another ref): %w", ref, e.cfg.Paths().Project(ref), cause)
		}
		e.cleanup(cctx, p, false)
		if err := e.reg.SetProjectStatus(cctx, ref, registry.StatusInitFailed); err != nil {
			e.log.Error("could not mark project INIT_FAILED", "ref", ref, "error", err)
		}
		e.event(cctx, ref, "project.init_failed", map[string]string{"stage": stage, "error": cause.Error()})
		return nil, fmt.Errorf("lifecycle: create %s failed at %s (project left INIT_FAILED, nothing running; remove it with `sbctl projects delete %s --skip-final-backup`): %w", ref, stage, ref, cause)
	}
	if err := e.storeKeys(ctx, ref, keys); err != nil {
		return fail("store secrets", err)
	}
	if err := e.plane.Create(ctx, p, keys, req.Seed); err != nil {
		return fail("data plane", err)
	}
	if len(e.opts.Fleet) > 0 {
		if err := e.opts.Fleet.EnsureTenant(ctx, e.tenantSpec(p, keys)); err != nil {
			return fail("fleet tenants", err)
		}
	}
	if err := e.reg.PutRoute(ctx, registry.Route{Host: e.cfg.ProjectHost(ref), Ref: ref, Kind: "api"}); err != nil {
		return fail("route", err)
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusActiveHealthy); err != nil {
		return fail("status", err)
	}
	e.event(ctx, ref, "project.created", map[string]string{"class": class})
	return e.reg.GetProject(ctx, ref)
}

// cleanup undoes what Create may have started, ignoring errors (it is already failing).
// keepData skips the data plane, whose Delete removes the project directory.
func (e *Engine) cleanup(ctx context.Context, p *registry.Project, keepData bool) {
	if len(e.opts.Fleet) > 0 {
		if err := e.opts.Fleet.RemoveTenant(ctx, p.Ref); err != nil {
			e.log.Warn("cleanup: remove tenants", "ref", p.Ref, "error", err)
		}
	}
	if err := e.reg.DeleteRoute(ctx, e.cfg.ProjectHost(p.Ref)); err != nil && !errors.Is(err, registry.ErrNotFound) {
		e.log.Warn("cleanup: delete route", "ref", p.Ref, "error", err)
	}
	if keepData {
		return
	}
	if err := e.plane.Delete(ctx, p.Ref); err != nil {
		e.log.Error("cleanup: data plane delete failed; units or data may remain", "ref", p.Ref, "error", err)
	}
}

func active(s registry.Status) bool {
	return s == registry.StatusActiveHealthy || s == registry.StatusActiveUnhealthy
}

func invalidState(p *registry.Project, op string) error {
	return fmt.Errorf("%w: cannot %s %s while it is %s", ErrInvalidState, op, p.Ref, p.Status)
}

// Pause implements Manager: GoTrue, PostgREST and PostgreSQL stop, in that order, and
// the project becomes INACTIVE. Its route and fleet tenants stay; the data stays.
func (e *Engine) Pause(ctx context.Context, ref string) error {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if ref == config.SystemRef || !active(p.Status) {
		return invalidState(p, "pause")
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusPausing); err != nil {
		return err
	}
	if err := e.plane.Stop(ctx, ref); err != nil {
		_ = e.reg.SetProjectStatus(context.WithoutCancel(ctx), ref, registry.StatusActiveUnhealthy)
		return fmt.Errorf("lifecycle: pause %s: %w", ref, err)
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusInactive); err != nil {
		return err
	}
	e.event(ctx, ref, "project.paused", nil)
	return nil
}

// Resume implements Manager: the reverse of Pause. If a unit does not come up, what
// started is stopped again and the project stays INACTIVE, so Resume can be retried.
func (e *Engine) Resume(ctx context.Context, ref string) error {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if ref == config.SystemRef || p.Status != registry.StatusInactive {
		return invalidState(p, "resume")
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return err
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusComingUp); err != nil {
		return err
	}
	if err := e.plane.Start(ctx, p, keys); err != nil {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if serr := e.plane.Stop(cctx, ref); serr != nil {
			e.log.Warn("resume: stop after failed start", "ref", ref, "error", serr)
		}
		_ = e.reg.SetProjectStatus(cctx, ref, registry.StatusInactive)
		e.event(cctx, ref, "project.resume_failed", map[string]string{"error": err.Error()})
		return fmt.Errorf("lifecycle: resume %s: %w", ref, err)
	}
	if err := e.reg.PutRoute(ctx, registry.Route{Host: e.cfg.ProjectHost(ref), Ref: ref, Kind: "api"}); err != nil {
		e.log.Warn("resume: route", "ref", ref, "error", err)
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusActiveHealthy); err != nil {
		return err
	}
	e.event(ctx, ref, "project.resumed", nil)
	return nil
}

// Delete implements Manager with a final base backup when a backup engine is set.
func (e *Engine) Delete(ctx context.Context, ref string) error {
	return e.DeleteWith(ctx, ref, DeleteOptions{})
}

// DeleteWith deletes ref. Order: final base backup (the database is started for it if the
// project is paused), fleet tenants, route, units and data, registry row. If the backup
// fails the project is left as it was and the error is returned; any later failure
// leaves the project GOING_DOWN so that Delete can be run again. A project that failed
// to initialize has nothing worth backing up and skips the backup.
func (e *Engine) DeleteWith(ctx context.Context, ref string, o DeleteOptions) error {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if ref == config.SystemRef {
		return fmt.Errorf("%w: the system project cannot be deleted", ErrInvalidState)
	}
	prev := p.Status
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusGoingDown); err != nil {
		return err
	}
	if e.opts.Backup != nil && !o.SkipFinalBackup && prev != registry.StatusInitFailed {
		if err := e.finalBackup(ctx, p, prev); err != nil {
			_ = e.reg.SetProjectStatus(context.WithoutCancel(ctx), ref, prev)
			return fmt.Errorf("lifecycle: final backup of %s failed, project kept: %w", ref, err)
		}
	}
	if len(e.opts.Fleet) > 0 {
		if err := e.opts.Fleet.RemoveTenant(ctx, ref); err != nil {
			e.log.Warn("delete: remove fleet tenants (continuing)", "ref", ref, "error", err)
			e.event(ctx, ref, "project.tenant_cleanup_failed", map[string]string{"error": err.Error()})
		}
	}
	if err := e.reg.DeleteRoute(ctx, e.cfg.ProjectHost(ref)); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if err := e.plane.Delete(ctx, ref); err != nil {
		return fmt.Errorf("lifecycle: delete %s: %w", ref, err)
	}
	if err := e.reg.DeleteProject(ctx, ref); err != nil {
		return err
	}
	e.event(ctx, ref, "project.deleted", map[string]any{"final_backup": e.opts.Backup != nil && !o.SkipFinalBackup && prev != registry.StatusInitFailed})
	return nil
}

func (e *Engine) finalBackup(ctx context.Context, p *registry.Project, prev registry.Status) error {
	keys, err := e.loadKeys(ctx, p.Ref)
	if err != nil {
		return err
	}
	if prev == registry.StatusInactive {
		// Paused: the cluster is down. Start it alone for the backup, then stop it again.
		if err := e.plane.StartDatabase(ctx, p, keys); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := cleanupCtx(ctx)
			defer cancel()
			if err := e.plane.Stop(cctx, p.Ref); err != nil {
				e.log.Warn("delete: stop database after backup", "ref", p.Ref, "error", err)
			}
		}()
	}
	_, err = e.opts.Backup.BaseBackup(ctx, p.Ref)
	return err
}

// RotateKeys implements Manager: a new JWT secret, legacy anon and service_role keys
// and opaque keys; database passwords are unchanged. GoTrue and PostgREST are
// restarted on the new secret and the fleet tenants are updated. A paused project only
// gets new stored keys; its env files are rewritten when it resumes. If applying the
// keys fails, the previous keys are restored.
func (e *Engine) RotateKeys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.Status != registry.StatusInactive && !active(p.Status) {
		return nil, invalidState(p, "rotate keys of")
	}
	old, err := e.loadKeys(ctx, ref)
	if err != nil {
		return nil, err
	}
	nk := *old
	nk.JWTSecret = secrets.NewJWTSecret()
	nk.PublishableKey = secrets.NewPublishableKey()
	nk.SecretKey = secrets.NewSecretKey()
	if err := nk.ResignLegacy(ref, e.opts.Now()); err != nil {
		return nil, err
	}
	rollback := func(cause error) (*secrets.ProjectKeys, error) {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if err := e.storeKeys(cctx, ref, old); err != nil {
			e.log.Error("rotate-keys: could not restore previous keys", "ref", ref, "error", err)
		} else if active(p.Status) {
			if err := e.plane.Reconfigure(cctx, p, old); err != nil {
				e.log.Error("rotate-keys: could not restart on previous keys", "ref", ref, "error", err)
			}
		}
		return nil, fmt.Errorf("lifecycle: rotate keys of %s (previous keys restored): %w", ref, cause)
	}
	if err := e.storeKeys(ctx, ref, &nk); err != nil {
		return rollback(err)
	}
	if active(p.Status) {
		if err := e.plane.Reconfigure(ctx, p, &nk); err != nil {
			return rollback(err)
		}
		if len(e.opts.Fleet) > 0 && ref != config.SystemRef {
			if err := e.opts.Fleet.EnsureTenant(ctx, e.tenantSpec(p, &nk)); err != nil {
				return rollback(fmt.Errorf("update fleet tenants: %w", err))
			}
		}
	}
	e.event(ctx, ref, "project.keys_rotated", nil)
	return &nk, nil
}

// StartActive starts every project the registry lists as ACTIVE (after a reboot, or
// after `sbctl system stop`), one at a time, and returns the errors by ref. A project
// that fails to start is marked ACTIVE_UNHEALTHY; paused projects are left alone.
func (e *Engine) StartActive(ctx context.Context) map[string]error {
	errs := map[string]error{}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		errs[""] = err
		return errs
	}
	for i := range ps {
		p := ps[i]
		if p.Ref == config.SystemRef || !active(p.Status) {
			continue
		}
		if err := e.startOne(ctx, &p); err != nil {
			errs[p.Ref] = err
		}
	}
	return errs
}

func (e *Engine) startOne(ctx context.Context, p *registry.Project) error {
	unlock, err := e.lock(ctx, p.Ref)
	if err != nil {
		return err
	}
	defer unlock()
	keys, err := e.loadKeys(ctx, p.Ref)
	if err == nil {
		err = e.plane.Start(ctx, p, keys)
	}
	want := registry.StatusActiveHealthy
	if err != nil {
		want = registry.StatusActiveUnhealthy
	}
	if want != p.Status {
		_ = e.reg.SetProjectStatus(ctx, p.Ref, want)
	}
	return err
}

// Keys implements Manager.
func (e *Engine) Keys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	if _, err := e.reg.GetProject(ctx, ref); err != nil {
		return nil, err
	}
	return e.loadKeys(ctx, ref)
}

// Health implements Manager. For an ACTIVE project it also moves the registry status
// between ACTIVE_HEALTHY and ACTIVE_UNHEALTHY to match what the checks found.
func (e *Engine) Health(ctx context.Context, ref string) ([]ServiceHealth, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	hs := e.plane.Health(ctx, p, nil)
	if active(p.Status) {
		want := registry.StatusActiveHealthy
		for _, h := range hs {
			if !h.Healthy {
				want = registry.StatusActiveUnhealthy
			}
		}
		if want != p.Status {
			e.settleStatus(ctx, ref, p.Status, want)
		}
	}
	return hs, nil
}

// settleStatus moves ref from the status a health check started under to want, but only
// if the project still has that status once it holds the per-ref lock. The checks run
// without the lock and can take seconds; a Pause, Resume or Delete that finished in the
// meantime owns the status, and a stale ACTIVE_UNHEALTHY must not overwrite INACTIVE.
func (e *Engine) settleStatus(ctx context.Context, ref string, from, want registry.Status) {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		e.log.Warn("health: lock for status update", "ref", ref, "error", err)
		return
	}
	defer unlock()
	cur, err := e.reg.GetProject(ctx, ref)
	if err != nil || cur.Status != from {
		return
	}
	if err := e.reg.SetProjectStatus(ctx, ref, want); err != nil {
		e.log.Warn("health: update status", "ref", ref, "error", err)
	}
}

// ConnString implements Manager.
func (e *Engine) ConnString(ctx context.Context, ref, role string) (string, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return "", err
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return "", err
	}
	var pw string
	switch role {
	case RolePostgres:
		pw = keys.DBPassword
	case RoleAdmin:
		pw = keys.AdminPassword
	default:
		return "", fmt.Errorf("lifecycle: ConnString supports roles %q and %q, not %q", RolePostgres, RoleAdmin, role)
	}
	return dsnURL(role, pw, e.cfg.PortsFor(ref, p.Seq).Postgres, "postgres"), nil
}

// FormatHealth renders health results as one line per service.
func FormatHealth(hs []ServiceHealth) string {
	var b strings.Builder
	for _, h := range hs {
		fmt.Fprintf(&b, "%-10s %-15s", h.Name, h.Status)
		if h.Error != "" {
			b.WriteString(" " + h.Error)
		}
		b.WriteString("\n")
	}
	return b.String()
}
