package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
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
//     A new Postgres release is also checked against the extensions the project's databases have
//     installed (extensions.go): one that the release cannot serve refuses the upgrade here.
//  2. A fresh base backup (reason pre-upgrade) while the project still serves. It fails closed:
//     without a backup nothing is stopped.
//  3. Under the project's lock: the extension check again; the backup timer stops; with a new
//     Postgres the shared services let go of the database and all three units stop, otherwise
//     only GoTrue and PostgREST restart; the units are rendered from the target versions and
//     started; each answers a real request (GoTrue runs its schema migrations before it starts).
//     Then a health check of every service, and, after a Postgres change, a load check of the
//     code of every installed extension.
//  4. The registry records the target versions and ACTIVE_HEALTHY, in one write.
//
// Any failure in step 3 renders the previous versions again, starts them and checks them. The
// data directory is the same throughout and is not restored: a Postgres, GoTrue or PostgREST
// minor release does not change the format of the files, so the previous binaries read what the
// new ones left. GoTrue's and Storage's migrations are the exception in principle: they run when a
// service starts and only go forward, so a previous GoTrue may meet a schema that a newer one
// migrated. The base backup of step 2 is the way back for the data (`supavise backups restore`);
// the upgrade's status, events and error name its id. Recover (a daemon stopped in the middle)
// stops the project's units and lets StartActive start them on the recorded, previous versions;
// an upgrade that died before step 3 touched a unit only marks the upgrade failed.
//
// An upgrade never moves a service to an older release than the project runs: the node's pins go
// back after a rollback of the binary, and a project upgraded on the newer release stays on it
// (the eligibility says so) until the node pins newer ones. Tags that cannot be ordered are
// refused as well (CompareTags).
//
// Whoever runs an upgrade (the daemon for the API, the CLI for `supavise projects upgrade`)
// holds a session-level advisory lock on the project from BeginUpgrade to the end of Run, the
// base backup included, and the lock goes when its process does. A project that is UPGRADING with
// nobody holding the lock lost its runner: Recover (at the daemon's start) and SettleUpgrades
// (every few minutes) then stop its units, mark the upgrade failed and start it on its recorded
// versions. While the lock is held they leave the project alone.

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

// UpgradeNotice is what Options.UpgradeNotify receives: one of the three events of a project's
// upgrade, with what an alert about it says.
type UpgradeNotice struct {
	// Event is EventUpgradeStarted, EventUpgradeSucceeded or EventUpgradeFailed.
	Event      string
	Ref        string
	TrackingID string
	// Changes lists the service moves in words ("gotrue 2.195.0 -> 2.196.0"); empty when the
	// record has none.
	Changes string
	// BackupID is the base backup taken before the upgrade; 0 when none was.
	BackupID int64
	// Seconds is how long the upgrade took (EventUpgradeSucceeded).
	Seconds int
	// ErrorCode is the API's code of the stage that failed, Cause the error in words and Outcome
	// what became of the project (EventUpgradeFailed): "nothing was changed", "rolled back to the
	// previous versions", "the rollback failed too: ..." or "interrupted".
	ErrorCode, Cause, Outcome string
	// Settled is true when the event closes an upgrade whose process had stopped (the daemon found
	// the record running with nobody behind it).
	Settled bool
}

// Unresolved reports whether a failed upgrade left the project broken: the rollback failed too,
// so the project is ACTIVE_UNHEALTHY and a person has to look. Every other failure leaves the
// project on its previous versions (an interrupted one is started on them again by the daemon).
func (n UpgradeNotice) Unresolved() bool {
	return n.Event == EventUpgradeFailed && strings.HasPrefix(n.Outcome, "the rollback failed too")
}

func (e *Engine) notifyUpgrade(ctx context.Context, n UpgradeNotice) {
	if e.opts.UpgradeNotify != nil {
		e.opts.UpgradeNotify(ctx, n)
	}
}

