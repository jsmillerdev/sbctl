package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jsmillerdev/supavise/internal/artifacts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// Project upgrade: moving a project's Postgres, GoTrue and PostgREST to other release tags
// (in practice the node's pins) on the same data directory.
//
// The shape follows hosted Supabase: the project's Owner or Administrator asks for it, the
// project shows UPGRADING with a tracking id and a progress that Studio draws, and a failure
// brings the original back online. What differs is what the upgrade is. Hosted copies the data
// into a new instance with pg_upgrade. Here the data stays where it is: the three services are
// minor-release changes of the same major version, so the on-disk format does not change, and an
// upgrade is a restart on other binaries.
//
// Order:
//
//  1. UPGRADING, a registry row of the upgrade (its tracking id), the artifacts of the target
//     versions fetched and verified (nothing runs yet, so a failure here changes nothing).
//  2. A fresh base backup (reason pre-upgrade) while the project still serves. It fails closed:
//     without a backup nothing is stopped.
//  3. Under the project's lock: the backup timer stops; with a new Postgres the shared services
//     let go of the database and all three units stop, otherwise only GoTrue and PostgREST restart;
//     the units are rendered from the target versions and started; each answers a real request
//     (GoTrue runs its schema migrations before it starts). Then a health check of every service.
//  4. The registry records the target versions and ACTIVE_HEALTHY, in one write.
//
// Any failure in step 3 renders the previous versions again, starts them and checks them. The
// data directory is the same throughout and is not restored: a Postgres, GoTrue or PostgREST
// minor release does not change the format of the files, so the previous binaries read what the
// new ones left. GoTrue's and Storage's migrations are the exception in principle: they run when a
// service starts and only go forward, so a previous GoTrue may meet a schema that a newer one
// migrated. The base backup of step 2 is the way back for the data (`supavise backups restore`);
// the upgrade's status, events and error name its id. Recover (a daemon stopped in the middle)
// stops the project's units and lets StartActive start them on the recorded, previous versions.

// Progress values of the Management API's upgrade status, in the order Studio's upgrade screen
// draws them. The names are hosted's (they describe a pg_upgrade onto a new instance); the
// comment says what they mean here.
const (
	ProgressRequested      = "0_requested"                              // accepted
	ProgressStarted        = "1_started"                                // fetching and verifying artifacts
	ProgressArtifactsReady = "2_launched_upgraded_instance"             // artifacts ready; base backup running
	ProgressBackupDone     = "3_detached_volume_from_upgraded_instance" // backup taken; about to stop services
	ProgressStopping       = "4_attached_volume_to_original_instance"   // stopping services
	ProgressDatabase       = "5_initiated_data_upgrade"                 // starting PostgreSQL on its new version
	ProgressServices       = "6_completed_data_upgrade"                 // starting GoTrue (migrations) and PostgREST
	ProgressHealth         = "7_detached_volume_from_original_instance" // health check of every service
	ProgressCommit         = "8_attached_volume_to_upgraded_instance"   // recording the new versions
	ProgressCompleted      = "9_completed_upgrade"                      // done
)

// Error values of the status: the stage that failed.
const (
	UpgradeErrArtifacts = "1_upgraded_instance_launch_failed" // fetching or verifying the target artifacts
	UpgradeErrBackup    = "4_data_upgrade_initiation_failed"  // the pre-upgrade backup
	UpgradeErrStart     = "5_data_upgrade_completion_failed"  // stopping or starting the services
	UpgradeErrHealth    = "8_upgrade_completion_failed"       // the health gate, recording the result, an interrupted upgrade
)

// Events of an upgrade (the registry's events table; Studio does not read them).
const (
	EventUpgradeStarted    = "project.upgrade_started"
	EventUpgradeBackupDone = "project.upgrade_backup_done"
	EventUpgradeSucceeded  = "project.upgrade_succeeded"
	EventUpgradeFailed     = "project.upgrade_failed"
)

