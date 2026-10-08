package failover

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

// A project move (design 2.10.3). The leader runs it, holding the project's lock; the project
// shows RESTARTING while it is in flight, and every step is a row in the move's log, so
// --resume continues after a crash. The step names, in the order they run:
//
//	switchover   begin, quiesce, stop-old, promote, homed, start-new, tenant, demote-old, base-backup
//	failover     begin, fence-old, promote, homed, start-new, tenant, reseed-old, base-backup

// abortError ends a switchover that undid itself: the replica never caught up, nothing was
// promoted, and the old primary was started again.
type abortError struct{ cause error }

func (e *abortError) Error() string { return e.cause.Error() }
func (e *abortError) Unwrap() error { return e.cause }

// FailoverProject switches a project over to the node that holds its replica, or fails it over
// when its primary is not healthy.
func (o *Orchestrator) FailoverProject(ctx context.Context, opts ProjectOptions) (*registry.Move, error) {
	release, err := o.acquire()
	if err != nil {
		return nil, err
	}
	defer release()

	// The project's lock first, and the plan under it: a pause or a resume cannot change the status
	// the move records, and a move that cannot get the lock leaves no row behind.
	hold := &lockHold{o: o, ref: opts.Ref}
	if !opts.DryRun {
		unlock, err := o.lockProject(ctx, opts.Ref)
		if err != nil {
			return nil, err
		}
		hold.unlock = unlock
		defer hold.release()
	}
	pl, run, err := o.planProject(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		return nil, nil
	}
	if refused := pl.Refused(opts.Force || opts.Resume); len(refused) > 0 {
		return nil, &RefusedError{Checks: refused, Force: opts.Force}
	}
	if opts.ExpectKind != "" && !opts.Resume && pl.Kind != opts.ExpectKind {
		return nil, fmt.Errorf("%w: it was a %s and is a %s now", ErrPlanChanged, opts.ExpectKind, pl.Kind)
	}

	var j *journal
	var begin projectBegin
	if opts.Resume {
		if j, run, begin, err = o.resumeProject(ctx, run); err != nil {
			return nil, err
		}
	} else {
		begin = projectBegin{Replica: run.replica.Identifier, Origin: run.replica.Origin, Status: run.project.Status}
		mv := registry.Move{
			Scope: registry.MoveProject, Ref: run.project.Ref, FromNode: run.from.ID, ToNode: run.to.ID, Epoch: run.epoch,
			Kind: registry.MoveSwitchover,
		}
		if !run.planned {
			mv.Kind = registry.MoveFailover
		}
		if j, err = o.newProjectJournal(ctx, mv); err != nil {
			return nil, err
		}
		if err := j.record(ctx, "begin", begin.String()); err != nil {
			return nil, err
		}
	}

	o.announce(ctx, alerts.KindFailoverStarted, alerts.SeverityInfo, j.snapshot(),
		fmt.Sprintf("Moving project %s from %s to %s.", run.project.Ref, run.from.Name, run.to.Name))
	err = o.projectSteps(ctx, j, run, begin, timeoutSeconds(o.conf().StopTimeout()), hold)
	return o.endMove(ctx, j, run.project.Ref, err)
}

// projectBegin is what the first step records and a resume needs: the replica to promote (its
// row is gone once the project is homed on its node) and the status to give the project back.
type projectBegin struct {
	Replica string
	Origin  string
	Status  registry.Status
}

func (b projectBegin) String() string {
	return fmt.Sprintf("replica=%s origin=%s status=%s", b.Replica, b.Origin, b.Status)
}

func parseProjectBegin(detail string) projectBegin {
	var b projectBegin
	for _, f := range strings.Fields(detail) {
		switch k, v, _ := strings.Cut(f, "="); k {
		case "replica":
			b.Replica = v
		case "origin":
			b.Origin = v
		case "status":
			b.Status = registry.Status(v)
		}
	}
	return b
}

