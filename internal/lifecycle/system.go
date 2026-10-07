package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/artifacts"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// SystemDatabases are created in the system cluster next to "postgres" (GoTrue's
// dashboard auth schema) and "sbctl" (the registry, used by supabase_admin). Each of the
// others belongs to one fleet service: see FleetRoles.
var SystemDatabases = []string{"sbctl", "_supavisor", "_realtime", "_storage"}

// FleetRole is the login role one fleet service uses for its own database in the
// system cluster. It owns that database and the same-named schema and nothing else, and
// PUBLIC may not connect to the database, so a compromised fleet service cannot read
// the registry or another service's data the way supabase_admin could.
type FleetRole struct {
	Service  string // "supavisor", "realtime", "storage"
	Database string
	Role     string
}

// FleetRoles lists the fleet services' roles. Their passwords are sealed in the
// registry under the system project (see Engine.FleetCredentials).
var FleetRoles = []FleetRole{
	{Service: "supavisor", Database: "_supavisor", Role: "sbctl_supavisor"},
	{Service: "realtime", Database: "_realtime", Role: "sbctl_realtime"},
	{Service: "storage", Database: "_storage", Role: "sbctl_storage"},
}

func fleetSecretName(service string) string { return "fleet_" + service + "_password" }

// FleetCredential is a fleet service's database login.
type FleetCredential struct {
	FleetRole
	Password string
}

// FleetCredentials returns the decrypted logins of the fleet services' roles, which
// InitSystem creates. Workstream E connects with these instead of supabase_admin.
func (e *Engine) FleetCredentials(ctx context.Context) ([]FleetCredential, error) {
	out := make([]FleetCredential, 0, len(FleetRoles))
	for _, fr := range FleetRoles {
		sealed, err := e.reg.GetSecret(ctx, config.SystemRef, fleetSecretName(fr.Service))
		if err != nil {
			return nil, fmt.Errorf("lifecycle: credentials of %s: %w (run `sbctl system init`)", fr.Role, err)
		}
		pt, err := e.sec.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: open credentials of %s: %w", fr.Role, err)
		}
		out = append(out, FleetCredential{FleetRole: fr, Password: string(pt)})
	}
	return out, nil
}

// ensureFleetCredentials gives each fleet role a password and seals it in the registry,
// unless it already has one. It runs after the system project row exists.
func (e *Engine) ensureFleetCredentials(ctx context.Context, pp pgPaths) error {
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.WithoutCancel(ctx))
	if err := quietSession(ctx, c); err != nil {
		return err
	}
	for _, fr := range FleetRoles {
		if _, err := e.reg.GetSecret(ctx, config.SystemRef, fleetSecretName(fr.Service)); err == nil {
			continue
		} else if !errors.Is(err, registry.ErrNotFound) {
			return err
		}
		pw := secrets.NewPassword()
		if err := setPassword(ctx, c, fr.Role, pw); err != nil {
			return err
		}
		sealed, err := e.sec.Seal([]byte(pw))
		if err != nil {
			return err
		}
		if err := e.reg.PutSecret(ctx, config.SystemRef, fleetSecretName(fr.Service), sealed); err != nil {
			return err
		}
	}
	return nil
}

// OpenOptions configure Open and InitSystem.
type OpenOptions struct {
	Log *slog.Logger
	// ConfigPath, ArchiveCommand: see PlaneOptions.
	ConfigPath        string
	ArchiveCommand    string
	ArchiveCommandFor func(ref string) string
	ArchiveTimeout    int
	Fleet             fleet.Fleet
	Backup            BaseBackuper
	// BackupFactory builds the BaseBackuper once the registry and the secrets are open
	// (the backup service needs both, and the Engine needs the backup service). When the
	// result also has a SetManager(Manager) method, Open hands it the Engine. Ignored
	// when Backup is set.
	BackupFactory func(n *Node) (BaseBackuper, error)
	// Timers replaces the backup timer control (tests); nil means the systemd backend's.
	Timers Timers
	// Supervisor replaces the backend chosen by cfg.Supervisor (tests).
	Supervisor units.Supervisor
	// Artifacts replaces the artifact store (tests).
	Artifacts Artifacts
	// Store options for the default artifact store.
	StoreOptions []artifacts.Option
}