var (
	// ErrUpgradeNotNeeded: the project already runs the target versions.
	ErrUpgradeNotNeeded = errors.New("lifecycle: the project already runs the target versions")
	// ErrUpgradeUnsupported: no upgrade path from the project's versions to the target (another
	// Postgres major version, a downgrade) or a rule of the node forbids it.
	ErrUpgradeUnsupported = errors.New("lifecycle: no upgrade path to the target versions")
	// ErrNoBackupEngine: the node has no backup service, and an upgrade does not start without
	// the backup it would roll back to.
	ErrNoBackupEngine = errors.New("lifecycle: this node has no backup service, and a project is not upgraded without a fresh backup")
)

// Blocker types that Studio's upgrade warning knows (ProjectUpgradeEligibility validation_errors)
// are named as it names them; the rest are ours and appear in the CLI.
const (
	BlockerHibernating   = "project_hibernating"
	BlockerStatus        = "project_status"
	BlockerNoUpgradePath = "no_upgrade_path"
	BlockerNoBackup      = "no_backup_service"
	BlockerSystem        = "system_project"
)

// UpgradeBlocker is one reason a project cannot be upgraded now.
type UpgradeBlocker struct {
	Type    string
	Message string
}

// UpgradeEligibility answers whether a project can move from the versions it runs to target
// versions, and what that would do.
type UpgradeEligibility struct {
	Ref string
	// Eligible is true when there is something to upgrade and nothing in the way.
	Eligible bool
	// UpToDate: the project already runs every target version.
	UpToDate bool
	// Current is what the project runs; Target is what it would run (the node's pins for the
	// services a request does not name); Latest is the node's pins.
	Current, Target, Latest map[string]string
	// Changes lists the services that would change, in start order.
	Changes []ServiceChange
	// CurrentMajor and TargetMajor are the Postgres major versions of Current and Target.
	CurrentMajor, TargetMajor int
	// PostgresRestart: PostgreSQL itself restarts (its release changes), so every connection
	// drops. Otherwise only GoTrue and PostgREST restart and the database stays up.
	PostgresRestart bool
	Blockers        []UpgradeBlocker
	// DowntimeHours estimates how long the project is offline; the base backup before it runs
	// while the project serves and does not count.
	DowntimeHours float64
	// Notes are things to know before upgrading (not blockers).
	Notes []string
}

// UpgradeRequest names what a project is upgraded to.
type UpgradeRequest struct {
	// Target maps service name (config.Svc*) to release tag; nil means the node's pins. A
	// service it does not name stays on the version the project runs.
	Target map[string]string
	// TargetVersion is the version the caller asked for ("17", an app version); it is kept in
	// the upgrade's status as it was written.
	TargetVersion string
}

// ProjectUpgrader is the optional Manager capability behind the Management API's upgrade
// routes and `supavise projects upgrade`. The Engine has it; callers ask the Manager for it.
type ProjectUpgrader interface {
	// NodeVersions returns the node's pins for the project services.
	NodeVersions() (map[string]string, error)
	// EffectiveVersions returns what p runs.
	EffectiveVersions(p *registry.Project) (map[string]string, error)
	// UpgradeEligibility answers whether ref can move to the node's pins.
	UpgradeEligibility(ctx context.Context, ref string) (*UpgradeEligibility, error)
	// BeginUpgrade checks that ref can be upgraded, moves it to UPGRADING and returns the
	// handle that runs the upgrade. The caller must call Run; until then the project stays
	// UPGRADING. ErrInvalidState, ErrUpgradeNotNeeded, ErrUpgradeUnsupported and
	// ErrNoBackupEngine refuse it before anything changes.
	BeginUpgrade(ctx context.Context, ref string, req UpgradeRequest) (UpgradeRun, error)
}

// UpgradeRun is an upgrade that BeginUpgrade started.
type UpgradeRun interface {
	// Upgrade is the current state of the upgrade (its tracking id, versions, progress).
	Upgrade() registry.Upgrade
	// Run performs the upgrade and settles the project's status. It returns nil when the project
	// runs the target versions, and an error otherwise; the error says whether the previous
	// versions were brought back.
	Run(ctx context.Context) error
}

// UpgradeBackuper is an optional BaseBackuper capability: the backup before an upgrade, recorded
// with the reason "pre-upgrade" (backup.Service has it). The Engine prefers it over BaseBackup.
type UpgradeBackuper interface {
	UpgradeBackup(ctx context.Context, ref string) (*registry.Backup, error)
}

