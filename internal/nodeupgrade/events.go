package nodeupgrade

import "context"

// Kinds of Event: what `supavise upgrade` and `supavise rollback` tell Options.Notify.
const (
	// EventStarted: the preparation passed (the artifacts are fetched and the backups are taken, and
	// none of it changed anything that runs) and the node is about to change.
	EventStarted = "started"
	// EventSucceeded: the node runs the new release and its status is no worse than before.
	EventSucceeded = "succeeded"
	// EventRefused: the node was about to change (EventStarted was sent) and the run stopped before it
	// changed anything that runs, for example because the new binary could not be installed (exit
	// status 2). A refusal before EventStarted raises no event.
	EventRefused = "refused"
	// EventRolledBack: the change failed and the node is back on the previous release (exit status 3).
	EventRolledBack = "rolled_back"
	// EventNeedsOperator: the change failed and the node is not back (exit status 4).
	EventNeedsOperator = "needs_operator"
	// EventInfraBehind: the plan found the node's AWS stack behind what the release needs, and the
	// run did not update it (an unattended run cannot). The run itself is not a failure.
	EventInfraBehind = "infra_behind"
)

// Event is one thing the operator should hear about an upgrade or a rollback. The package sends no
// message itself: cmd/supavise turns an Event into an alert, because the command runs outside the
// daemon and the alerting package is not this one's concern. A run that is refused (exit status 2)
// before EventStarted changed nothing and raises no Event.
type Event struct {
	Kind string
	// Rollback is true for `supavise rollback`, false for `supavise upgrade`.
	Rollback bool
	// From is the release the node ran when the command started; To is the release it was moving to
	// (for a rollback, the previous release it goes back to).
	From, To string
	// Unattended is true for `supavise upgrade --unattended`, which the maintenance window runs.
	Unattended bool
	// Projects counts the projects the upgrade moves (EventStarted) or moved (EventSucceeded), and the
	// projects a rollback puts back.
	Projects int
	// Halted is the project whose upgrade stopped the rollout; empty when none did (see HaltReporter).
	Halted string
	// Cause is why the change failed (EventRolledBack, EventNeedsOperator); for EventNeedsOperator it
	// also says what state the node is in.
	Cause string
	// BackTo is the release the node runs again after EventRolledBack.
	BackTo string
}

// HaltReporter is the optional Host capability that names the project whose upgrade stopped the
// rollout, so that a failure event can say which one.
type HaltReporter interface {
	// HaltedProject returns the project the last UpgradeProjects stopped at, or "".
	HaltedProject() string
}

func (o *Options) notify(ctx context.Context, ev Event) {
	if o.Notify == nil {
		return
	}
	ev.Unattended = o.Unattended
	o.Notify(ctx, ev)
}

func (r *run) halted() string {
	if hr, ok := r.h.(HaltReporter); ok {
		return hr.HaltedProject()
	}
	return ""
}