// resumeProject loads the unfinished move of the project.
func (o *Orchestrator) resumeProject(ctx context.Context, run *projectRun) (*journal, *projectRun, projectBegin, error) {
	st := o.store()
	mv, err := unfinishedProjectMove(ctx, st, run.project.Ref)
	if err != nil {
		return nil, nil, projectBegin{}, err
	}
	if mv == nil {
		return nil, nil, projectBegin{}, ErrNothingToResume
	}
	from, err := o.node(ctx, mv.FromNode)
	if err != nil {
		return nil, nil, projectBegin{}, err
	}
	to, err := o.node(ctx, mv.ToNode)
	if err != nil {
		return nil, nil, projectBegin{}, err
	}
	j := o.journalFor(*mv)
	run.from, run.to, run.epoch, run.planned = from, to, mv.Epoch, mv.Kind == registry.MoveSwitchover
	return j, run, parseProjectBegin(j.detail("begin")), nil
}

func timeoutSeconds(d time.Duration) int { return int(d / time.Second) }

// endMove records the outcome of a move in its log and as an alert, and returns the move.
func (o *Orchestrator) endMove(ctx context.Context, j *journal, ref string, runErr error) (*registry.Move, error) {
	if runErr != nil && interrupted(ctx, j, runErr) {
		// The daemon is stopping for the role the promotion gave its node. The move stays running in
		// its log, no alert says it failed, and the daemon that starts continues it.
		o.d.Log.Info("the daemon stops in the middle of a server move; the daemon that starts continues it", "cause", runErr)
		mv := j.snapshot()
		return &mv, fmt.Errorf("%w (%w)", ErrRestarting, runErr)
	}
	var abort *abortError
	state, text := registry.MoveDone, ""
	switch {
	case runErr == nil:
	case errors.As(runErr, &abort):
		state, text = registry.MoveAborted, runErr.Error()
	default:
		state, text = registry.MoveFailed, runErr.Error()
	}
	if err := j.finish(ctx, state, text); err != nil {
		o.d.Log.Error("could not record the end of the move", "error", err)
	}
	mv := j.snapshot()
	mv.State, mv.Error = state, text
	switch state {
	case registry.MoveDone:
		o.announce(ctx, alerts.KindFailoverCompleted, alerts.SeverityInfo, mv, doneDetail(mv))
	case registry.MoveAborted:
		o.announce(ctx, alerts.KindFailoverFailed, alerts.SeverityWarning, mv, failedDetail(mv, text))
	default:
		o.announce(ctx, alerts.KindFailoverFailed, alerts.SeverityCritical, mv, failedDetail(mv, text))
		// A project the move left in RESTARTING is not being restarted by anything any more.
		if ref != "" {
			if p, err := o.store().GetProject(ctx, ref); err == nil && p.Status == registry.StatusRestarting {
				_ = o.store().SetProjectStatus(ctx, ref, registry.StatusActiveUnhealthy)
			}
		}
	}
	if runErr != nil {
		return &mv, runErr
	}
	return &mv, nil
}

func doneDetail(mv registry.Move) string {
	var warnings []string
	for _, s := range mv.Steps {
		if strings.HasPrefix(s.Detail, "warning: ") {
			warnings = append(warnings, s.Name+": "+strings.TrimPrefix(s.Detail, "warning: "))
		}
	}
	d := fmt.Sprintf("The %s of %s from %s to %s finished.", mv.Kind, scopeName(mv), mv.FromNode, mv.ToNode)
	if len(warnings) > 0 {
		d += " Left to do: " + strings.Join(warnings, "; ") + "."
	}
	return d
}

func failedDetail(mv registry.Move, text string) string {
	verb := "stopped"
	if mv.State == registry.MoveAborted {
		verb = "was aborted and undone"
	}
	return fmt.Sprintf("The %s of %s from %s to %s %s after %s: %s. Run it again with --resume.", mv.Kind, scopeName(mv), mv.FromNode, mv.ToNode, verb, lastStep(mv), text)
}

func scopeName(mv registry.Move) string {
	if mv.Scope == registry.MoveProject {
		return "project " + mv.Ref
	}
	return "the server"
}