var _ ProjectUpgrader = (*Engine)(nil)

// Time the project is offline when only the API units restart, and when PostgreSQL does too,
// in hours (hosted reports hours). Starting a cluster takes seconds; GoTrue's migrations and a
// slow disk are what the margin is for.
const (
	downtimeAPIHours      = 0.05
	downtimePostgresHours = 0.25
)

// UpgradeEligibility implements ProjectUpgrader: whether ref can move to the node's pins.
func (e *Engine) UpgradeEligibility(ctx context.Context, ref string) (*UpgradeEligibility, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return e.plan(p, nil)
}

// plan computes the eligibility of p for target (nil: the node's pins).
func (e *Engine) plan(p *registry.Project, target map[string]string) (*UpgradeEligibility, error) {
	node, err := e.versions()
	if err != nil {
		return nil, err
	}
	cur, err := e.EffectiveVersions(p)
	if err != nil {
		return nil, err
	}
	if target == nil {
		target = node
	}
	to := mergeVersions(cur, target)
	el := &UpgradeEligibility{Ref: p.Ref, Current: cur, Target: to, Latest: node,
		Changes:      DiffVersions(cur, to),
		CurrentMajor: PostgresMajor(cur[config.SvcPostgres]), TargetMajor: PostgresMajor(to[config.SvcPostgres])}
	el.UpToDate = len(el.Changes) == 0
	el.PostgresRestart = cur[config.SvcPostgres] != to[config.SvcPostgres]
	el.DowntimeHours = downtimeAPIHours
	if el.PostgresRestart {
		el.DowntimeHours = downtimePostgresHours
	}
	block := func(typ, format string, args ...any) {
		el.Blockers = append(el.Blockers, UpgradeBlocker{Type: typ, Message: fmt.Sprintf(format, args...)})
	}
	switch {
	case p.Ref == config.SystemRef:
		block(BlockerSystem, "the system project belongs to the node and is upgraded with it, not on its own")
	case el.CurrentMajor != 0 && el.TargetMajor != 0 && el.CurrentMajor != el.TargetMajor:
		block(BlockerNoUpgradePath, "the project runs Postgres %d and the target is Postgres %d; upgrades across major versions are not supported yet", el.CurrentMajor, el.TargetMajor)
	}
	switch p.Status {
	case registry.StatusActiveHealthy:
	case registry.StatusInactive:
		block(BlockerHibernating, "the project is paused; resume it first")
	default:
		block(BlockerStatus, "the project is %s; an upgrade needs it ACTIVE_HEALTHY", p.Status)
	}
	if e.opts.Backup == nil {
		block(BlockerNoBackup, "the node has no backup service, and an upgrade is not started without a fresh backup")
	}
	el.Eligible = !el.UpToDate && len(el.Blockers) == 0
	for _, c := range el.Changes {
		switch c.Service {
		case config.SvcPostgres:
			el.Notes = append(el.Notes, "PostgreSQL restarts on the new release: every connection drops. The data directory is the same.")
		case config.SvcGoTrue:
			el.Notes = append(el.Notes, "GoTrue runs its database migrations when it starts. They only move forward: the base backup taken first is the way back for the data.")
		}
	}
	return el, nil
}