var (
	// ErrUpgradeNotNeeded: the project already runs the target versions.
	ErrUpgradeNotNeeded = errors.New("lifecycle: the project already runs the target versions")
	// ErrUpgradeUnsupported: no upgrade path from the project's versions to the target (another
	// Postgres major version, an older release, tags that cannot be ordered) or a rule of the
	// node forbids it.
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
	BlockerExtension     = "unsupported_extension"
)

// UpgradeBlocker is one reason a project cannot be upgraded now.
type UpgradeBlocker struct {
	Type    string
	Message string
	// Extension names the extension of a BlockerExtension.
	Extension string
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
	// Ahead lists the services the project runs a newer release of than the target (From is what
	// it runs, To the older target). A project is never moved to an older release, so each is
	// also a blocker: the node's pins went back (a rollback of the binary, an older versions
	// file) and the project stays where it is until the node pins newer ones.
	Ahead []ServiceChange
	// Extensions are the installed extensions the target Postgres release cannot serve; each is
	// also a blocker.
	Extensions []ExtensionProblem
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
	// AllowOlder lets the target be older than what the project runs. It is for putting a
	// project back on the releases it ran before the upgrade of a node that is being rolled
	// back (`supavise rollback`); every other caller leaves it false, and a project is never
	// moved to an older release by accident.
	AllowOlder bool
	// ReuseBackupSince, when set, lets the upgrade take as its pre-upgrade backup a base backup of
	// the project that finished at or after this time, instead of taking another one. `supavise
	// upgrade` backs up every project first and passes the time it began: the base backup and the
	// archived WAL after it restore the project to any moment up to the upgrade, so a second
	// backup would only double the time the rollout takes.
	ReuseBackupSince time.Time
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
	return e.UpgradeEligibilityFor(ctx, ref, UpgradeRequest{})
}

// UpgradeEligibilityFor is UpgradeEligibility for the target a request names (nil Target: the
// node's pins). `supavise upgrade` plans with the services it moves.
func (e *Engine) UpgradeEligibilityFor(ctx context.Context, ref string, req UpgradeRequest) (*UpgradeEligibility, error) {
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return e.plan(ctx, p, req.Target, req.AllowOlder)
}

