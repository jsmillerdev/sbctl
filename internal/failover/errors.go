package failover

import (
	"errors"
	"fmt"
	"strings"
)

// Errors of the orchestrator. They wrap, so errors.Is answers.
var (
	// ErrBusy: another move is running on this node. One runs at a time.
	ErrBusy = errors.New("failover: another move is running")
	// ErrRefused: the preconditions failed. The error text lists them (see RefusedError).
	ErrRefused = errors.New("failover: refused")
	// ErrNothingToResume: --resume with no unfinished move.
	ErrNothingToResume = errors.New("failover: there is no unfinished move to resume")
	// ErrUnfinished: a move was started and not finished; run it again with --resume.
	ErrUnfinished = errors.New("failover: an earlier move is unfinished")
	// ErrHomeUnreachable: a project failover needs the old home to stop the primary, and it does
	// not answer. That is a node failure; the server failover handles it.
	ErrHomeUnreachable = errors.New("failover: the project's home node does not answer")
	// ErrReplayBehind: the replica did not replay up to the old primary's last WAL in time. Nothing
	// was promoted; the old primary is started again.
	ErrReplayBehind = errors.New("failover: the replica did not reach the old primary's final position")
	// ErrNoFinalPosition: a planned stop did not report the position the cluster stopped at, so
	// there is nothing to hold the promotion for. Nothing was promoted; the old primary is
	// started again.
	ErrNoFinalPosition = errors.New("failover: the stopped primary reported no final position")
	// ErrFence: the old primary could not be fenced, so nothing was promoted.
	ErrFence = errors.New("failover: the old primary could not be fenced")
	// ErrEpochLost: another node holds a higher epoch. Nothing was promoted.
	ErrEpochLost = errors.New("failover: another node holds a higher epoch")
	// ErrNoTakeover: the provider did not move the service address, and the operator must. The
	// move goes on and prints the DNS guidance. ErrNoServiceAddress and ErrCrossRegion are
	// cases of it.
	ErrNoTakeover       = errors.New("failover: the service address was not moved")
	ErrNoServiceAddress = fmt.Errorf("%w: the cluster has no service address to take over", ErrNoTakeover)
	ErrCrossRegion      = fmt.Errorf("%w: the nodes are in different regions and an Elastic IP belongs to one", ErrNoTakeover)
	// ErrNoCluster: this server has not joined a cluster, so there is nothing to fail over to.
	ErrNoCluster = errors.New("failover: this server is not part of a cluster")
	// ErrPlanChanged: the plan at the start of the run is not the one the operator confirmed
	// (ProjectOptions.ExpectKind, ServerOptions.ExpectKind). Nothing was changed.
	ErrPlanChanged = errors.New("failover: the plan changed since it was confirmed")
	// ErrNotLeader: the operation belongs to the leader.
	ErrNotLeader = errors.New("failover: this node is not the leader")
	// ErrRestarting: the daemon is stopping because its node's role in the cluster changed (the
	// system cluster it promoted makes it the leader, which the daemon takes up by restarting). The
	// move is not over and not failed: the daemon that starts continues it from its log.
	ErrRestarting = errors.New("failover: the daemon restarts in its new role and goes on with the move there")
	// ErrStreamClosed: the control socket closed before the move reported its end.
	ErrStreamClosed = errors.New("failover: the connection to the daemon closed before the move ended")
	// ErrNothingRunning: the daemon runs no move and has none it finished since it started.
	ErrNothingRunning = errors.New("failover: the daemon runs no move")
)

// RefusedError is returned (wrapping ErrRefused) when a move's preconditions fail.
type RefusedError struct {
	Checks []Check
	// Force is true when the move was started with --force; only Hard checks refuse then.
	Force bool
}

func (e *RefusedError) Error() string {
	var b strings.Builder
	b.WriteString("failover: refused:")
	for _, c := range e.Checks {
		fmt.Fprintf(&b, "\n  - %s", c.Name)
		if c.Detail != "" {
			fmt.Fprintf(&b, ": %s", c.Detail)
		}
	}
	return b.String()
}

func (e *RefusedError) Unwrap() error { return ErrRefused }