// BeginUpgrade implements ProjectUpgrader.
func (e *Engine) BeginUpgrade(ctx context.Context, ref string, req UpgradeRequest) (UpgradeRun, error) {
	if e.opts.Backup == nil {
		return nil, ErrNoBackupEngine
	}
	store := registry.Upgrades(e.reg)
	if store == nil {
		return nil, errors.New("lifecycle: the registry cannot record upgrades")
	}
	for svc := range req.Target {
		if !isProjectService(svc) {
			return nil, fmt.Errorf("%w: %q is not a service a project upgrades", ErrUpgradeUnsupported, svc)
		}
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
	if p.Ref == config.SystemRef {
		return nil, fmt.Errorf("%w: the system project belongs to the node", ErrUpgradeUnsupported)
	}
	if p.Status != registry.StatusActiveHealthy {
		return nil, invalidState(p, "upgrade")
	}
	el, err := e.plan(p, req.Target)
	if err != nil {
		return nil, err
	}
	if len(el.Blockers) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUpgradeUnsupported, el.Blockers[0].Message)
	}
	if el.UpToDate {
		return nil, ErrUpgradeNotNeeded
	}
	now := e.opts.Now().UTC()
	up := registry.Upgrade{TrackingID: newTrackingID(), Ref: ref, From: el.Current, To: el.Target, TargetVersion: req.TargetVersion,
		Status: registry.UpgradeRunning, Progress: ProgressRequested, InitiatedAt: now, LatestStatusAt: now}
	if err := store.PutUpgrade(ctx, &up); err != nil {
		return nil, fmt.Errorf("lifecycle: record the upgrade of %s: %w", ref, err)
	}
	if err := e.reg.SetProjectStatus(ctx, ref, registry.StatusUpgrading); err != nil {
		return nil, err
	}
	e.upgrading.Store(ref, struct{}{})
	e.event(ctx, ref, EventUpgradeStarted, map[string]any{"tracking_id": up.TrackingID, "from": up.From, "to": up.To, "target_version": req.TargetVersion})
	e.log.Info("upgrade_started", "ref", ref, "tracking_id", up.TrackingID, "changes", describeChanges(el.Changes))
	return &upgradeRun{e: e, store: store, up: up, prev: p.Status, restarts: el.PostgresRestart}, nil
}

// UpgradeProject upgrades ref to the target versions (nil: the node's pins) and returns when it
// is done: BeginUpgrade, then Run. The returned upgrade is the final state, also on failure.
func (e *Engine) UpgradeProject(ctx context.Context, ref string, target map[string]string) (*registry.Upgrade, error) {
	run, err := e.BeginUpgrade(ctx, ref, UpgradeRequest{Target: target})
	if err != nil {
		return nil, err
	}
	err = run.Run(ctx)
	u := run.Upgrade()
	return &u, err
}

func isProjectService(svc string) bool {
	for _, s := range config.ProjectServices {
		if s == svc {
			return true
		}
	}
	return false
}

func newTrackingID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func describeChanges(cs []ServiceChange) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s %s -> %s", c.Service, ShortVersion(c.Service, c.From), ShortVersion(c.Service, c.To)))
	}
	return strings.Join(parts, ", ")
}

type upgradeRun struct {
	e     *Engine
	store registry.UpgradeStore
	up    registry.Upgrade
	// prev is the status to go back to when nothing was touched.
	prev registry.Status
	// restarts: PostgreSQL's release changes, so the cluster restarts.
	restarts bool
}

func (r *upgradeRun) Upgrade() registry.Upgrade {
	u := r.up
	u.From, u.To = mergeVersions(nil, r.up.From), mergeVersions(nil, r.up.To)
	return u
}

// save records progress. A registry that cannot take it is logged and does not stop the upgrade:
// the status is for the dashboard, and the project's own row decides what runs.
func (r *upgradeRun) save(ctx context.Context, progress string) {
	r.up.Progress, r.up.LatestStatusAt = progress, r.e.opts.Now().UTC()
	if err := r.store.PutUpgrade(ctx, &r.up); err != nil {
		r.e.log.Warn("could not record the upgrade's progress", "ref", r.up.Ref, "progress", progress, "error", err)
	}
}

// Run implements UpgradeRun.
func (r *upgradeRun) Run(ctx context.Context) error {
	e, ref := r.e, r.up.Ref
	defer e.upgrading.Delete(ref)

	r.save(ctx, ProgressStarted)
	if err := e.ensureArtifacts(ctx, r.up.From, r.up.To); err != nil {
		return r.abort(ctx, UpgradeErrArtifacts, "fetching the target artifacts", err)
	}
	r.save(ctx, ProgressArtifactsReady)

	b, err := e.preUpgradeBackup(ctx, ref)
	if err != nil {
		return r.abort(ctx, UpgradeErrBackup, "the base backup before the upgrade", err)
	}
	r.up.BackupID = b.ID
	e.event(ctx, ref, EventUpgradeBackupDone, map[string]any{"tracking_id": r.up.TrackingID, "backup_id": b.ID, "location": b.Location})
	r.save(ctx, ProgressBackupDone)

	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return r.abort(ctx, UpgradeErrStart, "waiting for the project's lock", err)
	}
	defer unlock()
	return r.swap(ctx)
}