// announce raises the failover_* alert of a move. They are announcements: each is sent when it
// happens, and the cap does not hold them back.
func (o *Orchestrator) announce(ctx context.Context, kind, severity string, mv registry.Move, detail string) {
	what := "Project"
	if mv.Scope == registry.MoveServer {
		what = "Server"
	}
	title := fmt.Sprintf("%s %s %s", what, mv.Kind, map[string]string{
		alerts.KindFailoverStarted: "started", alerts.KindFailoverCompleted: "completed", alerts.KindFailoverFailed: "failed",
	}[kind])
	if mv.State == registry.MoveAborted && kind == alerts.KindFailoverFailed {
		title = fmt.Sprintf("%s %s aborted", what, mv.Kind)
	}
	o.alert(ctx, alerts.Event{Kind: kind, Severity: severity, Title: title, Detail: detail, Ref: mv.Ref,
		Key: fmt.Sprintf("failover/%s/%d/%s", mv.Scope, mv.ID, kind)})
}

// projectSteps runs the steps of a project move that are not in the log yet.
func (o *Orchestrator) projectSteps(ctx context.Context, j *journal, run *projectRun, begin projectBegin, timeout int, hold *lockHold) (err error) {
	ref := run.project.Ref
	paused := begin.Status == registry.StatusInactive
	st := o.store()
	setStatus := func(s registry.Status) {
		if err := st.SetProjectStatus(ctx, ref, s); err != nil {
			o.d.Log.Warn("could not set the project's status", "ref", ref, "status", s, "error", err)
		}
	}

	if run.planned {
		if err := j.step(ctx, "quiesce", func() (string, error) {
			setStatus(registry.StatusRestarting)
			return "", nil
		}); err != nil {
			return err
		}
		if err := j.step(ctx, "stop-old", func() (string, error) {
			lsn, err := o.stopPrimary(ctx, run.from.ID, ref)
			if err != nil {
				return "", fmt.Errorf("stopping the primary on %s: %w", run.from.Name, err)
			}
			// The position is what the promotion waits for. Without one the replica could be
			// promoted before it has the old primary's last WAL, and a switchover loses nothing
			// only because it waits. Nothing is recorded, so a resume stops again and reads it again.
			if _, perr := ParseLSN(lsn); perr != nil {
				return "", &abortError{cause: fmt.Errorf("%w: %s reported %q for %s", ErrNoFinalPosition, run.from.Name, lsn, ref)}
			}
			return lsn, nil
		}); err != nil {
			var abort *abortError
			if errors.As(err, &abort) {
				return o.undoSwitchover(ctx, j, run, begin, abort, hold)
			}
			return err
		}
	} else {
		if err := j.step(ctx, "fence-old", func() (string, error) {
			setStatus(registry.StatusRestarting)
			return o.fenceProject(ctx, run)
		}); err != nil {
			return err
		}
	}

	if err := j.step(ctx, "promote", func() (string, error) {
		return o.promoteReplica(ctx, run.to, begin.Replica, promoteArgs{
			Epoch: run.epoch, Timeout: timeout, WaitLSN: j.detail("stop-old"), Drain: !run.planned,
		})
	}); err != nil {
		var abort *abortError
		if errors.As(err, &abort) && run.planned {
			return o.undoSwitchover(ctx, j, run, begin, abort, hold)
		}
		return err
	}

	if err := j.step(ctx, "homed", func() (string, error) { return o.rehome(ctx, run, begin.Origin) }); err != nil {
		return err
	}

	if paused {
		// The project was paused: it runs where it is homed now, stopped like it was.
		if err := j.step(ctx, "pause-new", func() (string, error) {
			if _, err := o.d.Primaries.Stop(ctx, run.to.ID, ref); err != nil {
				return "", fmt.Errorf("pausing the project on %s: %w", run.to.Name, err)
			}
			return "", nil
		}); err != nil {
			return err
		}
	} else {
		if err := j.step(ctx, "start-new", func() (string, error) {
			if err := o.whileTheNodeLearnsWhoLeads(ctx, func() error { return o.d.Primaries.Start(ctx, run.to.ID, ref) }); err != nil {
				return "", fmt.Errorf("starting the project on %s: %w", run.to.Name, err)
			}
			return "", nil
		}); err != nil {
			return err
		}
		if err := j.step(ctx, "tenant", func() (string, error) {
			// The engine registers a project only while it is active, and takes the project's lock to do
			// it: the project is active again, and the move lets go of the lock meanwhile.
			if o.d.Fleet == nil {
				return "", nil
			}
			setStatus(statusAfter(begin.Status))
			return "", hold.without(ctx, func() error { return o.ensureTenant(ctx, ref) })
		}); err != nil {
			return err
		}
	}
	if run.planned {
		if err := j.step(ctx, "demote-old", func() (string, error) {
			return o.demoteOld(ctx, run.from, ref, j.detail("homed"), run.epoch, timeout)
		}); err != nil {
			setStatus(statusAfter(begin.Status))
			return fmt.Errorf("the project runs on %s, but %w", run.to.Name, err)
		}
	} else {
		if err := j.step(ctx, "reseed-old", func() (string, error) { return o.reseedOld(ctx, run.from, ref, run.epoch), nil }); err != nil {
			return err
		}
	}
	if !paused {
		if err := j.step(ctx, "base-backup", func() (string, error) { return o.baseBackup(ctx, run.to.ID, ref, run.epoch), nil }); err != nil {
			return err
		}
	}
	o.settle(ctx, ref, statusAfter(begin.Status))
	return nil
}