// plan computes the eligibility of p for target (nil: the node's pins). allowOlder lifts the
// refusal of a target older than what p runs.
func (e *Engine) plan(ctx context.Context, p *registry.Project, target map[string]string, allowOlder bool) (*UpgradeEligibility, error) {
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
	// Hosted never offers a downgrade, and an older GoTrue may meet a schema that a newer one
	// migrated forward. A pair of tags that cannot be ordered is refused the same way.
	for _, c := range el.Changes {
		cmp, err := CompareTags(c.Service, c.From, c.To)
		switch {
		case c.From == "":
		case err != nil:
			block(BlockerNoUpgradePath, "cannot tell whether %s %s -> %s is an upgrade: %v", c.Service, ShortVersion(c.Service, c.From), ShortVersion(c.Service, c.To), err)
		case cmp > 0 && allowOlder:
		case cmp > 0:
			el.Ahead = append(el.Ahead, c)
			block(BlockerNoUpgradePath, "the project runs %s %s, which is newer than %s; it stays on its versions until the node pins newer ones", c.Service, ShortVersion(c.Service, c.From), ShortVersion(c.Service, c.To))
		}
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
	if el.PostgresRestart && p.Status == registry.StatusActiveHealthy && len(el.Blockers) == 0 {
		// Best effort: a release that is not fetched yet, or a cluster that does not answer, is
		// a note here and a refusal when the upgrade runs.
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		probs, err := e.extensionProblems(cctx, p, to)
		cancel()
		if err != nil {
			el.Notes = append(el.Notes, "The installed extensions could not be checked against the new PostgreSQL release yet ("+err.Error()+"); they are checked again before anything stops.")
		}
		for _, pr := range probs {
			el.Extensions = append(el.Extensions, pr)
			el.Blockers = append(el.Blockers, UpgradeBlocker{Type: BlockerExtension, Extension: pr.Name, Message: "extension " + pr.String()})
		}
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
	if err := e.upgradeBusy(ref, "upgrade"); err != nil {
		return nil, err
	}
	for svc := range req.Target {
		if !isProjectService(svc) {
			return nil, fmt.Errorf("%w: %q is not a service a project upgrades", ErrUpgradeUnsupported, svc)
		}
	}
	// The runner's claim comes before the status: a project that is UPGRADING always has a
	// holder while its upgrade is alive.
	release, err := e.claimRunner(ctx, ref)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
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
	el, err := e.plan(ctx, p, req.Target, req.AllowOlder)
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
		up.Status, up.Error, up.Detail = registry.UpgradeFailed, UpgradeErrBackup, "could not set the project UPGRADING: "+err.Error()
		_ = store.PutUpgrade(context.WithoutCancel(ctx), &up)
		return nil, err
	}
	e.upgrading.Store(ref, struct{}{})
	e.event(ctx, ref, EventUpgradeStarted, map[string]any{"tracking_id": up.TrackingID, "from": up.From, "to": up.To, "target_version": req.TargetVersion})
	e.log.Info("upgrade_started", "ref", ref, "tracking_id", up.TrackingID, "changes", describeChanges(el.Changes))
	e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeStarted, Ref: ref, TrackingID: up.TrackingID, Changes: describeChanges(el.Changes)})
	started = true
	return &upgradeRun{e: e, store: store, up: up, prev: p.Status, restarts: el.PostgresRestart, release: release, reuseSince: req.ReuseBackupSince}, nil
}

// localRunners holds the upgrades of registries without advisory locks (the in-memory one, which
// every Engine in a test process shares), keyed by registry and ref.
var localRunners sync.Map

var errRunnerBusy = errors.New("another process is upgrading it")

func (e *Engine) localRunnerKey(ref string) string { return fmt.Sprintf("%p/%s", e.reg, ref) }

// runnerLockSQL is the advisory lock of the process that runs ref's upgrade. It is not the
// project's operation lock (lock): that one is taken only for the steps that change the project,
// and the base backup, which can take long, runs without it.
const runnerLockSQL = `pg_try_advisory_lock(hashtext('supavise:upgrade:' || $1::text))`

// claimRunner records that the caller runs ref's upgrade, for as long as it has not called the
// returned release (idempotent). With a Postgres registry it is a session-level advisory lock on
// a connection of its own, which Postgres drops when the process dies, however it dies.
func (e *Engine) claimRunner(ctx context.Context, ref string) (func(), error) {
	busy := fmt.Errorf("%w: cannot upgrade %s: %w", ErrInvalidState, ref, errRunnerBusy)
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil {
		key := e.localRunnerKey(ref)
		if _, taken := localRunners.LoadOrStore(key, struct{}{}); taken {
			return nil, busy
		}
		return sync.OnceFunc(func() { localRunners.Delete(key) }), nil
	}
	conn, err := pgx.ConnectConfig(ctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: claim the upgrade of %s: %w", ref, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "select "+runnerLockSQL, ref).Scan(&got); err != nil || !got {
		_ = conn.Close(context.WithoutCancel(ctx))
		if err != nil {
			return nil, fmt.Errorf("lifecycle: claim the upgrade of %s: %w", ref, err)
		}
		return nil, busy
	}
	return sync.OnceFunc(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
	}), nil
}