// ensureArtifacts makes the artifacts of every changed service available before anything is
// stopped: fetched when the store can, otherwise present.
func (e *Engine) ensureArtifacts(ctx context.Context, from, to map[string]string) error {
	for _, c := range DiffVersions(from, to) {
		switch a := e.arts.(type) {
		case TagFetcher:
			if _, err := a.FetchTag(ctx, c.Service, c.To); err != nil {
				return fmt.Errorf("%s %s: %w", c.Service, c.To, err)
			}
		case TagArtifacts:
			if _, err := a.DirFor(c.Service, c.To); err != nil {
				return fmt.Errorf("%s %s: %w", c.Service, c.To, err)
			}
		default:
			if pin, err := e.arts.Tag(c.Service); err != nil || pin != c.To {
				return fmt.Errorf("%s %s: the artifact store cannot find a release by tag", c.Service, c.To)
			}
			if _, err := e.arts.Dir(c.Service); err != nil {
				return fmt.Errorf("%s %s: %w", c.Service, c.To, err)
			}
		}
	}
	return nil
}

func (e *Engine) preUpgradeBackup(ctx context.Context, ref string) (*registry.Backup, error) {
	if ub, ok := e.opts.Backup.(UpgradeBackuper); ok {
		return ub.UpgradeBackup(ctx, ref)
	}
	return e.opts.Backup.BaseBackup(ctx, ref)
}

// abort ends an upgrade that has not touched a service: the project goes back to the status it
// had and keeps running its versions.
func (r *upgradeRun) abort(ctx context.Context, code, stage string, cause error) error {
	e, ref := r.e, r.up.Ref
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if unlock, err := e.lock(cctx, ref); err != nil {
		e.log.Error("upgrade: could not lock the project to settle its status", "ref", ref, "error", err)
	} else {
		if cur, err := e.reg.GetProject(cctx, ref); err == nil && cur.Status == registry.StatusUpgrading {
			if err := e.reg.SetProjectStatus(cctx, ref, r.prev); err != nil {
				e.log.Error("upgrade: could not restore the project's status", "ref", ref, "error", err)
			}
		}
		unlock()
	}
	r.failed(cctx, code, cause, "nothing was changed")
	return fmt.Errorf("lifecycle: upgrade of %s failed at %s before any service was touched; the project runs its previous versions: %w", ref, stage, cause)
}

// failed records the end of an upgrade that did not succeed.
func (r *upgradeRun) failed(ctx context.Context, code string, cause error, outcome string) {
	r.up.Status, r.up.Error, r.up.Detail = registry.UpgradeFailed, code, cause.Error()+" ("+outcome+")"
	r.save(ctx, r.up.Progress)
	r.e.event(ctx, r.up.Ref, EventUpgradeFailed, map[string]any{"tracking_id": r.up.TrackingID, "error_code": code, "error": cause.Error(),
		"outcome": outcome, "backup_id": r.up.BackupID, "progress": r.up.Progress})
	r.e.log.Error("upgrade_failed", "ref", r.up.Ref, "tracking_id", r.up.TrackingID, "stage", code, "backup_id", r.up.BackupID, "outcome", outcome, "error", cause)
}

