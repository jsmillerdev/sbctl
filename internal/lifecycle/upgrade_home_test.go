package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// pendingPlane is a plane that can hold restarts back, and notes when it is asked.
type pendingPlane struct {
	*upPlane
	asked []string
	err   error // what both calls answer instead of yes
}

func (p *pendingPlane) PendingRestart(_ context.Context, pr *registry.Project, _ *secrets.ProjectKeys) (bool, error) {
	p.asked = append(p.asked, "PendingRestart "+pr.Ref)
	return p.err == nil, p.err
}

func (p *pendingPlane) RestartPending(_ context.Context, pr *registry.Project, _ *secrets.ProjectKeys) (bool, error) {
	p.asked = append(p.asked, "RestartPending "+pr.Ref)
	return p.err == nil, p.err
}

var _ PendingRestarter = (*pendingPlane)(nil)

// movedUpHarness is an upHarness whose project is homed on node n2.
func movedUpHarness(t *testing.T) *upHarness {
	t.Helper()
	ctx := context.Background()
	h := newUpHarness(t)
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(ctx, h.ref, n2.ID, clusterEpoch(t, h.reg)); err != nil {
		t.Fatal(err)
	}
	return h
}

// `supavise upgrade` runs `projects upgrade --all --restart-changed` on every node over the whole
// registry. A project homed on another node is that node's: the planner says so instead of offering
// an upgrade that BeginUpgrade refuses (which halts the rollout), and PendingRestart neither asks the
// plane nor renders the project's units into this node's state directory (where a replica of the
// project may run on the files of the replica).
func TestUpgradePlannerAndPendingRestartLeaveAProjectToItsHome(t *testing.T) {
	ctx := context.Background()
	h := movedUpHarness(t)
	p := h.project(t)
	pp := &pendingPlane{upPlane: h.plane}
	engineOn := func(node string) *Engine {
		return NewEngine(h.cfg, h.reg, h.e.sec, h.arts, pp, Options{Fleet: fleet.Fleet{&fakeTenant{}}, Backup: h.backup, NodeID: node})
	}

	// On its home the project is eligible.
	el, err := engineOn(p.NodeID).UpgradeEligibilityFor(ctx, h.ref, UpgradeRequest{})
	if err != nil || !el.Eligible || len(el.Blockers) != 0 {
		t.Fatalf("on the home: %+v, %v", el, err)
	}

	// Anywhere else it is not, and the reason names the home.
	other := engineOn("n1")
	el, err = other.UpgradeEligibilityFor(ctx, h.ref, UpgradeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if el.Eligible || len(el.Blockers) != 1 || el.Blockers[0].Type != BlockerElsewhere || !strings.Contains(el.Blockers[0].Message, "node "+p.NodeID) {
		t.Fatalf("on another node: eligible %v, blockers %+v", el.Eligible, el.Blockers)
	}
	if len(el.Ahead) != 0 {
		t.Fatalf("a project of another node is judged against this node's pins: ahead %+v", el.Ahead)
	}
	if _, err := other.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("BeginUpgrade = %v", err)
	}

	// PendingRestart renders units, so it does not look at a project that runs elsewhere.
	pp.asked = nil
	if pending, err := other.PendingRestart(ctx, h.ref); err != nil || pending {
		t.Fatalf("PendingRestart on another node = %v, %v", pending, err)
	}
	if restarted, err := other.RestartPending(ctx, h.ref); !errors.Is(err, ErrInvalidState) || restarted {
		t.Fatalf("RestartPending on another node = %v, %v", restarted, err)
	}
	if len(pp.asked) != 0 {
		t.Fatalf("the plane was asked about a project of another node: %v", pp.asked)
	}

	// The home asks as before.
	home := engineOn(p.NodeID)
	if pending, err := home.PendingRestart(ctx, h.ref); err != nil || !pending {
		t.Fatalf("PendingRestart on the home = %v, %v", pending, err)
	}
	if got := strings.Join(pp.asked, ","); got != "PendingRestart "+h.ref {
		t.Fatalf("asked = %s", got)
	}
}

// A node that holds fenced primaries and awaits `node rejoin` finishes `supavise upgrade`: the fenced
// primary does not run here, so it owes no restart, and the CLI's loop reaches the projects after it.
// Any other error still stops the call.
func TestPendingRestartOfAFencedPrimaryOwesNothing(t *testing.T) {
	ctx := context.Background()
	h := newUpHarness(t)
	pp := &pendingPlane{upPlane: h.plane, err: fmt.Errorf("%w: n2 leads", ErrFenced)}
	e := NewEngine(h.cfg, h.reg, h.e.sec, h.arts, pp, Options{Fleet: fleet.Fleet{&fakeTenant{}}, Backup: h.backup})
	if pending, err := e.PendingRestart(ctx, h.ref); err != nil || pending {
		t.Fatalf("PendingRestart = %v, %v", pending, err)
	}
	if restarted, err := e.RestartPending(ctx, h.ref); err != nil || restarted {
		t.Fatalf("RestartPending = %v, %v", restarted, err)
	}
	pp.err = errors.New("the supervisor did not answer")
	if _, err := e.PendingRestart(ctx, h.ref); err == nil {
		t.Fatal("another failure was hidden")
	}
}

// A server outside a cluster (no node id) asks about every project, as it always did.
func TestUpgradePlannerWithoutANodeJudgesEveryProject(t *testing.T) {
	h := movedUpHarness(t)
	el, err := h.e.UpgradeEligibilityFor(context.Background(), h.ref, UpgradeRequest{})
	if err != nil || !el.Eligible {
		t.Fatalf("%+v, %v", el, err)
	}
}
