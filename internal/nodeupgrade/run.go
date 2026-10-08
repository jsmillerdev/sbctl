package nodeupgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Exit statuses of `supavise upgrade` and `supavise rollback`.
const (
	ExitOK            = 0
	ExitRefused       = 2 // a pre-check said no; nothing changed
	ExitRolledBack    = 3 // the upgrade failed and the node is back on the previous release
	ExitNeedsOperator = 4 // the upgrade failed and the node needs the operator
)

// Failure is the error Run returns for every outcome that is not success. Code is the exit status.
type Failure struct {
	Code int
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// ExitCode returns the exit status for err: 0 for nil, the Failure's code, 1 otherwise.
func ExitCode(err error) int {
	var f *Failure
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &f):
		return f.Code
	}
	return 1
}

func refused(format string, a ...any) error {
	return &Failure{Code: ExitRefused, Err: fmt.Errorf(format, a...)}
}

// ErrAWSUnattended is why --aws is refused together with --unattended: the stack update shows a
// change set and waits for the operator to type apply.
var ErrAWSUnattended = errors.New("--aws changes the AWS stack with your credentials and asks you to confirm it: it cannot run with --unattended")

// Phases written to the upgrade marker (<state_dir>/system/upgrade.json), which `supavise status`
// and the dashboard banner read. The last four end the upgrade.
const (
	PhasePreparing   = "preparing"
	PhaseSwitching   = "switching"
	PhaseServices    = "services"
	PhaseProjects    = "projects"
	PhaseVerifying   = "verifying"
	PhaseRollingBack = "rolling_back"

	PhaseDone       = "done"
	PhaseRolledBack = "rolled_back"
	PhaseFailed     = "failed"
	PhaseRefused    = "refused"
)

// Finished reports whether phase ends an upgrade.
func Finished(phase string) bool {
	switch phase {
	case PhaseDone, PhaseRolledBack, PhaseFailed, PhaseRefused:
		return true
	}
	return false
}

// Candidate is a release found and verified (signature and manifest) but not downloaded.
type Candidate struct {
	Tag string
	// Data belongs to the Host.
	Data any
}

// Staged is a release binary that has been downloaded, verified and run for its Info.
type Staged struct {
	Info *Info
	// Data belongs to the Host.
	Data any
}

// Host is the machine under the upgrade. cmd/supavise implements it for a Linux node; the tests
// use a fake. Every method that changes the node is called by Run only after the checks that
// refuse the upgrade have passed.
type Host interface {
	// Inspect reads the node. It changes nothing.
	Inspect(ctx context.Context) (*Node, error)
	// Resolve finds the release (empty tag: the newest) and verifies its signature and manifest
	// against the version the node runs. It downloads no binary.
	Resolve(ctx context.Context, tag string, node *Node) (*Candidate, error)
	// Stage downloads the binary of the release, verifies it and reads its Info.
	Stage(ctx context.Context, c *Candidate) (*Staged, error)
	// Discard removes what Stage downloaded.
	Discard(s *Staged)

	// Prefetch downloads and unpacks every artifact the release pins that the node lacks.
	Prefetch(ctx context.Context, s *Staged, node *Node, p *Plan) error
	// Backup takes a base backup of each ref, up to parallel at a time, and fails if any fails.
	Backup(ctx context.Context, refs []string, parallel int) error

	// Mark writes the upgrade marker; the Host adds the versions, the start time and its process.
	Mark(phase, detail string)
	// Install keeps the release that runs and the new one, swaps the binary in, renders the
	// units with it, restarts the daemon and waits until it answers. swapped reports whether the
	// installed binary was replaced: when it is false, nothing changed.
	Install(ctx context.Context, s *Staged, prev, next Record) (swapped bool, err error)
	// WaitShared waits until each service runs its new release and answers, in order.
	WaitShared(ctx context.Context, moves []ServiceMove) error
	// UpgradeProjects runs `projects upgrade --all` on the new binary for the target releases and
	// returns every project that moved since `since`, also when it fails. The same rollout also
	// restarts the projects whose PostgreSQL, GoTrue or PostgREST files the new daemon rendered differently
	// and left running (the daemon holds those restarts back while an upgrade runs), in the same
	// canary and batch order.
	UpgradeProjects(ctx context.Context, target map[string]string, since time.Time) ([]ProjectMove, error)
	// Status returns the verdict of `supavise status` and its first line.
	Status(ctx context.Context) (verdict, summary string, err error)
	// EndUpgrade records in the kept record of version when the upgrade that installed it ended:
	// a later `supavise rollback` puts back only the projects this upgrade moved.
	EndUpgrade(ctx context.Context, version string, at time.Time) error
	// Cleanup removes the artifacts and the kept binaries beyond keep releases.
	Cleanup(ctx context.Context, keep int, current string)

	// AppliedMigrations reads the registry migrations applied.
	AppliedMigrations(ctx context.Context) ([]string, error)
	// PreviousRelease returns the kept release before current, and the record of current when
	// the node kept one (nil otherwise); prev is nil when there is nothing to go back to.
	PreviousRelease(ctx context.Context, current string) (prev, cur *Record, err error)
	// MovesBetween lists what the finished upgrades that began from since to until (zero: no
	// limit) did to each project, as one move per project: the releases it ran before the first of
	// them and the ones it runs after the last. Upgrades outside the window, such as a revert that a
	// rollback made later, are not in it.
	MovesBetween(ctx context.Context, since, until time.Time) ([]ProjectMove, error)
	// RevertProjects puts projects back on the releases they ran.
	RevertProjects(ctx context.Context, moves []ProjectMove) error
	// Restore reinstalls the kept release rec: its binary, its units, the daemon, and waits.
	// from is the release the node is rolled back from; it is not a target of a later rollback.
	Restore(ctx context.Context, from string, rec Record) error
	// Confirm asks the operator; it returns an error when there is nobody to ask.
	Confirm(question string) (bool, error)
}