// swap is steps 3 and 4: the project's lock is held.
func (r *upgradeRun) swap(ctx context.Context) error {
	e, ref := r.e, r.up.Ref
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		r.failed(ctx, UpgradeErrStart, err, "the project is gone")
		return fmt.Errorf("lifecycle: upgrade of %s: %w", ref, err)
	}
	if p.Status != registry.StatusUpgrading {
		// Someone else moved the project on (a delete by another process): leave it alone.
		err := fmt.Errorf("%w: %s is %s, not UPGRADING", ErrInvalidState, ref, p.Status)
		r.failed(ctx, UpgradeErrStart, err, "nothing was changed")
		return err
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		_ = e.reg.SetProjectStatus(cctx, ref, r.prev)
		r.failed(cctx, UpgradeErrStart, err, "nothing was changed")
		return fmt.Errorf("lifecycle: upgrade of %s: %w", ref, err)
	}
	target := *p
	target.Versions = mergeVersions(p.Versions, r.up.To)

	e.stopTimer(ctx, ref)
	r.save(ctx, ProgressStopping)
	code, serr := r.install(ctx, &target, keys)
	if serr == nil {
		r.save(ctx, ProgressHealth)
		if serr = healthError(e.plane.Health(ctx, &target, keys)); serr != nil {
			code = UpgradeErrHealth
		}
	}
	if serr == nil {
		if r.restarts {
			e.applySavedSettings(ctx, &target, keys)
		}
		r.save(ctx, ProgressCommit)
		p.Versions, p.Status = target.Versions, registry.StatusActiveHealthy
		if serr = e.reg.UpdateProject(ctx, p); serr != nil {
			code, serr = UpgradeErrHealth, fmt.Errorf("record the new versions: %w", serr)
		}
	}
	if serr != nil {
		return r.rollback(ctx, p, keys, code, serr)
	}
	e.startTimer(ctx, ref)
	r.up.Status, r.up.Error, r.up.Detail = registry.UpgradeDone, "", ""
	r.save(ctx, ProgressCompleted)
	e.event(ctx, ref, EventUpgradeSucceeded, map[string]any{"tracking_id": r.up.TrackingID, "from": r.up.From, "to": r.up.To,
		"backup_id": r.up.BackupID, "seconds": int(e.opts.Now().Sub(r.up.InitiatedAt).Seconds())})
	e.log.Info("upgrade_succeeded", "ref", ref, "tracking_id", r.up.TrackingID, "backup_id", r.up.BackupID)
	return nil
}

// install stops what has to stop and starts the units of target. It returns the error code of the
// stage that failed.
func (r *upgradeRun) install(ctx context.Context, target *registry.Project, keys *secrets.ProjectKeys) (string, error) {
	e, ref := r.e, r.up.Ref
	if !r.restarts {
		// Only GoTrue and PostgREST change: the cluster keeps serving, and Reconfigure renders
		// both units from the target versions and restarts each, waiting for it to answer.
		r.save(ctx, ProgressServices)
		if err := e.plane.Reconfigure(ctx, target, keys); err != nil {
			return UpgradeErrStart, err
		}
		return "", nil
	}
	e.quiesce(ctx, ref)
	if err := e.plane.Stop(ctx, ref); err != nil {
		return UpgradeErrStart, fmt.Errorf("stop: %w", err)
	}
	r.save(ctx, ProgressDatabase)
	if err := e.plane.StartDatabase(ctx, target, keys); err != nil {
		return UpgradeErrStart, err
	}
	r.save(ctx, ProgressServices)
	if err := e.plane.Start(ctx, target, keys); err != nil {
		return UpgradeErrStart, err
	}
	return "", nil
}

// rollback renders the previous versions again, starts them, checks them and records the
// outcome. p carries the row as read under the lock.
func (r *upgradeRun) rollback(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, code string, cause error) error {
	e, ref := r.e, r.up.Ref
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	// Explicit previous versions: a project that recorded none runs the node's pins, and the
	// pins may be the very versions that just failed.
	prev := *p
	prev.Versions = mergeVersions(p.Versions, r.up.From)
	var rerr error
	if r.restarts {
		if err := e.plane.Stop(cctx, ref); err != nil {
			e.log.Warn("upgrade: stop before the rollback", "ref", ref, "error", err)
		}
		if rerr = e.plane.StartDatabase(cctx, &prev, keys); rerr == nil {
			rerr = e.plane.Start(cctx, &prev, keys)
		}
	} else {
		rerr = e.plane.Reconfigure(cctx, &prev, keys)
	}
	if rerr == nil {
		rerr = healthError(e.plane.Health(cctx, &prev, keys))
	}
	status := registry.StatusActiveHealthy
	if rerr != nil {
		status = registry.StatusActiveUnhealthy
	}
	p.Versions, p.Status = prev.Versions, status
	if err := e.reg.UpdateProject(cctx, p); err != nil {
		e.log.Error("upgrade: could not record the previous versions after the rollback", "ref", ref, "error", err)
	}
	if rerr == nil {
		e.startTimer(cctx, ref)
		r.failed(cctx, code, cause, "rolled back to the previous versions")
		return fmt.Errorf("lifecycle: upgrade of %s failed and the previous versions are running again (the data was not restored; the pre-upgrade backup is %d): %w", ref, r.up.BackupID, cause)
	}
	r.failed(cctx, code, cause, "the rollback failed too: "+rerr.Error())
	return fmt.Errorf("lifecycle: upgrade of %s failed (%v) and the rollback to the previous versions failed too (%v); the project is ACTIVE_UNHEALTHY, and the pre-upgrade backup %d is the way back for the data (`supavise backups restore`)", ref, cause, rerr, r.up.BackupID)
}

