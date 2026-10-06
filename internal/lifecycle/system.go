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
	"time"

	"github.com/OWNER/sbctl/internal/artifacts"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// SystemDatabases are created in the system cluster next to "postgres" (GoTrue's
// dashboard auth schema) and "sbctl" (the registry). Each holds a schema of the same
// name, owned by supabase_admin. The fleet services connect as supabase_admin;
// supabase_storage_admin may also use _storage.
var SystemDatabases = []string{"sbctl", "_supavisor", "_realtime", "_storage"}

// OpenOptions configure Open and InitSystem.
type OpenOptions struct {
	Log *slog.Logger
	// ConfigPath, ArchiveCommand: see PlaneOptions.
	ConfigPath     string
	ArchiveCommand string
	Fleet          fleet.Fleet
	Backup         BaseBackuper
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
	return PlaneOptions{Log: o.log(), ConfigPath: o.ConfigPath, ArchiveCommand: o.ArchiveCommand, Backup: o.Backup}
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
	plane := NewPostgresPlane(cfg, sup, arts, reg, o.planeOptions())
	eng := NewEngine(cfg, reg, sec, arts, plane, Options{Log: o.log(), Fleet: o.Fleet, Backup: o.Backup})
	return &Node{Cfg: cfg, Secrets: sec, Supervisor: sup, Artifacts: arts, Registry: reg, Plane: plane, Engine: eng}, nil
}

// systemProject is the registry view of the system cluster.
func systemProject(cfg *config.Config, versions map[string]string) *registry.Project {
	return &registry.Project{
		Ref: config.SystemRef, Seq: 0, Name: "system", Region: "local", Engine: registry.EnginePostgres,
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
	eng := NewEngine(cfg, reg, sec, arts, plane, Options{Log: log, Fleet: o.Fleet, Backup: o.Backup})
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
	if err = plane.startAPI(ctx, p, keys); err != nil {
		return nil, err
	}
	if err = reg.SetProjectStatus(ctx, config.SystemRef, registry.StatusActiveHealthy); err != nil {
		return nil, err
	}
	if en, ok := sup.(units.Enabler); ok {
		if err = en.Enable(ctx, config.UnitName(config.SvcPostgres, config.SystemRef), config.UnitName(config.SvcGoTrue, config.SystemRef)); err != nil {
			return nil, err
		}
	}
	eng.event(ctx, config.SystemRef, "system.initialized", map[string]bool{"existing": existing})
	return node, nil
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

// createSystemDatabases creates the databases of SystemDatabases that do not exist and
// the same-named schema in each.
func createSystemDatabases(ctx context.Context, pp pgPaths) error {
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	for _, db := range SystemDatabases {
		var exists bool
		if err := c.QueryRow(ctx, `select exists (select 1 from pg_database where datname = $1)`, db).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			var stmt string
			if err := c.QueryRow(ctx, `select format('create database %I owner %I', $1::text, $2::text)`, db, RoleAdmin).Scan(&stmt); err != nil {
				return err
			}
			if _, err := c.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("lifecycle: create database %s: %w", db, err)
			}
		}
		if db == "sbctl" {
			continue // the registry creates its own schema
		}
		if err := ensureSchema(ctx, pp, db); err != nil {
			return err
		}
	}
	_, err = c.Exec(ctx, `grant connect, create on database "_storage" to supabase_storage_admin`)
	return err
}

func ensureSchema(ctx context.Context, pp pgPaths, db string) error {
	c, err := connect(ctx, socketDSN(pp, db))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var stmt string
	if err := c.QueryRow(ctx, `select format('create schema if not exists %I authorization %I', $1::text, $2::text)`, db, RoleAdmin).Scan(&stmt); err != nil {
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

// Reloader returns a function that makes systemd re-read unit files, or an error when
// the configured supervisor has no systemd (the exec backend, or a non-Linux build).
func (o OpenOptions) Reloader(cfg *config.Config) (func(context.Context) error, error) {
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, err
	}
	r, ok := sup.(interface{ Reload(context.Context) error })
	if !ok {
		return nil, errors.New("the configured supervisor does not use systemd")
	}
	return func(ctx context.Context) error { defer closeSup(sup); return r.Reload(ctx) }, nil
}

func closeSup(s units.Supervisor) {
	if c, ok := s.(interface{ Close() }); ok {
		c.Close()
	}
}
