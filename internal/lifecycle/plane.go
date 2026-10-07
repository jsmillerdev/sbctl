package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// Artifacts is the part of artifacts.Store the lifecycle needs.
type Artifacts interface {
	// Dir returns the unpacked artifact root of svc, or an error if it is not fetched.
	Dir(svc string) (string, error)
	// Tag returns the pinned release tag of svc.
	Tag(svc string) (string, error)
}

// PlaneOptions tune a PostgresPlane. The zero value is production behavior.
type PlaneOptions struct {
	Log *slog.Logger
	// ArchiveCommand overrides archive_command. Empty means "<bin_path> wal push --ref
	// <ref> %p"; "off" turns archiving off (tests and nodes without a backup backend).
	ArchiveCommand string
	// ArchiveCommandFor builds archive_command per project (backup.ArchiveCommand, which
	// the lifecycle package cannot import); it wins over ArchiveCommand when it returns
	// a value. ArchiveTimeout is archive_timeout in seconds (default 900).
	ArchiveCommandFor func(ref string) string
	ArchiveTimeout    int
	// ConfigPath is exported to Postgres as SBCTL_CONFIG so that archive_command, which
	// runs inside the postmaster, loads the same config file as the daemon.
	ConfigPath string
	// Backup serves DataPlane.Snapshot; nil makes Snapshot return ErrNoSnapshot.
	Backup BaseBackuper
	// PostgresReadyTimeout bounds a cold start including first-boot migrations (default
	// 3 minutes); ServiceReadyTimeout bounds GoTrue and PostgREST (default 60 seconds).
	PostgresReadyTimeout time.Duration
	ServiceReadyTimeout  time.Duration
	HTTPClient           *http.Client
}

// PostgresPlane is the DataPlane of engine "postgres": one cluster, one GoTrue and one
// PostgREST per project, each as a unit of the Supervisor. The system project uses the
// same code with ref "system" and no PostgREST.
type PostgresPlane struct {
	cfg  *config.Config
	sup  units.Supervisor
	arts Artifacts
	reg  registry.Registry
	opts PlaneOptions
	log  *slog.Logger
	http *http.Client
}

var _ Plane = (*PostgresPlane)(nil)

// NewPostgresPlane builds the plane. reg is only used to look up a project's port
// sequence in Route, Usage and Delete.
func NewPostgresPlane(cfg *config.Config, sup units.Supervisor, arts Artifacts, reg registry.Registry, opts PlaneOptions) *PostgresPlane {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.PostgresReadyTimeout == 0 {
		opts.PostgresReadyTimeout = 3 * time.Minute
	}
	if opts.ServiceReadyTimeout == 0 {
		opts.ServiceReadyTimeout = 60 * time.Second
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 4 * time.Second}
	}
	return &PostgresPlane{cfg: cfg, sup: sup, arts: arts, reg: reg, opts: opts, log: opts.Log, http: hc}
}

// hasPostgREST reports whether ref runs PostgREST (every project but system).
func hasPostgREST(ref string) bool { return ref != config.SystemRef }

func (pl *PostgresPlane) paths(p *registry.Project) pgPaths {
	return pathsFor(pl.cfg, p.Ref, pl.cfg.PortsFor(p.Ref, p.Seq).Postgres)
}

