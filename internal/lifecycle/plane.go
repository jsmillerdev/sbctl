package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
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
	// ConfigPath is exported to Postgres as SUPAVISE_CONFIG so that archive_command, which
	// runs inside the postmaster, loads the same config file as the daemon. Not exported
	// when archiving goes through the daemon's WAL relay (config.Backup.WALRelay): the
	// cluster's unit has no business reading the config file then.
	ConfigPath string
	// ArchiveReady is called with the ref after a project's directories exist and before
	// its cluster starts: the daemon serves the project's WAL relay socket from then on
	// (backup.Relay.Ensure), so the first archive_command does not wait for the next sweep.
	ArchiveReady func(ref string)
	// Backup serves DataPlane.Snapshot; nil makes Snapshot return ErrNoSnapshot.
	Backup BaseBackuper
	// PostgresReadyTimeout bounds a cold start including first-boot migrations (default
	// 3 minutes); ServiceReadyTimeout bounds GoTrue and PostgREST (default 60 seconds).
	PostgresReadyTimeout time.Duration
	ServiceReadyTimeout  time.Duration
	HTTPClient           *http.Client
	// Settings supplies the project's saved settings for the units' environment and the
	// cluster's arguments; nil renders the defaults (tests, the system project).
	Settings Settings
	// SystemAuth supplies what supavise-gotrue@system needs for dashboard SSO (see SystemAuth); nil
	// renders it as before: sign-up closed, no SAML.
	SystemAuth func(ctx context.Context) (*SystemAuth, error)
	// RestoreCommandFor builds restore_command for ref's cluster: the node's own relay fetch for
	// ref's archive (backup.RestoreCommandFor, which this package cannot import). DemoteToReplica
	// writes it into the cluster it turns into a standby.
	RestoreCommandFor func(ref string) string
	// ReplicaReadyTimeout bounds the start of a standby until it accepts connections (default 10
	// minutes: it may replay a backlog of archived WAL first).
	ReplicaReadyTimeout time.Duration
	// ClusterSQL replaces the SQL the plane asks of a cluster (tests).
	ClusterSQL ClusterSQL
}

// SystemAuth is the configuration of supavise-gotrue@system for single sign-on. With it the system
// GoTrue accepts SAML sign-ins and its sign-up is open, which is safe because every user it
// would create is first checked by the before-user-created hook (internal/sso/hook.go), served
// by the daemon, which allows registered SSO providers and invited addresses only and refuses
// everything else, and refuses when it cannot answer.
type SystemAuth struct {
	// SigningKey is GOTRUE_SAML_PRIVATE_KEY.
	SigningKey string
	// HookURL and HookSecret are the before-user-created hook.
	HookURL    string
	HookSecret string
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
	return pl.prepareAt(p, keys, pl.paths(p), false)
}

// prepareAt is prepare for the cluster layout pp: the canonical one of a primary, or the replica's
// (replica: no GoTrue directory, a replica has no GoTrue).
func (pl *PostgresPlane) prepareAt(p *registry.Project, keys *secrets.ProjectKeys, pp pgPaths, replica bool) error {
	if n := len(pp.SockFile); n > unixSocketMax {
		return fmt.Errorf("lifecycle: unix socket path %s is %d bytes, over the %d the OS allows; use a shorter state_dir", pp.SockFile, n, unixSocketMax)
	}
	// WALDir is the one directory of the project's backup path the cluster's unit sees
	// (read-only): the daemon serves the WAL relay socket in it.
	dirs := []string{pl.cfg.Paths().Project(p.Ref), pp.Dir, pp.Sock, pl.cfg.Paths().WALDir(p.Ref)}
	if !replica {
		dirs = append(dirs, pl.cfg.Paths().ProjectService(p.Ref, config.SvcGoTrue))
	}
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
	if _, err := writeFile(pp.HBA, []byte(hbaRules), 0o600); err != nil {
		return err
	}
	if _, err := writeFile(pp.RootKey, []byte(keys.PGSodiumRootKey), 0o600); err != nil {
		return err
	}
	if pl.opts.ArchiveReady != nil {
		pl.opts.ArchiveReady(p.Ref)
	}
	return nil
}