// runnerLive reports whether a process holds the upgrade of ref (this one included). When it
// cannot tell it says yes: a project is left UPGRADING rather than stopped under a live backup.
func (e *Engine) runnerLive(ctx context.Context, ref string) bool {
	if _, busy := e.upgrading.Load(ref); busy {
		return true
	}
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil {
		_, live := localRunners.Load(e.localRunnerKey(ref))
		return live
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		return true
	}
	defer conn.Close(context.WithoutCancel(ctx)) // drops the lock if the probe got it
	var got bool
	if err := conn.QueryRow(cctx, "select "+runnerLockSQL, ref).Scan(&got); err != nil {
		return true
	}
	return !got
}

// UpgradeProject upgrades ref to the target versions (nil: the node's pins) and returns when it
// is done: BeginUpgrade, then Run. The returned upgrade is the final state, also on failure.
func (e *Engine) UpgradeProject(ctx context.Context, ref string, target map[string]string) (*registry.Upgrade, error) {
	return e.UpgradeProjectWith(ctx, ref, UpgradeRequest{Target: target})
}

// UpgradeProjectWith is UpgradeProject for a whole request.
func (e *Engine) UpgradeProjectWith(ctx context.Context, ref string, req UpgradeRequest) (*registry.Upgrade, error) {
	run, err := e.BeginUpgrade(ctx, ref, req)
	if err != nil {
		return nil, err
	}
	err = run.Run(ctx)
	u := run.Upgrade()
	return &u, err
}

// upgradeBusy refuses an operation on ref while an upgrade runs in this process. The upgrade
// holds the project's lock through its disruptive step, and an operation that waited for the
// lock would then run on a project it was never asked about in this state; refusing at once
// is what the status UPGRADING promises. (Another process sees the status after it has the lock.)
func (e *Engine) upgradeBusy(ref, op string) error {
	if _, busy := e.upgrading.Load(ref); busy {
		return fmt.Errorf("%w: cannot %s %s while it is being upgraded", ErrInvalidState, op, ref)
	}
	return nil
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
	// release gives up the claim on the project's upgrade (claimRunner).
	release func()
	// reuseSince is UpgradeRequest.ReuseBackupSince.
	reuseSince time.Time
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
	defer r.release() // last: the status is settled before anyone may take the project over
	defer e.upgrading.Delete(ref)

	r.save(ctx, ProgressStarted)
	if err := e.ensureArtifacts(ctx, r.up.From, r.up.To); err != nil {
		return r.abort(ctx, UpgradeErrArtifacts, "fetching the target artifacts", err)
	}
	// The release is on disk now, so the extensions can be checked against it before the
	// backup spends its time.
	if err := r.checkExtensions(ctx); err != nil {
		return r.abort(ctx, UpgradeErrArtifacts, "checking the installed extensions", err)
	}
	r.save(ctx, ProgressArtifactsReady)

	b, err := r.backup(ctx)
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

// checkExtensions refuses the upgrade when an extension installed in one of the project's
// databases cannot run on the target Postgres release. It runs while the project still serves
// and nothing has been touched. It fails closed: a cluster it cannot ask is a refusal.
func (r *upgradeRun) checkExtensions(ctx context.Context) error {
	if !r.restarts {
		return nil
	}
	p, err := r.e.reg.GetProject(ctx, r.up.Ref)
	if err != nil {
		return err
	}
	return r.extensionsOK(ctx, p)
}

func (r *upgradeRun) extensionsOK(ctx context.Context, p *registry.Project) error {
	if !r.restarts {
		return nil
	}
	probs, err := r.e.extensionProblems(ctx, p, r.up.To)
	if err != nil {
		return fmt.Errorf("check the installed extensions against the new PostgreSQL release: %w", err)
	}
	if len(probs) > 0 {
		return fmt.Errorf("%w: the new PostgreSQL release cannot serve the installed extensions: %s", ErrUpgradeUnsupported, describeProblems(probs))
	}
	return nil
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

// backup returns the base backup the upgrade can be undone from: a recent one the request allows
// reusing, else a fresh one.
func (r *upgradeRun) backup(ctx context.Context) (*registry.Backup, error) {
	if !r.reuseSince.IsZero() {
		if bs, err := r.e.reg.ListBackups(ctx, r.up.Ref); err == nil {
			for i := range bs { // newest first
				if b := &bs[i]; b.Status == registry.BackupCompleted && b.FinishedAt != nil && !b.FinishedAt.Before(r.reuseSince) {
					r.e.log.Info("upgrade: using the base backup taken for this run", "ref", r.up.Ref, "backup_id", b.ID)
					return b, nil
				}
			}
		}
	}
	return r.e.preUpgradeBackup(ctx, r.up.Ref)
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
	r.e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeFailed, Ref: r.up.Ref, TrackingID: r.up.TrackingID, Changes: describeChanges(DiffVersions(r.up.From, r.up.To)),
		BackupID: r.up.BackupID, ErrorCode: code, Cause: cause.Error(), Outcome: outcome})
}

