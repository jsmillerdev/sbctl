package nodeupgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// announcingHost is the fake host of a machine that can announce maintenance in the registry.
type announcingHost struct {
	*fakeHost
	announceErr error
	endErr      error
}

func (a announcingHost) AnnounceMaintenance(_ context.Context, node, reason string) error {
	a.rec("announce %s %s", node, reason)
	return a.announceErr
}

func (a announcingHost) EndMaintenance(_ context.Context, node string) error {
	a.rec("end-maintenance %s", node)
	return a.endErr
}

func indexOf(order []string, prefix string) int {
	for i, c := range order {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// The leader of a cluster announces maintenance before anything is stopped and clears it when the
// upgrade ends, so that the follower's automatic failover never fires while the daemon restarts.
func TestUpgradeOnALeaderAnnouncesMaintenanceAroundTheChange(t *testing.T) {
	f := newFakeHost()
	f.node = clusterNode("v1.1.0")
	h := announcingHost{fakeHost: f}
	if err := Run(context.Background(), h, runOpts(f)); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	a, i, e := indexOf(f.calls, "announce n1 supavise upgrade to v1.1.0"), indexOf(f.calls, "install"), indexOf(f.calls, "end-maintenance n1")
	if a < 0 || a > i || e < indexOf(f.calls, "cleanup") {
		t.Fatalf("announce at %d, install at %d, end at %d:\n%s", a, i, e, f.order())
	}
	if a < indexOf(f.calls, "backup") {
		t.Fatalf("maintenance was announced before the backups, which run while the node serves:\n%s", f.order())
	}
	mustContain(t, f.out.String(), "no automatic failover while it runs")
	if n := strings.Count(f.order(), "announce"); n != 1 {
		t.Fatalf("announced %d times", n)
	}
}

// A run that fails, and rolls back, clears the announcement after the rollback: the daemon restarts
// twice and neither is a reason to fail over.
func TestFailedUpgradeOnALeaderClearsMaintenanceAfterTheRollback(t *testing.T) {
	f := newFakeHost()
	f.node = clusterNode("v1.1.0")
	f.installErr, f.installSwaps = errors.New("did not answer"), true
	h := announcingHost{fakeHost: f}
	if err := Run(context.Background(), h, runOpts(f)); code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v\n%s", err, f.order())
	}
	r, e := indexOf(f.calls, "restore"), indexOf(f.calls, "end-maintenance")
	if r < 0 || e < r {
		t.Fatalf("restore at %d, end at %d:\n%s", r, e, f.order())
	}
}

// A server that is not the leader of a cluster has nothing to announce.
func TestUpgradeOffTheLeaderAnnouncesNothing(t *testing.T) {
	single := newFakeHost()
	follower := newFakeHost()
	follower.node = clusterNode("v1.1.0")
	follower.node.Cluster.Leader = false
	follower.node.Projects = nil // a follower with a project of its own is refused; one without goes through
	for name, f := range map[string]*fakeHost{"a single server": single, "a follower": follower} {
		if err := Run(context.Background(), announcingHost{fakeHost: f}, runOpts(f)); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, f.out)
		}
		if f.has("announce") || f.has("end-maintenance") {
			t.Errorf("%s announced maintenance:\n%s", name, f.order())
		}
	}
}

// When the announcement cannot be made the upgrade stops before it changes anything and says so; a
// failover that fires during the restart would be the worse outcome.
func TestUpgradeStopsWhenMaintenanceCannotBeAnnounced(t *testing.T) {
	f := newFakeHost()
	f.node = clusterNode("v1.1.0")
	h := announcingHost{fakeHost: f, announceErr: errors.New("maintenance is already announced on n2")}
	err := Run(context.Background(), h, runOpts(f))
	if code(t, err) != ExitRefused || !strings.Contains(err.Error(), "already announced") || !strings.Contains(err.Error(), "nothing was stopped or changed") {
		t.Fatalf("err = %v", err)
	}
	if f.has("install") || f.has("projects") || f.has("end-maintenance") || len(f.notices) != 0 {
		t.Fatalf("something ran:\n%s\nnotices %v", f.order(), f.notices)
	}
}

// A leader that cannot clear the announcement says so and ends the run well: it expires by itself.
func TestUpgradeSucceedsWhenMaintenanceCannotBeCleared(t *testing.T) {
	f := newFakeHost()
	f.node = clusterNode("v1.1.0")
	h := announcingHost{fakeHost: f, endErr: errors.New("registry down")}
	if err := Run(context.Background(), h, runOpts(f)); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	mustContain(t, f.out.String(), "could not clear the maintenance announcement (registry down)")
}

func TestRollbackOnALeaderAnnouncesMaintenanceToo(t *testing.T) {
	f := rollbackHost()
	f.node = clusterNode("v1.1.0")
	f.node.Version = "v1.1.0"
	f.node.Pins = newPins()
	h := announcingHost{fakeHost: f}
	if err := Rollback(context.Background(), h, runOpts(f)); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	a, r, e := indexOf(f.calls, "announce n1 supavise rollback to v1.0.0"), indexOf(f.calls, "restore"), indexOf(f.calls, "end-maintenance n1")
	if a < 0 || a > r || e < indexOf(f.calls, "status") {
		t.Fatalf("announce at %d, restore at %d, end at %d:\n%s", a, r, e, f.order())
	}

	f = rollbackHost()
	f.node = clusterNode("v1.1.0")
	f.node.Version, f.node.Pins = "v1.1.0", newPins()
	h = announcingHost{fakeHost: f, announceErr: errors.New("busy")}
	err := Rollback(context.Background(), h, runOpts(f))
	if code(t, err) != ExitRefused || f.has("restore") || f.has("revert") {
		t.Fatalf("err = %v\n%s", err, f.order())
	}
}
