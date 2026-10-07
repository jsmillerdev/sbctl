package nodeupgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// ErrRegistryNewer means the registry was migrated past what the release to roll back to
// expects.
var ErrRegistryNewer = errors.New("the registry schema is newer than the release to go back to expects")

// CheckRollback applies the one rule that makes a rollback unsafe by itself: registry migrations
// only go forward, so a binary that does not know every migration the registry holds may find
// tables and columns it does not know, and may write rows the newer release cannot read. The rule
// refuses then. Restoring the pre-upgrade base backup of the system project (which holds the
// registry) brings the registry back; after that the same check passes, and nothing else has to be
// said. It compares sets of migrations, not their newest name: the lanes of the project number
// their migrations in separate ranges, so the newest name says little.
func CheckRollback(to *Record, applied []string) error {
	known := make(map[string]bool, len(to.Migrations))
	for _, n := range to.Migrations {
		known[n] = true
	}
	var unknown []string
	for _, n := range applied {
		if !known[n] {
			unknown = append(unknown, n)
		}
	}
	switch {
	case len(unknown) == 0:
		return nil
	case len(to.Migrations) == 0:
		return fmt.Errorf("%w: the kept record of %s does not say which migrations it knows, and the registry holds %d", ErrRegistryNewer, to.Version, len(applied))
	}
	return fmt.Errorf("%w: the registry holds %d migration(s) that %s does not know (%s), and migrations only go forward. Restore the system project's pre-upgrade backup first (`supavise backups restore system`, see the backups documentation), then run the rollback again", ErrRegistryNewer, len(unknown), to.Version, refList(unknown))
}

// ProjectMove is one project an upgrade moved: the releases it ran and the ones it runs.
type ProjectMove struct {
	Ref      string
	From, To map[string]string
	At       time.Time
}

// RevertTargets returns, for each move, the services that changed and the releases to put back,
// as the arguments `projects upgrade --to` takes (service name to tag).
func (m ProjectMove) RevertTargets() map[string]string {
	out := map[string]string{}
	for svc, to := range m.To {
		if from := m.From[svc]; from != "" && from != to {
			out[svc] = from
		}
	}
	return out
}

// rollbackArgs is what rollBackTo needs to know about the move it undoes.
type rollbackArgs struct {
	// From is the release the node runs now and FromPins its pins; To is the kept release to go
	// back to.
	From     string
	FromPins map[string]string
	To       *Record
	// Moves are the projects to put back on the releases they ran.
	Moves []ProjectMove
	// Verdict is the status verdict the node had before the upgrade; the rollback is done when
	// the node is no worse.
	Verdict string
	Mark    func(phase, detail string)
}

// rollBackTo is the common part of the automatic rollback after a failed upgrade and of
// `supavise rollback`: the registry rule, the projects, the binary, the shared services, the
// verdict. It returns nil when the node is back on a.To and healthy, and an error that says what
// state the node is in otherwise.
func rollBackTo(ctx context.Context, h Host, o Options, a rollbackArgs) error {
	applied, err := h.AppliedMigrations(ctx)
	if err != nil {
		return fmt.Errorf("cannot read the registry's migrations, so the rollback is not safe to start: %v. The node stays on %s; fix the registry connection and run `supavise rollback`", err, a.From)
	}
	if err := CheckRollback(a.To, applied); err != nil {
		return fmt.Errorf("%v. The node stays on %s", err, a.From)
	}
	var revertErr error
	if len(a.Moves) > 0 {
		o.say("putting %d project(s) back on the releases they ran", len(a.Moves))
		if revertErr = h.RevertProjects(ctx, a.Moves); revertErr != nil {
			o.say("warning: not every project went back: %v", revertErr)
		}
	}
	a.Mark(PhaseRollingBack, "restoring "+a.To.Version)
	if err := h.Restore(ctx, a.From, *a.To); err != nil {
		return fmt.Errorf("restoring %s failed: %v. Its binary is kept in the releases directory; install it by hand and restart supavise.service", a.To.Version, err)
	}
	var undo []ServiceMove
	for _, m := range DiffPins(a.FromPins, a.To.Pins) {
		if m.Service != config.SvcPostgREST {
			undo = append(undo, m)
		}
	}
	if err := h.WaitShared(ctx, undo); err != nil {
		return fmt.Errorf("%s is running again, but a shared service did not come back on its old release: %v", a.To.Version, err)
	}
	if err := waitVerdict(ctx, h, o, a.Verdict); err != nil {
		return fmt.Errorf("%s is running again, but the node is not healthy: %v", a.To.Version, err)
	}
	if revertErr != nil {
		return fmt.Errorf("%s is running again, but not every project went back to its old releases: %v. `supavise projects versions` lists them", a.To.Version, revertErr)
	}
	return nil
}