// refuse ends an upgrade that has not touched a service, with the project's lock held: the
// project goes back to the status it had. (abort does the same without the lock.)
func (r *upgradeRun) refuse(ctx context.Context, code string, cause error) error {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := r.e.reg.SetProjectStatus(cctx, r.up.Ref, r.prev); err != nil {
		r.e.log.Error("upgrade: could not restore the project's status", "ref", r.up.Ref, "error", err)
	}
	r.failed(cctx, code, cause, "nothing was changed")
	return fmt.Errorf("lifecycle: upgrade of %s failed before any service was touched; the project runs its previous versions: %w", r.up.Ref, cause)
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
		return r.refuse(ctx, UpgradeErrStart, err)
	}
	// The databases may have changed since the check before the backup.
	if err := r.extensionsOK(ctx, p); err != nil {
		return r.refuse(ctx, UpgradeErrArtifacts, err)
	}
	target := *p
	target.Versions = mergeVersions(p.Versions, r.up.To)

	// The progress comes first: from here on a recovery treats the units as touched.
	r.save(ctx, ProgressStopping)
	e.stopTimer(ctx, ref)
	code, serr := r.install(ctx, &target, keys)
	if serr == nil {
		r.save(ctx, ProgressHealth)
		if serr = healthError(e.plane.Health(ctx, &target, keys)); serr != nil {
			code = UpgradeErrHealth
		}
	}
	if serr == nil && r.restarts {
		// The cluster answers; the code of the extensions in its databases must load too.
		if insp, ok := e.plane.(ExtensionInspector); ok {
			if serr = insp.VerifyExtensions(ctx, &target); serr != nil {
				code = UpgradeErrHealth
			}
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
	e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeSucceeded, Ref: ref, TrackingID: r.up.TrackingID, Changes: describeChanges(DiffVersions(r.up.From, r.up.To)),
		BackupID: r.up.BackupID, Seconds: int(e.opts.Now().Sub(r.up.InitiatedAt).Seconds())})
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
	hs := e.plane.Health(cctx, &prev, keys)
	if rerr == nil {
		rerr = healthError(hs)
	}
	status := registry.StatusActiveHealthy
	if rerr != nil {
		status = registry.StatusActiveUnhealthy
	}
	p.Versions, p.Status = prev.Versions, status
	if err := e.reg.UpdateProject(cctx, p); err != nil {
		e.log.Error("upgrade: could not record the previous versions after the rollback", "ref", ref, "error", err)
	}
	// The nightly backup needs only the cluster: restart its timer whenever PostgreSQL runs, even
	// if an API unit does not.
	timerOn := rerr == nil || postgresHealthy(hs)
	if timerOn {
		e.startTimer(cctx, ref)
	}
	if rerr == nil {
		r.failed(cctx, code, cause, "rolled back to the previous versions")
		return fmt.Errorf("lifecycle: upgrade of %s failed and the previous versions are running again (the data was not restored; the pre-upgrade backup is %d): %w", ref, r.up.BackupID, cause)
	}
	outcome := "the rollback failed too: " + rerr.Error()
	if !timerOn {
		outcome += "; the project's scheduled backups stay paused until it is restarted"
		e.log.Warn("upgrade: the cluster is not running after the rollback, so scheduled backups stay paused", "ref", ref)
	}
	r.failed(cctx, code, cause, outcome)
	return fmt.Errorf("lifecycle: upgrade of %s failed (%v) and the rollback to the previous versions failed too (%v); the project is ACTIVE_UNHEALTHY, and the pre-upgrade backup %d is the way back for the data (`supavise backups restore`)", ref, cause, rerr, r.up.BackupID)
}