func (o *OpenOptions) log() *slog.Logger {
	if o.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o.Log
}

func (o *OpenOptions) planeOptions() PlaneOptions {
	return PlaneOptions{Log: o.log(), ConfigPath: o.ConfigPath, ArchiveCommand: o.ArchiveCommand,
		ArchiveCommandFor: o.ArchiveCommandFor, ArchiveTimeout: o.ArchiveTimeout, Backup: o.Backup}
}

// Node is everything a process needs to manage projects on this machine: the secrets
// key, the supervisor, the artifact store, the registry (in the system cluster), the
// Postgres data plane and the Manager.
type Node struct {
	Cfg        *config.Config
	Secrets    secrets.Secrets
	Supervisor units.Supervisor
	Artifacts  Artifacts
	Registry   registry.Registry
	Plane      *PostgresPlane
	Engine     *Engine
}

// Close releases the registry pool and the supervisor connection.
func (n *Node) Close() {
	if n.Registry != nil {
		n.Registry.Close()
	}
	if c, ok := n.Supervisor.(interface{ Close() }); ok {
		c.Close()
	}
}

func (o *OpenOptions) supervisor(cfg *config.Config) (units.Supervisor, error) {
	if o.Supervisor != nil {
		return o.Supervisor, nil
	}
	return units.New(cfg, o.log())
}

func (o *OpenOptions) artifactStore(cfg *config.Config) (Artifacts, error) {
	if o.Artifacts != nil {
		return o.Artifacts, nil
	}
	opts := append([]artifacts.Option{artifacts.WithLogger(o.log())}, o.StoreOptions...)
	return artifacts.New(cfg, opts...)
}

// Open connects to an initialized node: it loads the master key, reaches the registry
// through the system cluster's private unix socket, and builds the Engine. It does not
// start anything; run `sbctl system init` (or start sb-postgres@system) first.
func Open(ctx context.Context, cfg *config.Config, o OpenOptions) (*Node, error) {
	kb, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: master key: %w (run `sbctl system init`)", err)
	}
	sec, err := secrets.Load(kb)
	if err != nil {
		return nil, err
	}
	arts, err := o.artifactStore(cfg)
	if err != nil {
		return nil, err
	}
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, err
	}
	reg, err := registry.Open(ctx, RegistryDSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("lifecycle: cannot reach the registry in the system cluster (is sb-postgres@system running? run `sbctl system init`): %w", err)
	}
	node := &Node{Cfg: cfg, Secrets: sec, Supervisor: sup, Artifacts: arts, Registry: reg}
	bk := o.lateBackup(node)
	po := o.planeOptions()
	po.Backup = bk
	node.Plane = NewPostgresPlane(cfg, sup, arts, reg, po)
	node.Engine = NewEngine(cfg, reg, sec, arts, node.Plane, Options{Log: o.log(), Fleet: o.Fleet, Backup: bk, Timers: o.timers(cfg, sup)})
	return node, nil
}

// supervisorTimers starts and stops sb-basebackup@<ref>.timer through the supervisor
// (D-Bus StartUnit, which the polkit rule allows for sb-* units). The timers are not
// enabled for boot: the daemon starts them again for every active project at start.
type supervisorTimers struct{ sup units.Supervisor }

func (t supervisorTimers) StartTimer(ctx context.Context, ref string) error {
	return t.sup.Start(ctx, "sb-basebackup@"+ref+".timer")
}
func (t supervisorTimers) StopTimer(ctx context.Context, ref string) error {
	return t.sup.Stop(ctx, "sb-basebackup@"+ref+".timer")
}

// timers returns the backup timer control for the systemd backend; the exec backend has
// no timers (backups are taken with `sbctl backups create`).
func (o *OpenOptions) timers(cfg *config.Config, sup units.Supervisor) Timers {
	if o.Timers != nil {
		return o.Timers
	}
	if cfg.Supervisor != config.SupervisorSystemd {
		return nil
	}
	return supervisorTimers{sup: sup}
}