// VersionInspector is the optional Host capability behind --check: the installed version and what
// the binary says about itself, without the registry, the projects or the status report, so that
// a check works for an account that cannot reach the registry.
type VersionInspector interface {
	InspectVersion(ctx context.Context) (*Node, error)
}

// Options of Run.
type Options struct {
	// Version is the release to upgrade to; empty means the newest.
	Version string
	// Check only looks for a release; Plan stops after printing what would happen.
	Check, Plan     bool
	Yes, Unattended bool
	IncludePostgres bool
	// AWS also brings the AWS stack to the release's template first (`supavise upgrade --aws`),
	// with StackName and StackSets as the operator's choices. It needs an operator at the
	// terminal, so it is refused with Unattended.
	AWS       bool
	StackName string
	StackSets []string
	// Canary, Batch and Keep are the [upgrade] settings.
	Canary, Batch, Keep int
	// BackupParallel bounds the base backups taken at once (default 3).
	BackupParallel int
	// VerifyTimeout is how long the node has to report a verdict no worse than before the
	// upgrade (default 5 minutes); VerifyEvery is the polling interval (default 10 seconds).
	VerifyTimeout, VerifyEvery time.Duration
	Out                        io.Writer
	Log                        *slog.Logger
	Now                        func() time.Time
	// Notify, when set, is told about the start and the end of an upgrade or rollback that changes the
	// node (Event). It runs on the command's goroutine and may take a while (it delivers alerts), so
	// the caller bounds it. A refusal, which changes nothing, is not an Event.
	Notify func(ctx context.Context, ev Event)
}

func (o *Options) say(format string, a ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", a...)
	}
}