// settle gives the project the status the move ends with, unless something else changed it while the
// move let go of the lock: a project that was paused meanwhile keeps what that did.
func (o *Orchestrator) settle(ctx context.Context, ref string, s registry.Status) {
	st := o.store()
	if p, err := st.GetProject(ctx, ref); err == nil && !movable(p.Status) {
		return
	}
	if err := st.SetProjectStatus(ctx, ref, s); err != nil {
		o.d.Log.Warn("could not set the project's status", "ref", ref, "status", s, "error", err)
	}
}

// movable reports whether the status is one a move leaves behind or sets: the move's own RESTARTING,
// and the active ones it gives back.
func movable(s registry.Status) bool {
	return s == registry.StatusRestarting || s == registry.StatusActiveHealthy || s == registry.StatusActiveUnhealthy
}

// statusAfter is the status a project gets back once its move is over: paused stays paused, and
// anything else is healthy because the move made it so.
func statusAfter(before registry.Status) registry.Status {
	if before == registry.StatusInactive {
		return registry.StatusInactive
	}
	return registry.StatusActiveHealthy
}

// step runs fn unless the step is in the log, and records it when fn succeeds.
func (j *journal) step(ctx context.Context, name string, fn func() (string, error)) error {
	if j.has(name) {
		return nil
	}
	detail, err := fn()
	if err != nil {
		return err
	}
	return j.record(ctx, name, detail)
}

// promoteArgs are the arguments of promoteReplica.
type promoteArgs struct {
	Epoch   int64
	Timeout int
	// WaitLSN: promote only when replay has reached it (a planned move: the old primary's last WAL).
	WaitLSN string
	// Drain: let the archive drain first (an unplanned move: the WAL the standby has not received).
	Drain bool
}

// promoteReplica promotes the replica identifier on node.
//
// A promotion that waits for an LSN waits for it here first, before the node is asked to promote
// anything. A standby that does not get past the position in time is an abortError: nothing was
// asked of it, and the caller starts the old primary again. Once the position is passed, an error
// from the promotion itself says nothing about whether it happened (the node may still be inside
// pg_promote or its restart when the call fails), so it is returned as it is and the caller leaves
// the old primary stopped: two primaries are worse than a move that waits for --resume.
//
// An instance that is not in recovery any more is not taken for done, and no position is waited for
// (a primary has no replay position): the node is asked again. Its promotion is repeatable, and
// finishes what an earlier try left, which may be the restart on the canonical port: the daemon of a
// node whose system cluster is promoted restarts in the middle of the promotion and cuts it off.
func (o *Orchestrator) promoteReplica(ctx context.Context, node registry.Node, identifier string, a promoteArgs) (string, error) {
	if obs, err := o.d.Instances.Observe(ctx, node.ID, identifier); err == nil && !obs.InRecovery && obs.PostgresUp {
		a.WaitLSN = ""
	}
	if a.WaitLSN != "" {
		if err := o.waitReplayed(ctx, node.ID, identifier, a.WaitLSN); err != nil {
			if errors.Is(err, ErrReplayBehind) {
				return "", &abortError{cause: err}
			}
			return "", err
		}
	}
	req := peerapi.InstanceAction{Epoch: a.Epoch, WaitLSN: a.WaitLSN, DrainArchive: a.Drain, TimeoutSeconds: a.Timeout}
	err := o.whileTheNodeLearnsWhoLeads(ctx, func() error {
		_, err := o.d.Instances.Do(ctx, node.ID, identifier, peerapi.ActionPromote, req)
		return err
	})
	switch {
	case err == nil:
		return "primary on " + node.Name, nil
	case a.WaitLSN != "" && errors.Is(err, lifecycle.ErrReplayBehind):
		// The node waits for the position before it writes anything (promote.ok comes after the
		// wait), so this answer says the same as the wait above: nothing was promoted.
		return "", &abortError{cause: fmt.Errorf("%w: %s on %s: %v", ErrReplayBehind, identifier, node.Name, err)}
	case errors.Is(err, placement.ErrStaleEpoch):
		// The node is at a higher epoch than this move: a leader that was not replaced would not be.
		return "", fmt.Errorf("%w: promoting %s on %s was refused: %v", ErrEpochLost, identifier, node.Name, err)
	}
	return "", fmt.Errorf("promoting %s on %s: %w", identifier, node.Name, err)
}