// lateBackup returns o.Backup, or a BaseBackuper that builds itself from BackupFactory on
// first use: the backup service needs the opened registry and secrets (n), and its
// restore needs the Engine, and neither exists yet while the plane and Engine are built.
func (o *OpenOptions) lateBackup(n *Node) BaseBackuper {
	if o.Backup != nil {
		return o.Backup
	}
	if o.BackupFactory == nil {
		return nil
	}
	return &lateBackuper{node: n, factory: o.BackupFactory}
}

type lateBackuper struct {
	node    *Node
	factory func(n *Node) (BaseBackuper, error)
	mu      sync.Mutex
	b       BaseBackuper
}

func (l *lateBackuper) get() (BaseBackuper, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b != nil {
		return l.b, nil
	}
	b, err := l.factory(l.node)
	if err != nil {
		return nil, err
	}
	if m, ok := b.(interface{ SetManager(Manager) }); ok && l.node.Engine != nil {
		m.SetManager(l.node.Engine)
	}
	l.b = b
	return b, nil
}

// FinalBackup implements FinalBackuper: the delete-time backup, when the service has one.
func (l *lateBackuper) FinalBackup(ctx context.Context, ref string) (*registry.Backup, error) {
	b, err := l.get()
	if err != nil {
		return nil, fmt.Errorf("lifecycle: backup service: %w", err)
	}
	if fb, ok := b.(FinalBackuper); ok {
		return fb.FinalBackup(ctx, ref)
	}
	return b.BaseBackup(ctx, ref)
}

// BaseBackup implements BaseBackuper.
func (l *lateBackuper) BaseBackup(ctx context.Context, ref string) (*registry.Backup, error) {
	b, err := l.get()
	if err != nil {
		return nil, fmt.Errorf("lifecycle: backup service: %w", err)
	}
	return b.BaseBackup(ctx, ref)
}

// systemProject is the registry view of the system cluster.
func systemProject(cfg *config.Config, versions map[string]string) *registry.Project {
	return &registry.Project{
		Ref: config.SystemRef, Seq: 0, Name: "system", Region: cfg.ProjectRegion(""), Engine: registry.EnginePostgres,
		Class: ClassSystem, Status: registry.StatusComingUp, Versions: versions, Limits: cfg.Defaults,
	}
}