func postgresHealthy(hs []ServiceHealth) bool {
	for _, h := range hs {
		if h.Name == config.SvcPostgres && h.Healthy {
			return true
		}
	}
	return false
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

// settleUpgrading settles a project that is UPGRADING with no process running its upgrade, and
// reports false when the project had moved on by the time the lock was held. Its upgrade is marked
// failed. If the upgrade died before it touched a unit (during the artifact fetch or the base
// backup), the units still run the recorded versions and the project goes back to ACTIVE_HEALTHY
// without a stop. Otherwise its units stop (they may run the target versions, which the registry
// never recorded) and the project becomes ACTIVE_UNHEALTHY, which is what StartActive starts on the
// versions the registry has.
func (e *Engine) settleUpgrading(ctx context.Context, ref string) (Recovered, bool) {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		e.log.Warn("recover: lock", "ref", ref, "error", err)
		return Recovered{}, false
	}
	defer unlock()
	cur, err := e.reg.GetProject(ctx, ref)
	if err != nil || cur.Status != registry.StatusUpgrading {
		return Recovered{}, false
	}
	to, note := registry.StatusActiveUnhealthy, "the process that ran the upgrade stopped; the project starts on its previous versions"
	if !e.recoverUpgrade(ctx, ref) {
		to, note = registry.StatusActiveHealthy, "the process that ran the upgrade stopped before it touched any service; the project kept running its versions"
	}
	if err := e.reg.SetProjectStatus(ctx, ref, to); err != nil {
		e.log.Error("recover: set status", "ref", ref, "error", err)
		return Recovered{}, false
	}
	e.event(ctx, ref, "project.recovered", map[string]string{"from": string(registry.StatusUpgrading), "to": string(to), "note": note})
	e.log.Warn("recovered project after an interrupted operation", "ref", ref, "from", registry.StatusUpgrading, "to", to, "note", note)
	return Recovered{Ref: ref, From: registry.StatusUpgrading, To: to, Note: note}, true
}

// SettleUpgrades is Recover's UPGRADING step for a daemon that keeps running: an upgrade run by
// the CLI whose process died (a dropped SSH session that took it along, a kill, the OOM killer)
// leaves its project UPGRADING with nobody to finish it, and Studio would show the upgrade screen
// for good. Each such project is stopped, marked ACTIVE_UNHEALTHY and started again on its
// recorded versions (one whose upgrade died before touching a unit just goes back to
// ACTIVE_HEALTHY); an upgrade whose process still holds its claim is not touched. The daemon
// calls it every few minutes.
func (e *Engine) SettleUpgrades(ctx context.Context) []Recovered {
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		e.log.Error("settle upgrades: listing projects", "error", err)
		return nil
	}
	var out []Recovered
	for i := range ps {
		p := ps[i]
		if p.Ref == config.SystemRef || p.Status != registry.StatusUpgrading || e.runnerLive(ctx, p.Ref) {
			continue
		}
		r, ok := e.settleUpgrading(ctx, p.Ref)
		if !ok {
			continue
		}
		out = append(out, r)
		if r.To == registry.StatusActiveHealthy {
			continue // its units were never stopped
		}
		cur, err := e.reg.GetProject(ctx, p.Ref)
		if err != nil {
			continue
		}
		if err := e.startOne(ctx, cur); err != nil {
			e.log.Error("settle upgrades: the project did not start on its previous versions", "ref", p.Ref, "error", err)
		}
	}
	e.settleUpgradeRows(ctx, ps)
	return out
}