// hbaRules is the cluster's pg_hba.conf. The artifact's default trusts every loopback
// connection, which on a shared host means every local user; this file trusts only the
// unix socket in the supavise-private project directory.
const hbaRules = `# Generated by supavise; changes are overwritten.
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
	spec, err := pl.postgresSpec(ctx, p, keys)
	if err != nil {
		return err
	}
	return pl.startCluster(ctx, spec, pl.paths(p), pl.opts.PostgresReadyTimeout)
}

// startCluster renders the cluster unit spec, restarts a running cluster whose settings changed
// (or defers the restart, see DeferRestarts), starts it and waits until it answers on pp. A
// replica's cluster takes the same path with its own spec and port.
func (pl *PostgresPlane) startCluster(ctx context.Context, spec units.Spec, pp pgPaths, ready time.Duration) error {
	var err error
	before := pl.digest(spec)
	changed := false
	if cr, ok := pl.sup.(units.ChangeRenderer); ok {
		if changed, err = cr.RenderChanged(ctx, spec); err != nil {
			return err
		}
	} else if err := pl.sup.Render(ctx, spec); err != nil {
		return err
	}
	if changed || pl.hasHeld(spec) {
		// A running cluster keeps the settings it started with (Start on a running unit is
		// a no-op), so changed sizing, archive_command or launcher path need a restart.
		st, serr := pl.sup.Status(ctx, spec.Unit())
		running := serr != nil || st.State == units.StateActive || st.State == units.StateActivating
		switch {
		case !running:
			pl.clearHeld(spec)
		case serr == nil && pl.heldState(spec, st) == heldSettled:
			// The cluster runs exactly the files rendered now: a rollback rendered back the files
			// that a held-back restart never replaced.
		case !changed:
			// Rendered earlier, the restart held back, and nothing deferred now: it waits for the
			// rollout (PendingRestart), the only restart of clusters that runs in canary order.
			pl.log.Info("postgres restart still held back; `supavise upgrade` restarts it", "unit", spec.Unit())
		case restartsDeferred(ctx):
			// A restart stops the project's GoTrue and PostgREST with it (their units require the
			// cluster), so it belongs to the upgrade's rollout like theirs (RestartPending).
			pl.holdBack(spec, before)
			pl.log.Info("postgres settings changed; the restart waits for the upgrade's rollout", "unit", spec.Unit())
		case serr != nil:
			// Cannot tell whether it runs. Start on a running unit is a no-op, so skipping
			// the stop would leave the cluster on its old settings without a trace; Stop on a
			// stopped unit is harmless, so stop it.
			pl.log.Warn("postgres settings changed; unit status unknown, restarting to be safe", "unit", spec.Unit(), "error", serr)
			if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
				return err
			}
			pl.clearHeld(spec)
		default:
			pl.log.Info("postgres settings changed; restarting the running cluster", "unit", spec.Unit())
			if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
				return err
			}
			pl.clearHeld(spec)
		}
	}
	if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
		return err
	}
	return pl.wait(ctx, spec.Unit(), "PostgreSQL", ready, func(ctx context.Context) error { return pl.sql().Ping(ctx, addrOf(pp)) })
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
	specs, err := pl.apiSpecs(ctx, p, keys)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if err := pl.renderStopIfChanged(ctx, spec); err != nil {
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

// renderStopIfChanged renders spec and, when the unit runs older files than the ones rendered
// (changed now, rendered earlier and left running, or held back), stops the unit, so that the Start
// that follows runs the new files: Start on a running unit is a no-op, and GoTrue or PostgREST would
// otherwise keep the binary and settings it started with after a release or a setting changed
// what the unit runs. An unchanged unit is left alone. A context marked DeferRestarts renders
// and leaves the unit running, with a mark that the restart is owed: the upgrade's rollout restarts
// it (RestartPending).
func (pl *PostgresPlane) renderStopIfChanged(ctx context.Context, spec units.Spec) error {
	before := pl.digest(spec)
	changed, err := pl.render(ctx, spec)
	if err != nil {
		return err
	}
	st, err := pl.sup.Status(ctx, spec.Unit())
	if err != nil || (st.State != units.StateActive && st.State != units.StateActivating) {
		pl.clearHeld(spec)
		return nil
	}
	held := pl.heldState(spec, st)
	if held == heldSettled || (!changed && held == heldNone && !pl.filesNewerThan(spec, st)) {
		return nil
	}
	if restartsDeferred(ctx) {
		if changed {
			pl.holdBack(spec, before)
		}
		pl.log.Info("service files changed; the restart waits for the upgrade's rollout", "unit", spec.Unit())
		return nil
	}
	pl.log.Info("service files changed; restarting the running unit", "unit", spec.Unit())
	if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
		return err
	}
	pl.clearHeld(spec)
	return nil
}

// render renders spec and reports whether its files changed.
func (pl *PostgresPlane) render(ctx context.Context, spec units.Spec) (changed bool, err error) {
	if cr, ok := pl.sup.(units.ChangeRenderer); ok {
		return cr.RenderChanged(ctx, spec)
	}
	return false, pl.sup.Render(ctx, spec)
}

// filesNewerThan reports whether spec's rendered files were written after the process of the unit
// started, which is how a restart that was held back (or that the daemon's stop cut off) shows. A
// supervisor that cannot say when the unit started says no.
func (pl *PostgresPlane) filesNewerThan(spec units.Spec, st units.Status) bool {
	if st.Since.IsZero() {
		return false
	}
	files := units.FilesFor(pl.cfg, spec)
	for _, f := range []string{files.Env, files.Run} {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(st.Since) {
			return true
		}
	}
	return false
}

// A restart the daemon held back is recorded in a mark next to the unit's env file
// (<svc>.held), which holds the digest of the files the running process started with. The mark makes
// the owed restart visible to the rollout however long ago the files were rendered, and lets a
// rollback tell a unit that never restarted (the files rendered back are the digest in its mark: it
// already runs them) from one the rollout restarted onto the new files.
type heldState int

const (
	heldNone    heldState = iota // no restart is owed
	heldPending                  // the unit runs files other than the rendered ones
	heldSettled                  // the unit runs exactly the rendered files; the mark is gone
)

func (pl *PostgresPlane) heldPath(spec units.Spec) string {
	return strings.TrimSuffix(units.FilesFor(pl.cfg, spec).Env, ".env") + ".held"
}

// digest hashes the rendered env file and run script of spec; "" when there are none.
func (pl *PostgresPlane) digest(spec units.Spec) string {
	files := units.FilesFor(pl.cfg, spec)
	h := sha256.New()
	found := false
	for _, f := range []string{files.Env, files.Run} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		found = true
		h.Write([]byte(f))
		h.Write(b)
	}
	if !found {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (pl *PostgresPlane) hasHeld(spec units.Spec) bool {
	_, err := os.Stat(pl.heldPath(spec))
	return err == nil
}

// holdBack records that spec's running unit keeps the files with digest before. A mark that is
// there already names older files and stays.
func (pl *PostgresPlane) holdBack(spec units.Spec, before string) {
	if pl.hasHeld(spec) {
		return
	}
	if before == "" {
		before = "-" // unknown: the restart is always owed
	}
	if err := os.WriteFile(pl.heldPath(spec), []byte(before+"\n"), 0o644); err != nil {
		pl.log.Warn("could not record the held-back restart; `supavise upgrade` may not find it", "unit", spec.Unit(), "error", err)
	}
}

func (pl *PostgresPlane) clearHeld(spec units.Spec) { _ = os.Remove(pl.heldPath(spec)) }

// heldState reads the mark of spec for a unit that runs (status st). A mark older than the unit's
// process is stale (the unit restarted on its own since) and is removed. One that names the digest
// of the files rendered now is removed too, and the files get the unit's start time as their
// modification time, so that filesNewerThan does not take the render that put them back for a
// restart that is owed.
func (pl *PostgresPlane) heldState(spec units.Spec, st units.Status) heldState {
	path := pl.heldPath(spec)
	fi, err := os.Stat(path)
	if err != nil {
		return heldNone
	}
	if !st.Since.IsZero() && fi.ModTime().Before(st.Since) {
		_ = os.Remove(path)
		return heldNone
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return heldPending
	}
	if d := strings.TrimSpace(string(b)); d != "" && d != "-" && d == pl.digest(spec) {
		_ = os.Remove(path)
		if !st.Since.IsZero() {
			files := units.FilesFor(pl.cfg, spec)
			for _, f := range []string{files.Env, files.Run} {
				_ = os.Chtimes(f, st.Since, st.Since)
			}
		}
		return heldSettled
	}
	return heldPending
}

// HeldRestart reports whether a restart of a unit of project ref is recorded as held back. It
// reads the marks only; Engine.PendingRestart is the check that also looks at the unit.
func HeldRestart(cfg *config.Config, ref string) bool {
	m, _ := filepath.Glob(filepath.Join(cfg.Paths().Project(ref), "*.held"))
	return len(m) > 0
}

type deferRestartsKey struct{}

// DeferRestarts marks ctx so that Start renders the PostgreSQL, GoTrue and PostgREST files of a
// project and leaves a running unit on its old ones. The daemon marks its start while `supavise
// upgrade` moves the node forward: a new binary can render every project's files differently, and
// the restart of fifty projects one after the other, with no canary and no stop at the first
// failure, belongs to the upgrade's rollout (Engine.RestartPending, `projects upgrade
// --restart-changed`), not to the daemon's start. The system cluster is not deferred: the daemon
// needs it, and the plan names its restart.
func DeferRestarts(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferRestartsKey{}, true)
}