// InitSystem creates or repairs the system project, which is a project like any other
// with ref "system" on the configured system ports and without PostgREST:
//
//  1. master key, artifacts (postgres, auth, postgrest)
//  2. cluster initialized and started, role passwords set
//  3. databases sbctl, _supavisor, _realtime, _storage; registry migrations
//  4. the system project row and its sealed credentials
//  5. GoTrue configured for Studio sign-in (site URL https://studio.<domain>)
//
// It is idempotent: on an existing cluster it starts what is stopped and re-applies the
// configuration. If a first-time init fails before the credentials are in the
// registry, everything it created is removed so that init can simply be run again.
// fetch=false skips downloads (the artifacts must already be in place).
func InitSystem(ctx context.Context, cfg *config.Config, o OpenOptions, fetch bool) (_ *Node, err error) {
	log := o.log()
	if cfg.BaseDomain() == "" {
		return nil, errors.New("lifecycle: set domain (or public_ip) in the config before system init")
	}
	sec, err := secrets.LoadOrCreate(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: master key %s: %w", cfg.KeyPath, err)
	}
	arts, err := o.artifactStore(cfg)
	if err != nil {
		return nil, err
	}
	if fetch {
		f, ok := arts.(interface {
			Fetch(context.Context, string) (string, error)
		})
		if !ok {
			return nil, errors.New("lifecycle: artifact store cannot fetch")
		}
		for _, svc := range []string{config.SvcPostgres, config.SvcGoTrue, config.SvcPostgREST} {
			log.Info("fetching artifact", "service", svc)
			if _, err := f.Fetch(ctx, svc); err != nil {
				return nil, err
			}
		}
	}
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, err
	}
	mem := registry.NewMemory() // only for Route/Usage lookups, which init does not use
	plane := NewPostgresPlane(cfg, sup, arts, mem, o.planeOptions())
	versions := map[string]string{}
	for _, svc := range config.ProjectServices {
		if versions[svc], err = arts.Tag(svc); err != nil {
			return nil, err
		}
	}
	p := systemProject(cfg, versions)
	pp := plane.paths(p)
	_, statErr := os.Stat(filepath.Join(pp.Data, "PG_VERSION"))
	existing := statErr == nil
	_, pendErr := os.Stat(filepath.Join(pp.Data, initPendingWitness))
	if existing || pendErr == nil {
		// A crash during an earlier init leaves a cluster nobody can use. Detect that and
		// start over instead of failing on every rerun.
		stale, why, serr := plane.staleSystem(ctx, p, pendErr == nil)
		if serr != nil {
			return nil, serr
		}
		if stale {
			log.Warn("system cluster was never fully initialized; removing it and starting over", "reason", why)
			if err = plane.Delete(ctx, config.SystemRef); err != nil {
				return nil, fmt.Errorf("lifecycle: remove the unfinished system cluster: %w", err)
			}
			existing = false
		}
	}

	var keys *secrets.ProjectKeys
	var reg *registry.Postgres
	node := &Node{Cfg: cfg, Secrets: sec, Supervisor: sup, Artifacts: arts, Plane: plane}
	defer func() {
		if err != nil {
			node.Close()
		}
	}()

	if !existing {
		if keys, err = secrets.NewProjectKeys(config.SystemRef, o.now()); err != nil {
			return nil, err
		}
		defer func() {
			// Until the credentials are in the registry nobody can use this cluster.
			if err != nil && !keysStored(node, p) {
				cctx, cancel := cleanupCtx(ctx)
				defer cancel()
				if derr := plane.Delete(cctx, config.SystemRef); derr != nil {
					log.Error("system init cleanup failed", "error", derr)
				}
			}
		}()
		log.Info("initializing system cluster")
		if err = plane.createDatabase(ctx, p, keys, nil); err != nil {
			return nil, err
		}
	} else {
		log.Info("system cluster exists; starting it")
		if err = plane.startRendered(ctx, p); err != nil {
			return nil, err
		}
	}

	if err = createSystemDatabases(ctx, plane.paths(p)); err != nil {
		return nil, err
	}
	reg, err = registry.Open(ctx, RegistryDSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open registry: %w", err)
	}
	node.Registry = reg
	plane.reg = reg
	bk := o.lateBackup(node)
	plane.opts.Backup = bk
	eng := NewEngine(cfg, reg, sec, arts, plane, Options{Log: log, Fleet: o.Fleet, Backup: bk, Timers: o.timers(cfg, sup)})
	node.Engine = eng

	if existing {
		if keys, err = eng.loadKeys(ctx, config.SystemRef); err != nil {
			return nil, err
		}
		if err = plane.StartDatabase(ctx, p, keys); err != nil { // re-render with current settings
			return nil, err
		}
	} else {
		if err = reg.CreateProject(ctx, p); err != nil {
			return nil, fmt.Errorf("lifecycle: record system project: %w", err)
		}
		if err = eng.storeKeys(ctx, config.SystemRef, keys); err != nil {
			return nil, err
		}
	}
	if err = eng.ensureFleetCredentials(ctx, plane.paths(p)); err != nil {
		return nil, err
	}
	if err = plane.startAPI(ctx, p, keys); err != nil {
		return nil, err
	}
	if err = reg.SetProjectStatus(ctx, config.SystemRef, registry.StatusActiveHealthy); err != nil {
		return nil, err
	}
	eng.startTimer(ctx, config.SystemRef)
	eng.event(ctx, config.SystemRef, "system.initialized", map[string]bool{"existing": existing})
	return node, nil
}

// initPendingWitness is the file supabase-postgres-start leaves in PGDATA until the
// first boot has finished; while it exists the launcher refuses to start the cluster.
const initPendingWitness = ".supabase-postgres-init-pending"