// prepare creates the project directories and the files outside PGDATA: pg_hba.conf
// and the pgsodium root key.
func (pl *PostgresPlane) prepare(p *registry.Project, keys *secrets.ProjectKeys) error {
	pp := pl.paths(p)
	if n := len(pp.SockFile); n > unixSocketMax {
		return fmt.Errorf("lifecycle: unix socket path %s is %d bytes, over the %d the OS allows; use a shorter state_dir", pp.SockFile, n, unixSocketMax)
	}
	dirs := []string{pl.cfg.Paths().Project(p.Ref), pp.Dir, pp.Sock, pl.cfg.Paths().ProjectService(p.Ref, config.SvcGoTrue)}
	if hasPostgREST(p.Ref) {
		dirs = append(dirs, pl.cfg.Paths().ProjectService(p.Ref, config.SvcPostgREST))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	if err := os.Chmod(pp.Sock, 0o700); err != nil {
		return err
	}
	if err := pl.ensureBackupDir(p.Ref); err != nil {
		return err
	}
	if _, err := writeFile(pp.HBA, []byte(hbaRules), 0o600); err != nil {
		return err
	}
	_, err := writeFile(pp.RootKey, []byte(keys.PGSodiumRootKey), 0o600)
	return err
}

// ensureBackupDir creates <backups>/<ref> for the local (file://) backup backend. The
// systemd template binds that one directory into the cluster's namespace for
// archive_command, and a bind of a missing path is skipped, so it must exist before the
// first start. Other backends and a backend outside the state directory need nothing here.
func (pl *PostgresPlane) ensureBackupDir(ref string) error {
	dir, ok := strings.CutPrefix(pl.cfg.Backup.Backend, "file://")
	if !ok || filepath.Clean(dir) != pl.cfg.Paths().Backups() {
		return nil
	}
	return os.MkdirAll(filepath.Join(dir, ref), 0o700)
}

// hbaRules is the cluster's pg_hba.conf. The artifact's default trusts every loopback
// connection, which on a shared host means every local user; this file trusts only the
// unix socket in the sbctl-private project directory.
const hbaRules = `# Generated by sbctl; changes are overwritten.
local   all          supabase_admin                        trust
local   replication  supabase_admin                        trust
local   all          all                                   scram-sha-256
host    replication  supabase_replication_admin 127.0.0.1/32 scram-sha-256
host    replication  supabase_replication_admin ::1/128      scram-sha-256
host    all          all            127.0.0.1/32           scram-sha-256
host    all          all            ::1/128                scram-sha-256
`

func writeFile(path string, b []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(b) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == mode {
			return false, nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// Create implements DataPlane: it initializes (or seeds) the cluster, sets role
// passwords, and starts PostgreSQL, GoTrue and PostgREST. On error it leaves whatever
// it started running; the caller cleans up with Delete.
func (pl *PostgresPlane) Create(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, seed DataSeeder) error {
	if err := pl.createDatabase(ctx, p, keys, seed); err != nil {
		return err
	}
	return pl.startAPI(ctx, p, keys)
}

// ErrClusterExists is returned by Create when the project directory already holds a
// cluster and no seed was given. Create never initializes over it, and Engine.Create
// leaves the data alone instead of cleaning it up.
var ErrClusterExists = errors.New("lifecycle: directory already holds a cluster; refusing to initialize over it")

// createDatabase is Create without the API units: the cluster is initialized or seeded,
// started and ready, with its role passwords set.
func (pl *PostgresPlane) createDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, seed DataSeeder) error {
	pp := pl.paths(p)
	if _, err := os.Stat(filepath.Join(pp.Data, "PG_VERSION")); err == nil && seed == nil {
		return fmt.Errorf("%w: %s", ErrClusterExists, pp.Data)
	}
	if err := pl.prepare(p, keys); err != nil {
		return err
	}
	if seed != nil {
		if err := seed(ctx, p, pp.Data); err != nil {
			return fmt.Errorf("lifecycle: seed cluster: %w", err)
		}
		if err := ensureLauncherInvariants(pp.Data); err != nil {
			return err
		}
	}
	if err := pl.StartDatabase(ctx, p, keys); err != nil {
		return err
	}
	if seed == nil {
		// A seeded cluster already has these roles with these passwords (the restore
		// reuses the source project's keys), and may still be in recovery.
		if err := setRolePasswords(ctx, pp, pl.rolePasswords(keys)); err != nil {
			return err
		}
		// The first boot is over: render the unit again without the bootstrap password.
		// The files differ, so StartDatabase restarts the cluster, which also drops the
		// password from the postmaster's environment.
		return pl.StartDatabase(ctx, p, keys)
	}
	return nil
}

func (pl *PostgresPlane) rolePasswords(k *secrets.ProjectKeys) map[string]string {
	return map[string]string{
		RolePostgres: k.DBPassword, RoleAdmin: k.AdminPassword, RoleAuthn: k.AuthenticatorPassword,
		RoleAuthAdmin: k.AuthAdminPassword, RoleStorage: k.StorageAdminPassword, RoleReplication: k.ReplicationPassword,
	}
}

// ensureLauncherInvariants makes a seeded data directory acceptable to
// supabase-postgres-start, which refuses an initialized directory without a
// postmaster.opts (pg_basebackup does not copy that file).
func ensureLauncherInvariants(dataDir string) error {
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err != nil {
		return fmt.Errorf("lifecycle: seed left no cluster in %s", dataDir)
	}
	opts := filepath.Join(dataDir, "postmaster.opts")
	if fi, err := os.Stat(opts); err != nil || fi.Size() == 0 {
		return os.WriteFile(opts, []byte("postgres\n"), 0o600)
	}
	return nil
}

// StartDatabase implements Runner.
func (pl *PostgresPlane) StartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	if err := pl.prepare(p, keys); err != nil {
		return err
	}
	spec, err := pl.postgresSpec(p, keys)
	if err != nil {
		return err
	}
	changed := false
	if cr, ok := pl.sup.(units.ChangeRenderer); ok {
		if changed, err = cr.RenderChanged(ctx, spec); err != nil {
			return err
		}
	} else if err := pl.sup.Render(ctx, spec); err != nil {
		return err
	}
	if changed {
		// A running cluster keeps the settings it started with (Start on a running unit is
		// a no-op), so changed sizing, archive_command or launcher path need a restart.
		st, serr := pl.sup.Status(ctx, spec.Unit())
		switch {
		case serr != nil:
			// Cannot tell whether it runs. Start on a running unit is a no-op, so skipping
			// the stop would leave the cluster on its old settings without a trace; Stop on a
			// stopped unit is harmless, so stop it.
			pl.log.Warn("postgres settings changed; unit status unknown, restarting to be safe", "unit", spec.Unit(), "error", serr)
			if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
				return err
			}
		case st.State == units.StateActive || st.State == units.StateActivating:
			pl.log.Info("postgres settings changed; restarting the running cluster", "unit", spec.Unit())
			if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
				return err
			}
		}
	}
	if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
		return err
	}
	pp := pl.paths(p)
	return pl.wait(ctx, spec.Unit(), "PostgreSQL", pl.opts.PostgresReadyTimeout, func(ctx context.Context) error { return ping(ctx, pp) })
}