func (o *Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Run checks, plans and applies an upgrade. It returns nil when the node was upgraded or had
// nothing to do, and a *Failure otherwise.
func Run(ctx context.Context, h Host, o Options) error {
	r := &run{h: h, o: o}
	return r.run(ctx)
}

type run struct {
	h       Host
	o       Options
	started time.Time
	node    *Node
	plan    *Plan
	// announced is set once EventStarted was sent.
	announced bool
	// sharedFailed is set when a shared service did not come up on the release this run moved it to.
	sharedFailed bool
}

func (r *run) run(ctx context.Context) error {
	o := &r.o
	if o.AWS && o.Unattended {
		return &Failure{Code: ExitRefused, Err: ErrAWSUnattended}
	}
	inspect := r.h.Inspect
	if vi, ok := r.h.(VersionInspector); ok && o.Check {
		inspect = vi.InspectVersion
	}
	node, err := inspect(ctx)
	if err != nil {
		return refused("cannot read the node: %v", err)
	}
	r.node = node
	// A check only reads; it has no quarrel with an upgrade that runs.
	if node.Running != nil && !o.Check {
		return refused("another upgrade is running (process %d, phase %s)", node.Running.PID, node.Running.Phase)
	}
	cand, err := r.h.Resolve(ctx, o.Version, node)
	if err != nil {
		return refused("%v", err)
	}
	newer := true
	if c, ok := compareTags(cand.Tag, node.Version); ok {
		newer = c > 0
		if c < 0 {
			return refused("%s is older than the installed %s; `supavise rollback` goes back to the previous release", cand.Tag, node.Version)
		}
	}
	if o.Check {
		state := "up to date"
		if newer {
			state = "update available: run `sudo supavise upgrade`"
		}
		o.say("installed %s, newest %s: %s", node.Version, cand.Tag, state)
		return nil
	}

	// The binary of the release tells what it pins. The installed release is its own answer when
	// it is the one asked for.
	var staged *Staged
	info := node.BinaryInfo
	if cand.Tag != node.Version || info == nil {
		if staged, err = r.h.Stage(ctx, cand); err != nil {
			return refused("%v", err)
		}
		defer r.h.Discard(staged)
		info = staged.Info
	}
	r.plan = BuildPlan(node, info, PlanOptions{IncludePostgres: o.IncludePostgres, Canary: o.Canary, Batch: o.Batch})
	r.plan.Render(o.Out)
	if r.plan.Refusal != "" {
		return refused("%s", r.plan.Refusal)
	}
	if r.plan.Empty() && !o.AWS {
		return nil
	}
	if !r.plan.NodeChanges() {
		return r.stackOnly(ctx, cand)
	}
	gates := CheckGates(node, r.plan, GateOptions{Unattended: o.Unattended, Now: o.now()})
	for _, w := range gates.Warnings {
		o.say("warning: %s", w)
	}
	if len(gates.Refusals) > 0 {
		return refused("%s", strings.Join(gates.Refusals, "; "))
	}
	if o.Plan {
		return nil
	}
	if !o.Yes && !o.Unattended {
		ok, err := r.h.Confirm("Upgrade now?")
		if err != nil {
			return refused("%v", err)
		}
		if !ok {
			return refused("nothing was changed")
		}
	}
	// The stack comes first: a refusal or a failure of it leaves the host as it was, and a later
	// failure of the host needs no change to the stack, whose changes only add what is not used yet.
	var stackCmd string
	if o.AWS {
		var err error
		if stackCmd, err = r.stackStep(ctx, cand); err != nil {
			return err
		}
	}
	if staged == nil {
		// Same release, services behind it: there is no binary to swap, but the artifacts and the
		// backups are prepared the same way.
		staged = &Staged{Info: info}
	}
	if err := r.apply(ctx, staged); err != nil {
		return err
	}
	if stackCmd != "" {
		o.say("The AWS stack was not updated. To update it, run:\n\n  %s", stackCmd)
	}
	r.notifyInfraBehind(ctx)
	return nil
}

// notifyInfraBehind raises infra_behind when a timer's run leaves the AWS stack behind the release
// it needs. A run with an operator at the terminal has printed the gap and the command, and a
// read-only run (--plan) tells nobody.
func (r *run) notifyInfraBehind(ctx context.Context) {
	o := &r.o
	if !o.Unattended || o.Plan || !r.plan.StackPending() {
		return
	}
	g := r.plan.Stack
	o.notify(ctx, Event{Kind: EventInfraBehind, From: r.node.Version, To: r.plan.To, Cause: fmt.Sprintf("the AWS stack is at revision %d and %s needs %d", g.Report.Have, r.plan.To, g.Need)})
}

// stackOnly ends a run whose plan changes nothing on the node and has an AWS stack to update (or,
// with --aws, a stack to look at). Without --aws it only says so: the plan has printed the gap and
// the command, and a timer that finds the stack behind raises infra_behind, never a failure.
func (r *run) stackOnly(ctx context.Context, cand *Candidate) error {
	o := &r.o
	if !o.AWS {
		r.notifyInfraBehind(ctx)
		return nil
	}
	if o.Plan {
		return nil
	}
	if !o.Yes {
		ok, err := r.h.Confirm("Update the AWS stack now?")
		if err != nil {
			return refused("%v", err)
		}
		if !ok {
			return refused("nothing was changed")
		}
	}
	cmd, err := r.stackStep(ctx, cand)
	if err != nil {
		return err
	}
	if cmd != "" {
		return refused("the stack was not updated: no AWS credentials of yours are in the environment")
	}
	return nil
}

// compareTags orders two release tags; ok is false when either is not a release version.
func compareTags(a, b string) (int, bool) { return compareVersion(a, b) }

func (r *run) apply(ctx context.Context, staged *Staged) error {
	o, h, node, plan := &r.o, r.h, r.node, r.plan
	r.started = o.now()
	log := o.log()
	log.Info("upgrade_started", "from", node.Version, "to", plan.To, "projects", len(plan.Upgrade), "shared_services", len(plan.Shared))

	r.mark(PhasePreparing, "downloading the new release")
	if err := h.Prefetch(ctx, staged, node, plan); err != nil {
		return r.endRefused(ctx, fmt.Errorf("fetching the artifacts of %s: %w; nothing was changed", plan.To, err))
	}
	refs := BackupRefs(node)
	r.mark(PhasePreparing, fmt.Sprintf("backing up %d project(s)", len(refs)))
	o.say("taking a base backup of %d project(s) (the projects keep running)", len(refs))
	par := o.BackupParallel
	if par <= 0 {
		par = 3
	}
	if err := h.Backup(ctx, refs, par); err != nil {
		return r.endRefused(ctx, fmt.Errorf("the base backups failed: %w; nothing was stopped or changed", err))
	}

	// Everything above changed nothing that runs; from here the node changes.
	o.notify(ctx, Event{Kind: EventStarted, From: node.Version, To: plan.To, Projects: len(plan.Upgrade) + len(plan.Pending)})
	r.announced = true

	// The release that runs is kept with the migrations it knows: what its binary says, or, for a
	// binary built before it could say, the ones the registry holds now (it has run on them). Its
	// InstalledAt stays what the node's store says, which is "unknown" for a release that was
	// installed before the first upgrade kept it.
	prev := Record{Version: node.Version, Platform: node.Platform, Pins: node.Pins, Migrations: node.AppliedMigrations, MigrationsFrom: MigrationsFromApplied}
	if node.BinaryInfo != nil && len(node.BinaryInfo.RegistryMigrations) > 0 {
		prev.Migrations, prev.MigrationsFrom = node.BinaryInfo.RegistryMigrations, MigrationsFromBinary
	}
	next := Record{Version: plan.To, Platform: staged.Info.Platform, Pins: staged.Info.Pins, Migrations: staged.Info.RegistryMigrations, MigrationsFrom: MigrationsFromBinary, InstalledAt: o.now().UTC(), UpgradeStartedAt: r.started.UTC()}

	if plan.BinaryChange {
		r.mark(PhaseSwitching, "installing "+plan.To+" and restarting the daemon")
		o.say("installing %s and restarting the daemon", plan.To)
		swapped, err := h.Install(ctx, staged, prev, next)
		if err != nil {
			if !swapped {
				return r.endRefused(ctx, fmt.Errorf("installing %s failed before the binary was replaced: %w; nothing was changed", plan.To, err))
			}
			return r.rollback(ctx, prev, nil, fmt.Errorf("the new release did not come up: %w", err))
		}
	}

	if plan.HostPending && !plan.BinaryChange {
		// A swap converges the host as part of installing the binary; without one, the installed
		// binary does it here.
		if hc, ok := h.(HostConverger); ok {
			r.mark(PhaseSwitching, "converging the host")
			o.say("converging the host (revision %d -> %d)", plan.HostFrom, plan.HostTo)
			if err := hc.Converge(ctx); err != nil {
				return r.rollback(ctx, prev, nil, fmt.Errorf("converging the host failed: %w", err))
			}
		}
	}

	moves := append(append([]ServiceMove{}, plan.System...), plan.Shared...)
	if len(moves) > 0 {
		r.mark(PhaseServices, "rolling the shared services")
		o.say("rolling %d service(s) onto the new release, one at a time", len(moves))
		if err := h.WaitShared(ctx, moves); err != nil {
			r.sharedFailed = true
			return r.rollback(ctx, prev, nil, fmt.Errorf("a shared service did not come up on its new release: %w", err))
		}
	}

	var projectMoves []ProjectMove
	// A new binary can render a project's PostgreSQL, GoTrue or PostgREST files differently without moving a
	// release; the daemon leaves those units running while the upgrade runs, and the rollout
	// restarts them, so the rollout runs after every change of binary.
	if plan.Rollout() {
		r.mark(PhaseProjects, fmt.Sprintf("upgrading %d project(s)", len(plan.Upgrade)))
		o.say("upgrading %d project(s): %d canary, then %d at a time", len(plan.Upgrade), plan.Canary, plan.Batch)
		if plan.BinaryChange {
			o.say("(and restarting the projects whose service files the new release renders differently, PostgreSQL included)")
		}
		if len(plan.Pending) > 0 {
			o.say("(and restarting %d project(s) whose restart an earlier upgrade held back)", len(plan.Pending))
		}
		var err error
		if projectMoves, err = h.UpgradeProjects(ctx, plan.ProjectTarget, r.started); err != nil {
			return r.rollback(ctx, prev, projectMoves, fmt.Errorf("the rollout of the projects stopped: %w", err))
		}
	}

	r.mark(PhaseVerifying, "checking the node")
	if err := waitVerdict(ctx, h, r.o, node.Verdict); err != nil {
		return r.rollback(ctx, prev, projectMoves, fmt.Errorf("the node is not healthy after the upgrade: %w", err))
	}
	r.endUpgrade(ctx)
	h.Cleanup(ctx, o.Keep, plan.To)
	r.mark(PhaseDone, "")
	log.Info("upgrade_succeeded", "from", node.Version, "to", plan.To, "projects", len(projectMoves))
	o.say("Supavise %s is running; %d project(s) upgraded", plan.To, len(projectMoves))
	o.notify(ctx, Event{Kind: EventSucceeded, From: node.Version, To: plan.To, Projects: len(projectMoves)})
	return nil
}

func (r *run) mark(phase, detail string) { r.h.Mark(phase, detail) }

// endUpgrade records the end of the upgrade in the kept record, trying a few times: without it a
// later rollback cannot tell which projects the upgrade moved from the ones an Owner upgraded
// afterwards, and leaves the projects alone.
func (r *run) endUpgrade(ctx context.Context) {
	every := min(r.o.VerifyEvery, 2*time.Second)
	if every <= 0 {
		every = 2 * time.Second
	}
	end := r.o.now()
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(every):
			}
		}
		if err = r.h.EndUpgrade(ctx, r.plan.To, end); err == nil {
			return
		}
	}
	r.o.log().Warn("could not record the end of the upgrade", "error", err.Error())
	r.o.say("warning: could not record the end of the upgrade (%v); `supavise rollback` will leave the projects it moved on their releases", err)
}