// staleSystem decides whether the system cluster on disk is the leftover of an init
// that was interrupted, so that it holds nothing worth keeping: either the launcher's
// first boot never finished (pending), or the cluster has no registry or no system
// credentials and no other project. It starts the cluster to look. A cluster that has
// other projects but lost the system credentials is not touched; the error says how to
// recover.
func (pl *PostgresPlane) staleSystem(ctx context.Context, p *registry.Project, pending bool) (stale bool, why string, err error) {
	if pending {
		return true, "the first boot did not finish (" + initPendingWitness + " is present)", nil
	}
	dir := pl.cfg.Paths().Project(config.SystemRef)
	if err := pl.startRendered(ctx, p); err != nil {
		return false, "", fmt.Errorf("%w; if %s holds nothing you need (a failed first init), stop the units, remove that directory and run `sbctl system init` again", err, dir)
	}
	pp := pl.paths(p)
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return false, "", err
	}
	var hasDB bool
	err = c.QueryRow(ctx, `select exists (select 1 from pg_database where datname = 'sbctl')`).Scan(&hasDB)
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return false, "", err
	}
	if !hasDB {
		return true, "the sbctl registry database was never created", nil
	}
	rc, err := connect(ctx, socketDSN(pp, "sbctl"))
	if err != nil {
		return false, "", err
	}
	defer rc.Close(context.WithoutCancel(ctx))
	var hasTables bool
	if err := rc.QueryRow(ctx, `select to_regclass('sbctl.project_secrets') is not null and to_regclass('sbctl.projects') is not null`).Scan(&hasTables); err != nil {
		return false, "", err
	}
	if !hasTables {
		return true, "the registry schema was never created", nil
	}
	var secretRows, others int
	if err := rc.QueryRow(ctx, `select (select count(*) from sbctl.project_secrets where ref = 'system'),
	                                    (select count(*) from sbctl.projects where ref <> 'system')`).Scan(&secretRows, &others); err != nil {
		return false, "", err
	}
	switch {
	case secretRows > 0:
		return false, "", nil
	case others == 0:
		return true, "the registry holds no system credentials and no projects", nil
	default:
		return false, "", fmt.Errorf("lifecycle: the registry in %s has %d project(s) but no system credentials; restore the system secrets from a registry backup, or, to give up on the data, stop the sb-* units, remove %s and run `sbctl system init` (existing projects stay on disk but are no longer registered)", dir, others, dir)
	}
}

// keysStored reports whether the system credentials are in the registry.
func keysStored(n *Node, p *registry.Project) bool {
	if n.Registry == nil {
		return false
	}
	_, err := n.Registry.GetSecret(context.Background(), p.Ref, secrets.NameJWTSecret)
	return err == nil
}

func (o *OpenOptions) now() time.Time { return time.Now() }

// createSystemDatabases creates the databases of SystemDatabases that do not exist. Each
// fleet database is owned by its FleetRole (a login role without a password until
// ensureFleetCredentials sets one), holds a same-named schema owned by that role, and is
// closed to PUBLIC (so is the registry database). Databases and schemas an earlier version left owned by
// supabase_admin are handed over.
func createSystemDatabases(ctx context.Context, pp pgPaths) error {
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	exec := func(format string, args ...any) error {
		var stmt string
		if err := c.QueryRow(ctx, format, args...).Scan(&stmt); err != nil {
			return err
		}
		_, err := c.Exec(ctx, stmt)
		return err
	}
	owners := map[string]string{}
	for _, fr := range FleetRoles {
		owners[fr.Database] = fr.Role
		var exists bool
		if err := c.QueryRow(ctx, `select exists (select 1 from pg_roles where rolname = $1)`, fr.Role).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := exec(`select format('create role %I login nosuperuser nocreatedb nocreaterole noinherit', $1::text)`, fr.Role); err != nil {
				return fmt.Errorf("lifecycle: create role %s: %w", fr.Role, err)
			}
		}
	}
	for _, db := range SystemDatabases {
		owner := RoleAdmin
		if o, ok := owners[db]; ok {
			owner = o
		}
		var exists bool
		if err := c.QueryRow(ctx, `select exists (select 1 from pg_database where datname = $1)`, db).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := exec(`select format('create database %I owner %I', $1::text, $2::text)`, db, owner); err != nil {
				return fmt.Errorf("lifecycle: create database %s: %w", db, err)
			}
		} else if err := exec(`select format('alter database %I owner to %I', $1::text, $2::text)`, db, owner); err != nil {
			return err
		}
		if err := exec(`select format('revoke all on database %I from public', $1::text)`, db); err != nil {
			return err
		}
		if db == "sbctl" {
			continue // the registry creates its own schema
		}
		if err := ensureSchema(ctx, pp, db, owner); err != nil {
			return err
		}
	}
	_, err = c.Exec(ctx, `grant connect, create on database "_storage" to supabase_storage_admin`)
	return err
}