// startRendered starts the cluster unit from the files an earlier run rendered, which
// needs no credentials, and waits for it. The system project uses it to reach its own
// registry, where its credentials are stored.
func (pl *PostgresPlane) startRendered(ctx context.Context, p *registry.Project) error {
	unit := config.UnitName(config.SvcPostgres, p.Ref)
	files := units.FilesFor(pl.cfg, units.Spec{Service: config.SvcPostgres, Ref: p.Ref})
	if _, err := os.Stat(files.Run); err != nil {
		return fmt.Errorf("lifecycle: %s was never rendered (%w); is the state directory %s intact?", unit, err, pl.cfg.StateDir)
	}
	if err := pl.sup.Start(ctx, unit); err != nil {
		return err
	}
	pp := pl.paths(p)
	return pl.wait(ctx, unit, "PostgreSQL", pl.opts.PostgresReadyTimeout, func(ctx context.Context) error { return ping(ctx, pp) })
}

// Start implements Runner.
func (pl *PostgresPlane) Start(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	if err := pl.StartDatabase(ctx, p, keys); err != nil {
		return err
	}
	return pl.startAPI(ctx, p, keys)
}

func (pl *PostgresPlane) startAPI(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	specs, err := pl.apiSpecs(p, keys)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if err := pl.sup.Render(ctx, spec); err != nil {
			return err
		}
		if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
			return err
		}
		svc := spec.Service
		if err := pl.wait(ctx, spec.Unit(), svc, pl.opts.ServiceReadyTimeout, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, svc) }); err != nil {
			return err
		}
	}
	return nil
}

