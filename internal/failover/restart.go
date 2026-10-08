package failover

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// The daemon restarts when the role of its node changes. The system cluster that a server move
// promotes makes the node the leader, and the daemon takes a new role by stopping and starting again
// (the boot decision opens the registry writable, binds the leader's ports and starts the shared
// services; internal/app/wire_mesh.go). A server move that runs in the daemon of the survivor is
// therefore cut off once, between the promotion of the system cluster and the end of the move. That
// is not a failure and is not reported as one: the move is in failover.json (or, once adopted, in the
// moves table) in the state running, and the daemon that starts continues it (ResumeInterrupted),
// which the CLI follows (Follow, Client.Follow).

// resumeFallback is how long a restarted daemon waits before it continues a move itself. The wiring of
// a daemon continues one as soon as the shared services are up, because the move registers the projects
// with them again, and what it finds finished is not started twice; this is for a wiring that does not,
// and it is slower than any wiring needs to be. A variable for tests.
var resumeFallback = 3 * time.Minute

// interrupted reports whether the run of the move ended because of the daemon's role change: it is a
// server move that passed its leader marker (from there the system cluster is promoted or about to be
// and the move goes on from its log), and either its context is over (the daemon is stopping) or the
// registry it writes to still refuses writes (the daemon has not restarted yet, and the promoted
// cluster is writable only for the daemon that starts). A move cut off earlier failed like any other,
// and its owner decides about it.
func interrupted(ctx context.Context, j *journal, runErr error) bool {
	if j.move.Scope != registry.MoveServer || !j.has("marker") {
		return false
	}
	return ctx.Err() != nil || errors.Is(runErr, registry.ErrReadOnly)
}

// ResumeInterrupted continues the server move to this node that a restart of the daemon cut off. It
// does nothing unless the move is still running (a move that ended failed is the operator's:
// `supavise failover --resume`), passed its leader marker, and this node leads now, which is what
// the restart was for. The run is kept for Follow, and for the node that started the move to follow
// (the old leader of a switchover it asked for with --to). It returns nil, nil when there was
// nothing to continue.
func (o *Orchestrator) ResumeInterrupted(ctx context.Context) (*registry.Move, error) {
	prior, _, err := o.unfinishedServer(ctx)
	switch {
	case err != nil:
		return nil, err
	case prior == nil || prior.to != o.self().ID || !prior.running || !recordedStep(prior.steps, "marker"):
		return nil, nil
	case !o.d.Members.IsLeader():
		o.d.Log.Warn("a server move to this node stopped after its leader marker, and this node does not lead yet; finish it with supavise failover --resume",
			"from", prior.from, "to", prior.to, "epoch", prior.epoch, "last", prior.last)
		return nil, nil
	}
	o.d.Log.Info("continuing the server move that the restart of the daemon cut off", "from", prior.from, "to", prior.to, "epoch", prior.epoch, "last", prior.last)
	if err := o.wait(ctx, resumeFallback); err != nil {
		return nil, err
	}
	// The wait is a chance for the wiring to have continued the move: what it finished is not here.
	if again, _, err := o.unfinishedServer(ctx); err != nil || again == nil || !again.running {
		return nil, err
	}
	// Another caller may be running it now (the wiring's, which is slower than the wait on a node whose
	// services take long to come up): its run is the one that is followed, and this one starts nothing.
	if o.Busy() {
		o.d.Log.Info("another caller continues the server move that the restart cut off")
		return nil, nil
	}
	run := o.keepRun(prior.from)
	var mv *registry.Move
	var rerr error
	run.record(ctx, func(ctx context.Context) (*registry.Move, error) {
		mv, rerr = o.FailoverServer(ctx, ServerOptions{Resume: true, Yes: true})
		return mv, rerr
	})
	switch {
	case errors.Is(rerr, ErrBusy): // it started between the look and the call
		o.d.Log.Info("another caller continues the server move that the restart cut off")
		return nil, nil
	case rerr != nil:
		o.d.Log.Error("the server move that was cut off by the restart did not finish; run supavise failover --resume", "error", rerr)
	}
	return mv, rerr
}

func recordedStep(steps []registry.MoveStep, name string) bool {
	return slices.ContainsFunc(steps, func(s registry.MoveStep) bool { return s.Name == name })
}