func ensureSchema(ctx context.Context, pp pgPaths, db, owner string) error {
	c, err := connect(ctx, socketDSN(pp, db))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var stmt string
	if err := c.QueryRow(ctx, `select format('create schema if not exists %I authorization %I', $1::text, $2::text)`, db, owner).Scan(&stmt); err != nil {
		return err
	}
	if _, err = c.Exec(ctx, stmt); err != nil {
		return err
	}
	if err := c.QueryRow(ctx, `select format('alter schema %I owner to %I', $1::text, $2::text)`, db, owner).Scan(&stmt); err != nil {
		return err
	}
	_, err = c.Exec(ctx, stmt)
	return err
}

// SystemStatus checks the system project's units without needing the registry.
func SystemStatus(ctx context.Context, cfg *config.Config, o OpenOptions) ([]ServiceHealth, error) {
	arts, err := o.artifactStore(cfg)
	if err != nil {
		return nil, err
	}
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, err
	}
	plane := NewPostgresPlane(cfg, sup, arts, registry.NewMemory(), o.planeOptions())
	return plane.Health(ctx, systemProject(cfg, nil), nil), nil
}

// StopAll stops every project's units found in the state directory, user projects
// first and the system project last, so that nothing is left running. It does not
// need the registry. Units that do not run are skipped.
func StopAll(ctx context.Context, cfg *config.Config, o OpenOptions) error {
	arts, err := o.artifactStore(cfg)
	if err != nil {
		return err
	}
	sup, err := o.supervisor(cfg)
	if err != nil {
		return err
	}
	plane := NewPostgresPlane(cfg, sup, arts, registry.NewMemory(), o.planeOptions())
	ents, err := os.ReadDir(filepath.Join(cfg.StateDir, "projects"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var refs []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != config.SystemRef {
			refs = append(refs, e.Name())
		}
	}
	sort.Strings(refs)
	refs = append(refs, config.SystemRef)
	var first error
	for _, ref := range refs {
		if err := plane.Stop(ctx, ref); err != nil && first == nil {
			first = fmt.Errorf("stop %s: %w", ref, err)
		}
	}
	return first
}

// UnitInstaller returns a function that makes systemd re-read unit files when reload
// is set and enables the system project's units for boot. It needs root: the polkit rule
// deliberately does not grant daemon-reload or unit-file management to the sbctl user.
// It returns an error when the configured supervisor has no systemd (the exec backend,
// or a non-Linux build).
func (o OpenOptions) UnitInstaller(cfg *config.Config) (func(ctx context.Context, reload bool) error, error) {
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, err
	}
	r, ok := sup.(interface{ Reload(context.Context) error })
	en, ok2 := sup.(units.Enabler)
	if !ok || !ok2 {
		closeSup(sup)
		return nil, errors.New("the configured supervisor does not use systemd")
	}
	return func(ctx context.Context, reload bool) error {
		defer closeSup(sup)
		if reload {
			if err := r.Reload(ctx); err != nil {
				return err
			}
		}
		return en.Enable(ctx, config.UnitName(config.SvcPostgres, config.SystemRef), config.UnitName(config.SvcGoTrue, config.SystemRef))
	}, nil
}

func closeSup(s units.Supervisor) {
	if c, ok := s.(interface{ Close() }); ok {
		c.Close()
	}
}