// wait polls check until it succeeds, the timeout passes, or the unit dies. A unit that
// failed or is restarting makes wait return at once with the end of its log.
func (pl *PostgresPlane) wait(ctx context.Context, unit, what string, timeout time.Duration, check func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for delay := 100 * time.Millisecond; ; {
		if last = check(ctx); last == nil {
			return nil
		}
		if st, err := pl.sup.Status(ctx, unit); err == nil {
			switch {
			case st.State == units.StateFailed, st.State == units.StateInactive, st.SubState == "auto-restart":
				return fmt.Errorf("lifecycle: %s (%s) is %s/%s: %v%s", what, unit, st.State, st.SubState, last, pl.logTail(unit))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lifecycle: %s (%s) not ready after %s: %v%s", what, unit, timeout, last, pl.logTail(unit))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay += 100 * time.Millisecond
		}
	}
}

func (pl *PostgresPlane) logTail(unit string) string {
	if t, ok := pl.sup.(units.LogTailer); ok {
		if s := t.Tail(unit, 15); s != "" {
			return "\n" + s
		}
	}
	return " (see `journalctl -u " + unit + "`)"
}

// checkHTTP asks GoTrue /health or PostgREST / for a 200.
func (pl *PostgresPlane) checkHTTP(ctx context.Context, p *registry.Project, svc string) error {
	ports := pl.cfg.PortsFor(p.Ref, p.Seq)
	var url string
	switch svc {
	case config.SvcGoTrue:
		url = fmt.Sprintf("http://127.0.0.1:%d/health", ports.GoTrue)
	case config.SvcPostgREST:
		url = fmt.Sprintf("http://127.0.0.1:%d/", ports.PostgREST)
	default:
		return fmt.Errorf("lifecycle: no health check for %s", svc)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := pl.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}

func (pl *PostgresPlane) unitsOf(ref string) []string {
	us := []string{config.UnitName(config.SvcPostgres, ref), config.UnitName(config.SvcGoTrue, ref)}
	if hasPostgREST(ref) {
		us = append(us, config.UnitName(config.SvcPostgREST, ref))
	}
	return us
}

// Stop implements Runner: PostgREST, GoTrue, then PostgreSQL (the reverse of start).
func (pl *PostgresPlane) Stop(ctx context.Context, ref string) error {
	us := pl.unitsOf(ref)
	var first error
	for i := len(us) - 1; i >= 0; i-- {
		if err := pl.sup.Stop(ctx, us[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Reconfigure implements Runner.
func (pl *PostgresPlane) Reconfigure(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	specs, err := pl.apiSpecs(p, keys)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if err := pl.sup.Render(ctx, spec); err != nil {
			return err
		}
	}
	for _, spec := range specs {
		// Restart every API unit whatever its state: the caller passes only projects that
		// should be running, and a rollback after a failed rotation must also bring back a
		// unit that failed on the new keys (it is "failed", not "active", by then).
		if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
			return err
		}
		if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
			return err
		}
		svc := spec.Service
		if err := pl.wait(ctx, spec.Unit(), svc, pl.opts.ServiceReadyTimeout, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, svc) }); err != nil {
			return err
		}
	}
	return nil
}

// Health implements Runner.
func (pl *PostgresPlane) Health(ctx context.Context, p *registry.Project, _ *secrets.ProjectKeys) []ServiceHealth {
	pp := pl.paths(p)
	check := func(svc string, f func(context.Context) error) ServiceHealth {
		h := ServiceHealth{Name: svc}
		st, err := pl.sup.Status(ctx, config.UnitName(svc, p.Ref))
		switch {
		case err != nil:
			h.Status, h.Error = "UNHEALTHY", err.Error()
		case st.State != units.StateActive:
			h.Status = "UNHEALTHY"
			if st.State == units.StateActivating {
				h.Status = "COMING_UP"
			}
			h.Error = fmt.Sprintf("unit is %s/%s", st.State, st.SubState)
		default:
			if err := f(ctx); err != nil {
				h.Status, h.Error = "UNHEALTHY", err.Error()
				if time.Since(st.Since) < 30*time.Second {
					h.Status = "COMING_UP"
				}
			} else {
				h.Healthy, h.Status = true, "ACTIVE_HEALTHY"
			}
		}
		return h
	}
	out := []ServiceHealth{
		check(config.SvcPostgres, func(ctx context.Context) error { return ping(ctx, pp) }),
		check(config.SvcGoTrue, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, config.SvcGoTrue) }),
	}
	if hasPostgREST(p.Ref) {
		out = append(out, check(config.SvcPostgREST, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, config.SvcPostgREST) }))
	}
	return out
}

// Delete implements DataPlane: stop and remove the units and delete the project
// directory (cluster, env files, run scripts). It is safe to call on a half-created project.
func (pl *PostgresPlane) Delete(ctx context.Context, ref string) error {
	if ref != config.SystemRef && !secrets.ValidRef(ref) {
		return fmt.Errorf("lifecycle: refusing to delete %q: not a project ref", ref)
	}
	var first error
	us := pl.unitsOf(ref)
	for i := len(us) - 1; i >= 0; i-- {
		if err := pl.sup.Remove(ctx, us[i]); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		// Do not delete data under a process that may still run.
		return first
	}
	return os.RemoveAll(pl.cfg.Paths().Project(ref))
}

// Snapshot implements DataPlane through the BaseBackuper in PlaneOptions.
func (pl *PostgresPlane) Snapshot(ctx context.Context, ref string) (*registry.Backup, error) {
	if pl.opts.Backup == nil {
		return nil, ErrNoSnapshot
	}
	return pl.opts.Backup.BaseBackup(ctx, ref)
}

func (pl *PostgresPlane) lookup(ctx context.Context, ref string) (*registry.Project, error) {
	return pl.reg.GetProject(ctx, ref)
}

// Route implements DataPlane.
func (pl *PostgresPlane) Route(ctx context.Context, ref string) (Upstreams, error) {
	p, err := pl.lookup(ctx, ref)
	if err != nil {
		return Upstreams{}, err
	}
	ports := pl.cfg.PortsFor(p.Ref, p.Seq)
	addr := func(port int) string {
		if port == 0 {
			return ""
		}
		return "127.0.0.1:" + strconv.Itoa(port)
	}
	return Upstreams{Postgres: addr(ports.Postgres), GoTrue: addr(ports.GoTrue), PostgREST: addr(ports.PostgREST)}, nil
}

// Usage implements DataPlane: bytes under the project directory and the memory the
// supervisor reports for its units.
func (pl *PostgresPlane) Usage(ctx context.Context, ref string) (Usage, error) {
	var u Usage
	root := pl.cfg.Paths().Project(ref)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				u.DiskBytes += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		return u, err
	}
	for _, unit := range pl.unitsOf(ref) {
		if st, err := pl.sup.Status(ctx, unit); err == nil {
			u.MemoryBytes += st.MemoryBytes
		}
	}
	return u, nil
}