// settleUpgradeRows ends the upgrade record of a project that is not UPGRADING while the record
// still says running: its process stopped between recording the new versions and writing the
// final progress (or after a rollback), and nothing else would close it. Studio would poll
// the status for good, and artifact collection would keep the record's versions. The upgrade
// is done when the project runs its target versions, and failed otherwise.
func (e *Engine) settleUpgradeRows(ctx context.Context, ps []registry.Project) {
	store := registry.Upgrades(e.reg)
	if store == nil {
		return
	}
	for i := range ps {
		p := &ps[i]
		if p.Ref == config.SystemRef || p.Status == registry.StatusUpgrading {
			continue
		}
		u, err := store.LatestUpgrade(ctx, p.Ref)
		if err != nil || u.Status != registry.UpgradeRunning || e.runnerLive(ctx, p.Ref) {
			continue
		}
		now := e.opts.Now().UTC()
		eff, err := e.EffectiveVersions(p)
		if err == nil && len(DiffVersions(eff, u.To)) == 0 {
			u.Status, u.Progress, u.Error, u.LatestStatusAt = registry.UpgradeDone, ProgressCompleted, "", now
			u.Detail = ""
			if err := store.PutUpgrade(ctx, u); err != nil {
				e.log.Warn("settle upgrades: could not close the upgrade record", "ref", p.Ref, "error", err)
				continue
			}
			e.event(ctx, p.Ref, EventUpgradeSucceeded, map[string]any{"tracking_id": u.TrackingID, "from": u.From, "to": u.To,
				"backup_id": u.BackupID, "outcome": "settled: the project runs the target versions"})
			e.log.Warn("upgrade_succeeded", "ref", p.Ref, "tracking_id", u.TrackingID, "backup_id", u.BackupID, "outcome", "settled")
			e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeSucceeded, Ref: p.Ref, TrackingID: u.TrackingID, Changes: describeChanges(DiffVersions(u.From, u.To)),
				BackupID: u.BackupID, Settled: true})
			continue
		}
		u.Status, u.Error, u.LatestStatusAt = registry.UpgradeFailed, UpgradeErrHealth, now
		u.Detail = "the process that ran the upgrade stopped before it finished; the project runs its previous versions"
		if err := store.PutUpgrade(ctx, u); err != nil {
			e.log.Warn("settle upgrades: could not close the upgrade record", "ref", p.Ref, "error", err)
			continue
		}
		e.event(ctx, p.Ref, EventUpgradeFailed, map[string]any{"tracking_id": u.TrackingID, "error_code": u.Error, "error": u.Detail,
			"outcome": "interrupted", "backup_id": u.BackupID, "progress": u.Progress})
		e.log.Error("upgrade_failed", "ref", p.Ref, "tracking_id", u.TrackingID, "stage", u.Progress, "backup_id", u.BackupID, "outcome", "interrupted")
		e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeFailed, Ref: p.Ref, TrackingID: u.TrackingID, Changes: describeChanges(DiffVersions(u.From, u.To)),
			BackupID: u.BackupID, ErrorCode: u.Error, Cause: u.Detail, Outcome: "interrupted", Settled: true})
	}
}