// whileTheNodeLearnsWhoLeads runs a call to another node and repeats it while the node refuses it
// as coming from a node that does not lead (403). A node learns of a new leader from its replica
// of the registry, which trails the leader by a moment, so right after a server move the first
// calls of the new leader can be turned away. Nothing happened on the node when it refuses, so
// repeating the call is safe. Any other error ends it; a 409, which can be a standby that did not
// catch up, is not repeated here: the caller decides.
func (o *Orchestrator) whileTheNodeLearnsWhoLeads(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if werr := o.wait(ctx, 5*time.Second); werr != nil {
				return werr
			}
		}
		if err = call(); err == nil || !notYetLeader(err) {
			return err
		}
	}
	return err
}

// notYetLeader reports whether a node answered that the caller is not (yet) its leader, as the
// raw 403 of the mesh or as the cluster.ErrNotLeader that the placement layer turns it into.
func notYetLeader(err error) bool {
	var re *mesh.RemoteError
	return errors.Is(err, cluster.ErrNotLeader) || errors.As(err, &re) && (re.Status == http.StatusForbidden || re.Code == "not_leader")
}

// undoSwitchover puts a switchover back that stopped before the promotion: the old primary starts
// again, the shared services take the project back, and the project gets its status. The move ends
// aborted. If the old primary does not start, the project is down and the move says so.
func (o *Orchestrator) undoSwitchover(ctx context.Context, j *journal, run *projectRun, begin projectBegin, abort *abortError, hold *lockHold) error {
	ref := run.project.Ref
	if err := o.d.Primaries.Start(ctx, run.from.ID, ref); err != nil {
		_ = o.store().SetProjectStatus(ctx, ref, registry.StatusActiveUnhealthy)
		return fmt.Errorf("%w; starting the old primary on %s again also failed: %v", abort.cause, run.from.Name, err)
	}
	// Active again first: the engine registers a project with the shared services only while it is.
	_ = o.store().SetProjectStatus(ctx, ref, statusAfter(begin.Status))
	if begin.Status != registry.StatusInactive && o.d.Fleet != nil {
		if err := hold.without(ctx, func() error { return o.ensureTenant(ctx, ref) }); err != nil {
			o.d.Log.Warn("could not register the project with the shared services again", "ref", ref, "error", err)
		}
	}
	_ = j.record(ctx, "undo", "the old primary on "+run.from.Name+" runs again")
	return &abortError{cause: abort.cause}
}