func restartsDeferred(ctx context.Context) bool {
	v, _ := ctx.Value(deferRestartsKey{}).(bool)
	return v
}

// PendingRestarter is the optional Plane capability behind Engine.PendingRestart.
type PendingRestarter interface {
	// PendingRestart reports whether a running PostgreSQL, GoTrue or PostgREST unit of p runs older
	// files than the ones rendered for it now.
	PendingRestart(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error)
	// RestartPending stops the units PendingRestart names, starts them on the new files and waits
	// until they answer. It reports whether it restarted any. A restarted cluster takes the
	// project's GoTrue and PostgREST down with it, and they start again on the new files.
	RestartPending(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error)
}

var _ PendingRestarter = (*PostgresPlane)(nil)

// pendingAPI renders the API units of p and returns the ones that run older files.
func (pl *PostgresPlane) pendingAPI(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) ([]units.Spec, error) {
	specs, err := pl.apiSpecs(ctx, p, keys)
	if err != nil {
		return nil, err
	}
	var out []units.Spec
	for _, spec := range specs {
		changed, err := pl.render(ctx, spec)
		if err != nil {
			return nil, err
		}
		st, err := pl.sup.Status(ctx, spec.Unit())
		if err != nil || (st.State != units.StateActive && st.State != units.StateActivating) {
			pl.clearHeld(spec)
			continue
		}
		switch held := pl.heldState(spec, st); {
		case held == heldSettled:
		case changed, held == heldPending, pl.filesNewerThan(spec, st):
			out = append(out, spec)
		}
	}
	return out, nil
}