// recoverUpgrade ends the running upgrade record of ref as failed. It stops the project's units
// and reports true, unless the upgrade died before it touched any (progress before
// ProgressStopping): then the units still run the recorded versions, nothing is stopped, and it
// reports false. A missing or already closed record counts as touched.
func (e *Engine) recoverUpgrade(ctx context.Context, ref string) (touched bool) {
	touched = true
	if store := registry.Upgrades(e.reg); store != nil {
		if u, err := store.LatestUpgrade(ctx, ref); err == nil && u.Status == registry.UpgradeRunning {
			u.Status, u.Error, u.LatestStatusAt = registry.UpgradeFailed, UpgradeErrHealth, e.opts.Now().UTC()
			u.Detail = "the process that ran the upgrade stopped; the project starts on its previous versions"
			if u.Progress < ProgressStopping {
				touched = false
				u.Error = UpgradeErrArtifacts
				switch u.Progress {
				case ProgressArtifactsReady:
					u.Error = UpgradeErrBackup
				case ProgressBackupDone:
					u.Error = UpgradeErrStart
				}
				u.Detail = "the process that ran the upgrade stopped before any service was touched; the project kept running its versions"
			}
			if err := store.PutUpgrade(ctx, u); err != nil {
				e.log.Warn("recover: could not mark the upgrade failed", "ref", ref, "error", err)
			}
			e.event(ctx, ref, EventUpgradeFailed, map[string]any{"tracking_id": u.TrackingID, "error_code": u.Error, "error": u.Detail,
				"outcome": "interrupted", "backup_id": u.BackupID, "progress": u.Progress})
			e.log.Error("upgrade_failed", "ref", ref, "tracking_id", u.TrackingID, "stage", u.Progress, "backup_id", u.BackupID, "outcome", "interrupted")
			e.notifyUpgrade(ctx, UpgradeNotice{Event: EventUpgradeFailed, Ref: ref, TrackingID: u.TrackingID, Changes: describeChanges(DiffVersions(u.From, u.To)),
				BackupID: u.BackupID, ErrorCode: u.Error, Cause: u.Detail, Outcome: "interrupted", Settled: true})
		}
	}
	if !touched {
		return false
	}
	if err := e.plane.Stop(ctx, ref); err != nil {
		e.log.Warn("recover: stopping units", "ref", ref, "error", err)
	}
	return true
}

// CollectArtifacts removes the artifacts that nothing references: not the node's pins, not those
// of the last keepReleases releases the node ran, and not any version a project runs or is being
// upgraded to. keep names more versions to keep, by service (the pins of the binaries `supavise
// rollback` can go back to: a daemon that did not record its pins leaves no history). dryRun lists
// them only. The artifact store must be an *artifacts.Store.
func (e *Engine) CollectArtifacts(ctx context.Context, keepReleases int, dryRun bool, keep ...map[string]string) ([]artifacts.Unused, error) {
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
	keepSet, err := st.KeepSet(keepReleases, append(used, keep...))
	if err != nil {
		return nil, err
	}
	return st.GC(keepSet, dryRun)
}

// PendingRestart reports whether the PostgreSQL, GoTrue or PostgREST of ref runs older files than
// the ones the node renders for it now: a release (or a setting) changed them while the daemon,
// which held its restarts back for an upgrade's rollout (DeferRestarts), left the unit running. A
// plane that cannot tell answers no.
func (e *Engine) PendingRestart(ctx context.Context, ref string) (bool, error) {
	pr, ok := e.plane.(PendingRestarter)
	if !ok {
		return false, nil
	}
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil || !active(p.Status) {
		return false, err
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return false, err
	}
	return pr.PendingRestart(ctx, p, keys)
}

// RestartPending restarts the PostgreSQL, GoTrue and PostgREST of ref that run older files than
// the ones rendered for them, and waits until they answer. It reports whether it restarted any.
// A restarted database takes the project's GoTrue and PostgREST down with it, and they start
// again on the rendered files. A database on its current files is not touched.
func (e *Engine) RestartPending(ctx context.Context, ref string) (bool, error) {
	pr, ok := e.plane.(PendingRestarter)
	if !ok {
		return false, nil
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return false, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return false, err
	}
	if !active(p.Status) {
		return false, nil
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return false, err
	}
	return pr.RestartPending(ctx, p, keys)
}