// endRefused ends an upgrade that changed nothing.
func (r *run) endRefused(ctx context.Context, err error) error {
	r.mark(PhaseRefused, err.Error())
	if r.announced {
		// The operator was told the node was about to change; say that it did not.
		r.o.notify(ctx, Event{Kind: EventRefused, From: r.node.Version, To: r.plan.To, Cause: err.Error()})
	}
	r.o.log().Warn("upgrade_refused", "to", r.plan.To, "error", err.Error())
	return &Failure{Code: ExitRefused, Err: err}
}

// verdictRank orders verdicts from best to worst; an unknown one counts as degraded.
func verdictRank(v string) int {
	switch v {
	case VerdictHealthy:
		return 0
	case VerdictDown:
		return 2
	}
	return 1
}

// waitVerdict polls `supavise status` until the node is no worse than it was before the upgrade, or
// the timeout passes. Right after restarts services report COMING_UP for a while, so one bad
// reading is not a verdict.
func waitVerdict(ctx context.Context, h Host, o Options, before string) error {
	timeout, every := o.VerifyTimeout, o.VerifyEvery
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if every <= 0 {
		every = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		v, summary, err := h.Status(ctx)
		switch {
		case err == nil && verdictRank(v) <= verdictRank(before):
			return nil
		case err != nil:
			v, summary = VerdictUnknown, err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("status is %q (%s), it was %q before", v, summary, before)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// rollback puts the node back on prev after a failed apply and returns the failure to report:
// exit status 3 when the node is back on the previous release and healthy, 4 when it is not.
func (r *run) rollback(ctx context.Context, prev Record, moves []ProjectMove, cause error) error {
	o := &r.o
	// An interrupt is why the upgrade failed, not a reason to leave the node half way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Minute)
	defer cancel()
	o.say("the upgrade failed: %v", cause)
	o.say("going back to %s", prev.Version)
	o.log().Error("upgrade_failed", "to", r.plan.To, "error", cause.Error())
	r.mark(PhaseRollingBack, cause.Error())
	if !r.plan.BinaryChange {
		return r.revertProjectsOnly(ctx, moves, cause)
	}
	if err := rollBackTo(ctx, r.h, r.o, rollbackArgs{From: r.plan.To, FromPins: r.plan.Target.Pins, To: &prev, Moves: moves, Verdict: r.node.Verdict, Mark: r.mark}); err != nil {
		r.mark(PhaseFailed, err.Error())
		o.log().Error("upgrade_needs_operator", "error", err.Error())
		if errors.Is(err, ErrRegistryNewer) {
			err = fmt.Errorf("%w. The projects the rollout had not reached may still run the PostgreSQL, GoTrue and PostgREST they ran before, on the settings of the old release; `sudo supavise upgrade` restarts them in canary order", err)
		}
		fail := &Failure{Code: ExitNeedsOperator, Err: fmt.Errorf("%w (the upgrade failed because: %v)", err, cause)}
		o.notify(ctx, Event{Kind: EventNeedsOperator, From: r.node.Version, To: r.plan.To, Halted: r.halted(), Cause: fail.Error()})
		return fail
	}
	r.mark(PhaseRolledBack, cause.Error())
	o.log().Warn("upgrade_rolled_back", "to", prev.Version, "error", cause.Error())
	o.say("rolled back: Supavise %s is running again", prev.Version)
	o.notify(ctx, Event{Kind: EventRolledBack, From: r.node.Version, To: r.plan.To, BackTo: prev.Version, Halted: r.halted(), Cause: cause.Error()})
	return &Failure{Code: ExitRolledBack, Err: fmt.Errorf("the upgrade to %s failed and the node is back on %s: %w", r.plan.To, prev.Version, cause)}
}

// revertProjectsOnly is the rollback of an upgrade that did not change the binary (the services
// and projects were brought onto the release the installed binary already is). There is no kept
// binary to go back to, and the shared services run the releases this binary pins, so the rollback
// puts back the projects this run moved and checks the node. A shared service that did not come up
// cannot be put back by it: that is exit 4, and the message says so. When the shared services came
// up and a project failed, the node is exit 3 with the services on the new releases.
func (r *run) revertProjectsOnly(ctx context.Context, moves []ProjectMove, cause error) error {
	o := &r.o
	fail := func(err error) error {
		r.mark(PhaseFailed, err.Error())
		o.log().Error("upgrade_needs_operator", "error", err.Error())
		f := &Failure{Code: ExitNeedsOperator, Err: fmt.Errorf("%w (the upgrade failed because: %v)", err, cause)}
		o.notify(ctx, Event{Kind: EventNeedsOperator, From: r.node.Version, To: r.plan.To, Halted: r.halted(), Cause: f.Error()})
		return f
	}
	var revertErr error
	if len(moves) > 0 {
		o.say("putting %d project(s) back on the releases they ran", len(moves))
		if revertErr = r.h.RevertProjects(ctx, moves); revertErr != nil {
			o.say("warning: not every project went back: %v", revertErr)
		}
	}
	switch {
	case r.sharedFailed:
		return fail(fmt.Errorf("the installed binary %s already is the release being applied and pins the services that failed, so they cannot be put back by a rollback: fix the service, or run `supavise rollback` to go back to the previous release", r.node.Version))
	case revertErr != nil:
		return fail(fmt.Errorf("not every project went back to its old releases: %v. `supavise projects versions` lists them", revertErr))
	}
	if err := waitVerdict(ctx, r.h, r.o, r.node.Verdict); err != nil {
		return fail(fmt.Errorf("the node is not healthy: %v", err))
	}
	r.mark(PhaseRolledBack, cause.Error())
	o.log().Warn("upgrade_rolled_back", "to", r.node.Version, "error", cause.Error())
	o.say("rolled back: the projects are on the releases they ran; the binary %s did not change", r.node.Version)
	o.notify(ctx, Event{Kind: EventRolledBack, From: r.node.Version, To: r.plan.To, BackTo: r.node.Version, Halted: r.halted(), Cause: cause.Error()})
	return &Failure{Code: ExitRolledBack, Err: fmt.Errorf("the upgrade to %s failed and the projects it moved are back on the releases they ran (the binary did not change, so the node's services stay on the releases it pins): %w", r.plan.To, cause)}
}