// pendingDatabase renders the cluster unit of p and returns its spec when the running cluster
// owes a restart: this render changed its files, or the daemon held the restart back (the mark). A
// setting an Owner saved without restarting also renders into the unit, but the upgrade does not
// owe that restart, so it does not count; a restart the release owes applies it too.
func (pl *PostgresPlane) pendingDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (*units.Spec, error) {
	spec, err := pl.postgresSpec(ctx, p, keys)
	if err != nil {
		return nil, err
	}
	changed, err := pl.render(ctx, spec)
	if err != nil {
		return nil, err
	}
	st, err := pl.sup.Status(ctx, spec.Unit())
	if err != nil || (st.State != units.StateActive && st.State != units.StateActivating) {
		pl.clearHeld(spec)
		return nil, nil
	}
	switch held := pl.heldState(spec, st); {
	case held == heldSettled:
	case changed, held == heldPending:
		return &spec, nil
	}
	return nil, nil
}

// PendingRestart implements PendingRestarter.
func (pl *PostgresPlane) PendingRestart(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error) {
	db, err := pl.pendingDatabase(ctx, p, keys)
	if err != nil || db != nil {
		return db != nil, err
	}
	pending, err := pl.pendingAPI(ctx, p, keys)
	return len(pending) > 0, err
}