func healthError(hs []ServiceHealth) error {
	var bad []string
	for _, h := range hs {
		if !h.Healthy {
			bad = append(bad, h.Name+": "+strings.TrimSpace(h.Status+" "+h.Error))
		}
	}
	if len(hs) == 0 {
		return errors.New("no health result")
	}
	if len(bad) > 0 {
		return fmt.Errorf("not healthy after the change: %s", strings.Join(bad, "; "))
	}
	return nil
}

// recoverUpgrade settles a project the daemon left UPGRADING: its units stop (they may run the
// target versions, which the registry never recorded) and its upgrade is marked failed. The
// caller sets the status; StartActive then starts the project on the versions the registry has.
func (e *Engine) recoverUpgrade(ctx context.Context, ref string) {
	if store := registry.Upgrades(e.reg); store != nil {
		if u, err := store.LatestUpgrade(ctx, ref); err == nil && u.Status == registry.UpgradeRunning {
			u.Status, u.Error, u.LatestStatusAt = registry.UpgradeFailed, UpgradeErrHealth, e.opts.Now().UTC()
			u.Detail = "the daemon stopped during the upgrade; the project starts on its previous versions"
			if err := store.PutUpgrade(ctx, u); err != nil {
				e.log.Warn("recover: could not mark the upgrade failed", "ref", ref, "error", err)
			}
			e.event(ctx, ref, EventUpgradeFailed, map[string]any{"tracking_id": u.TrackingID, "error_code": u.Error, "error": u.Detail,
				"outcome": "interrupted", "backup_id": u.BackupID, "progress": u.Progress})
			e.log.Error("upgrade_failed", "ref", ref, "tracking_id", u.TrackingID, "stage", u.Progress, "backup_id", u.BackupID, "outcome", "interrupted")
		}
	}
	if err := e.plane.Stop(ctx, ref); err != nil {
		e.log.Warn("recover: stopping units", "ref", ref, "error", err)
	}
}

// CollectArtifacts removes the artifacts that nothing references: not the node's pins, not those
// of the last keepReleases releases the node ran, and not any version a project runs or is being
// upgraded to. dryRun lists them only. The artifact store must be an *artifacts.Store.
func (e *Engine) CollectArtifacts(ctx context.Context, keepReleases int, dryRun bool) ([]artifacts.Unused, error) {
	st, ok := e.arts.(interface {
		KeepSet(int, []map[string]string) (map[string]bool, error)
		GC(map[string]bool, bool) ([]artifacts.Unused, error)
	})
	if !ok {
		return nil, errors.New("lifecycle: the artifact store cannot collect garbage")
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var used []map[string]string
	store := registry.Upgrades(e.reg)
	for i := range ps {
		v, err := e.EffectiveVersions(&ps[i])
		if err != nil {
			return nil, err
		}
		used = append(used, v)
		if store == nil {
			continue
		}
		if u, err := store.LatestUpgrade(ctx, ps[i].Ref); err == nil && u.Status == registry.UpgradeRunning {
			used = append(used, u.To)
		}
	}
	keep, err := st.KeepSet(keepReleases, used)
	if err != nil {
		return nil, err
	}
	return st.GC(keep, dryRun)
}