// fenceProject asks the old home to stop the project's primary and keep it stopped. A home that is
// this node, the usual case since the leader runs the move and homes most projects, does it
// itself: a node cannot ask itself over the mesh.
func (o *Orchestrator) fenceProject(ctx context.Context, run *projectRun) (string, error) {
	req := FenceCall{
		FenceRequest: peerapi.FenceRequest{Epoch: run.epoch, Leader: o.self().ID, Reason: "project failover of " + run.project.Ref},
		Ref:          run.project.Ref,
	}
	var resp peerapi.FenceResponse
	var err error
	switch {
	case run.from.ID == o.self().ID:
		if resp, err = o.fenceProjectHere(ctx, req); err != nil {
			return "", fmt.Errorf("%w: %s could not stop the project: %v", ErrFence, run.from.Name, err)
		}
	case o.d.Peers == nil:
		return "", errors.New("no way to reach the old home")
	default:
		if resp, err = o.d.Peers.Fence(ctx, run.from.ID, req); err != nil {
			return "", fmt.Errorf("%w: %s did not answer the fence: %v", ErrHomeUnreachable, run.from.Name, err)
		}
	}
	if !resp.Fenced {
		return "", fmt.Errorf("%w: %s did not fence the project (it is at epoch %d)", ErrFence, run.from.Name, resp.Epoch)
	}
	return "fenced on " + run.from.Name, nil
}

// rehome flips the project's home in the registry and, for a switchover, adds the replica row of
// the old home, which the demotion then turns into a real replica in place. The detail is the
// new replica's identifier ("-" when there is none).
func (o *Orchestrator) rehome(ctx context.Context, run *projectRun, origin string) (string, error) {
	st := o.store()
	if err := st.SetProjectNode(ctx, run.project.Ref, run.to.ID, run.epoch); err != nil {
		return "", fmt.Errorf("moving the project's home to %s: %w", run.to.Name, err)
	}
	if !run.planned {
		return "-", nil
	}
	return o.addReplicaRow(ctx, run.project.Ref, run.from, origin)
}

// addReplicaRow inserts the replica row of a node that was the project's home. It reuses the row
// that an earlier try made. The row is complete and not healthy yet: the node reports the
// replica healthy once it streams, and the controller does not run a setup for a row at its last step.
func (o *Orchestrator) addReplicaRow(ctx context.Context, ref string, node registry.Node, origin string) (string, error) {
	st := o.store()
	reps, err := st.ListReplicas(ctx, ref)
	if err != nil {
		return "", err
	}
	for _, r := range reps {
		if r.NodeID == node.ID {
			return r.Identifier, nil
		}
	}
	if origin == "" {
		origin = registry.ReplicaManual
	}
	if ref == config.SystemRef {
		origin = registry.ReplicaSystem
	}
	r := registry.Replica{
		Identifier: registry.ReplicaIdentifier(ref, o.regionOf(node), newID6()), Ref: ref, NodeID: node.ID, Origin: origin,
		Status: "ACTIVE_UNHEALTHY", InitStep: registry.ReplicaStepDone,
	}
	if err := st.CreateReplica(ctx, &r); err != nil {
		return "", fmt.Errorf("adding the replica row of %s: %w", node.Name, err)
	}
	return r.Identifier, nil
}

// regionOf is the region part of a replica identifier for the node.
func (o *Orchestrator) regionOf(n registry.Node) string {
	r := strings.ToLower(n.Region)
	if r == "" {
		r = strings.ToLower(o.d.Cfg.Region)
	}
	if r == "" {
		r = "local"
	}
	return r
}

func newID6() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// lockHold is the project's lock as a move holds it, released once whatever happens.
type lockHold struct {
	o      *Orchestrator
	ref    string
	unlock func()
}

func (h *lockHold) release() {
	if h != nil && h.unlock != nil {
		h.unlock()
		h.unlock = nil
	}
}

// without runs fn with the project's lock let go and takes it again after. The engine's registration
// of a project with the shared services takes the same lock, which is not reentrant. The project is
// active then, not RESTARTING, so what else wants the lock finds it as it finds any active project.
func (h *lockHold) without(ctx context.Context, fn func() error) error {
	if h == nil || h.unlock == nil {
		return fn()
	}
	h.release()
	err := fn()
	unlock, lerr := h.o.lockProject(ctx, h.ref)
	if lerr != nil {
		return errors.Join(err, fmt.Errorf("taking the project's lock again: %w", lerr))
	}
	h.unlock = unlock
	return err
}

// tenantAttempts and tenantWait bound the tries to register a project with the shared services. A
// node that has just become the leader starts them while the move goes on, so the first try can meet
// a service that is not up yet.
const (
	tenantAttempts = 4
	tenantWait     = 5 * time.Second
)