// RestartPending implements PendingRestarter.
func (pl *PostgresPlane) RestartPending(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error) {
	// The rollout restarts what the daemon held back: nothing here may be deferred again.
	ctx = context.WithValue(ctx, deferRestartsKey{}, false)
	db, err := pl.pendingDatabase(ctx, p, keys)
	if err != nil {
		return false, err
	}
	api, err := pl.pendingAPI(ctx, p, keys)
	if err != nil || (db == nil && len(api) == 0) {
		return false, err
	}
	if db != nil {
		// GoTrue and PostgREST require the cluster, so they go down with it. Stop them first, in
		// the order Stop uses, then the cluster; startAPI starts them again after it.
		all, err := pl.apiSpecs(ctx, p, keys)
		if err != nil {
			return false, err
		}
		for i := len(all) - 1; i >= 0; i-- {
			if err := pl.sup.Stop(ctx, all[i].Unit()); err != nil {
				return false, err
			}
			pl.clearHeld(all[i])
		}
		pl.log.Info("postgres files changed; restarting the running cluster", "unit", db.Unit())
		if err := pl.sup.Stop(ctx, db.Unit()); err != nil {
			return false, err
		}
		pl.clearHeld(*db)
		if err := pl.StartDatabase(ctx, p, keys); err != nil {
			return false, err
		}
	} else {
		for _, spec := range api {
			pl.log.Info("service files changed; restarting the running unit", "unit", spec.Unit())
			if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
				return false, err
			}
			pl.clearHeld(spec)
		}
	}
	return true, pl.startAPI(ctx, p, keys)
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
	return pl.checkURL(ctx, url)
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
	specs, err := pl.apiSpecs(ctx, p, keys)
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
	out := []ServiceHealth{
		pl.serviceHealth(ctx, p.Ref, config.SvcPostgres, func(ctx context.Context) error { return ping(ctx, pp) }),
		pl.serviceHealth(ctx, p.Ref, config.SvcGoTrue, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, config.SvcGoTrue) }),
	}
	if hasPostgREST(p.Ref) {
		out = append(out, pl.serviceHealth(ctx, p.Ref, config.SvcPostgREST, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, config.SvcPostgREST) }))
	}
	return out
}

// serviceHealth is the health of svc's unit of ref: the unit state first, then a real request (f).
func (pl *PostgresPlane) serviceHealth(ctx context.Context, ref, svc string, f func(context.Context) error) ServiceHealth {
	h := ServiceHealth{Name: svc}
	st, err := pl.sup.Status(ctx, config.UnitName(svc, ref))
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

// Delete implements DataPlane: stop and remove the units and delete the project
// directory (cluster, env files, run scripts). It is safe to call on a half-created project.
func (pl *PostgresPlane) Delete(ctx context.Context, ref string) error {
	if ref != config.SystemRef && !secrets.ValidRef(ref) {
		return fmt.Errorf("lifecycle: refusing to delete %q: not a project ref", ref)
	}
	var first error
	// The bundler's instance for the project (supavise-edge-bundle@<ref>.service) holds the module
	// cache of its uploads, which nothing else may delete: removing the unit removes the
	// cache. It does nothing for a project whose sources were never bundled.
	us := append(pl.unitsOf(ref), config.UnitName(config.SvcEdgeBundle, ref))
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