// ensureTenant registers the project with the shared services again at its new home.
func (o *Orchestrator) ensureTenant(ctx context.Context, ref string) error {
	if o.d.Fleet == nil {
		return nil
	}
	var err error
	for attempt := 0; attempt < tenantAttempts; attempt++ {
		if attempt > 0 {
			if werr := o.wait(ctx, tenantWait); werr != nil {
				return werr
			}
		}
		if err = o.d.Fleet.EnsureTenant(ctx, ref); err == nil {
			return nil
		}
	}
	return fmt.Errorf("registering %s with the shared services: %w", ref, err)
}

// demoteOld turns the old home's stopped primary into a replica of the new home, in place. The
// new replica's row may not have reached the old node's registry yet, so it tries a few times.
func (o *Orchestrator) demoteOld(ctx context.Context, from registry.Node, ref, identifier string, epoch int64, timeout int) (string, error) {
	if identifier == "" || identifier == "-" {
		return "", errors.New("there is no replica row for the old home")
	}
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if werr := o.wait(ctx, 5*time.Second); werr != nil {
				return "", werr
			}
		}
		_, err = o.d.Instances.Do(ctx, from.ID, identifier, peerapi.ActionDemote, peerapi.InstanceAction{Epoch: epoch, TimeoutSeconds: timeout})
		if err == nil {
			return identifier + " on " + from.Name, nil
		}
	}
	return "", fmt.Errorf("turning the old primary on %s into a replica failed: %w; run the move again with --resume", from.Name, err)
}

// reseedOld rebuilds the old home of a failed-over project as a replica: its data diverged from the
// new primary's, so it is set aside, and the replica controller seeds a fresh standby. Neither
// failure undoes the move, which has served the project for a while by now; both are
// reported in the log and the alert as work left to do.
func (o *Orchestrator) reseedOld(ctx context.Context, from registry.Node, ref string, epoch int64) string {
	if err := o.setAside(ctx, from, ref, epoch); err != nil {
		return fmt.Sprintf("warning: the old primary's data on %s was not set aside (%v); remove it, then run supavise replicas add %s --node %s", from.Name, err, ref, from.ID)
	}
	if o.d.Replicas == nil {
		return "the old primary's data was set aside; add a replica there with supavise replicas add " + ref + " --node " + from.ID
	}
	if err := o.d.Replicas.SetupOn(ctx, ref, from.ID); err != nil {
		return fmt.Sprintf("warning: a replica could not be added on %s (%v); run supavise replicas add %s --node %s", from.Name, err, ref, from.ID)
	}
	return "a replica is being built on " + from.Name
}

// asideAttempts and asideWait bound the tries to set the old primary's data aside. The old home
// refuses while its copy of the registry still names it the project's home, and the copy follows the
// leader's change by a moment.
const (
	asideAttempts = 6
	asideWait     = 5 * time.Second
)

// setAside asks the old home to set the project's data aside, again while it answers that its registry
// still homes the project there (the answer a node gives until the leader's SetProjectNode has
// reached its copy).
func (o *Orchestrator) setAside(ctx context.Context, from registry.Node, ref string, epoch int64) error {
	var err error
	for attempt := 0; attempt < asideAttempts; attempt++ {
		if attempt > 0 {
			if werr := o.wait(ctx, asideWait); werr != nil {
				return werr
			}
		}
		if err = o.d.Primaries.SetAside(ctx, from.ID, ref, epoch); err == nil || !homedHere(err) {
			return err
		}
	}
	return err
}

// homedHere reports whether a node refused to set data aside because its registry homes the project on it.
func homedHere(err error) bool {
	var re *mesh.RemoteError
	return errors.As(err, &re) && re.Code == CodeHomedHere
}

// baseBackup takes a fresh base backup of the project on its new timeline. The move does not wait
// on it failing: the project is up, and the backup timer starts its own.
func (o *Orchestrator) baseBackup(ctx context.Context, node, ref string, epoch int64) string {
	if o.d.Backups == nil {
		return ""
	}
	if _, err := o.d.Backups.BaseBackup(ctx, node, ref, peerapi.BackupRequest{Epoch: epoch, Reason: "failover"}); err != nil {
		return fmt.Sprintf("warning: the base backup on the new timeline failed (%v); the next scheduled one takes it", err)
	}
	return "taken"
}
