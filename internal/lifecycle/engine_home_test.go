package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// routedPlane is a plane that routes by home, as internal/placement's Router does.
type routedPlane struct{ *fakePlane }

func (routedPlane) RoutesByHome() {}

var _ HomeRouter = routedPlane{}

// A node drives only the projects homed on it, unless its plane routes by home: for a project it
// holds a replica of, the node's own plane would stop, restart or delete the replica.
func TestEngineRefusesAProjectHomedElsewhereWithoutARouter(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	ref := c.home2.Ref // homed on n2
	ops := map[string]func() error{
		"pause":    func() error { return c.e.Pause(ctx, ref) },
		"resume":   func() error { return c.e.Resume(ctx, ref) },
		"delete":   func() error { return c.e.Delete(ctx, ref) },
		"rotate":   func() error { _, err := c.e.RotateKeys(ctx, ref); return err },
		"password": func() error { return c.e.SetDatabasePassword(ctx, ref, "new-password") },
		"settings": func() error {
			_, err := c.e.ApplyConfig(ctx, ref, projectconfig.PostgREST, ApplyOptions{})
			return err
		},
		"resize": func() error { _, err := c.e.Resize(ctx, ref, "small"); return err },
		// The units of a project that runs elsewhere are not this node's to judge.
		"health": func() error { _, err := c.e.Health(ctx, ref); return err },
		"upgrade": func() error {
			_, err := c.e.BeginUpgrade(ctx, ref, UpgradeRequest{})
			return err
		},
	}
	for name, op := range ops {
		err := op()
		if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "homed on node "+c.home2.NodeID) {
			t.Errorf("%s of a project homed on another node: %v", name, err)
		}
	}
	if len(c.plane.calls) != 0 {
		t.Fatalf("the node's plane was driven for a project homed elsewhere: %v", c.plane.calls)
	}
	if p, _ := c.reg.GetProject(ctx, ref); p.Status != c.home2.Status {
		t.Fatalf("the status of the project changed from %s to %s", c.home2.Status, p.Status)
	}

	// The project homed here is driven as before.
	if err := c.e.Pause(ctx, c.home1.Ref); err != nil {
		t.Fatal(err)
	}
	if !c.plane.has("Stop " + c.home1.Ref) {
		t.Fatalf("calls: %v", c.plane.calls)
	}

	// An Engine whose plane routes by home reaches the project wherever it runs.
	c.plane.calls = nil
	c.e.SetPlane(routedPlane{c.plane})
	if err := c.e.Pause(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if !c.plane.has("Stop " + ref) {
		t.Fatalf("calls: %v", c.plane.calls)
	}
}

// A routing plane reaches a project on its home, but a restore, an upgrade and the final backup of a
// delete work on this node's own disk and backup service, which hold nothing of a project that runs
// elsewhere: they are refused, and a delete that takes no backup goes ahead through the plane.
func TestRoutedEngineRefusesWhatNeedsTheHomesDisk(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	fr := &fakeRestorer{h: c.harness}
	c.e = NewEngine(c.cfg, c.reg, c.sec, fakeArts{}, routedPlane{c.plane}, Options{Fleet: fleet.Fleet{c.tenant}, Backup: fr, NodeID: "n1"})
	ref, home := c.home2.Ref, c.home2.NodeID

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "homed on node "+home) {
			t.Errorf("%s of a project homed on another node: %v", what, err)
		}
	}
	_, err := c.e.BeginRestore(ctx, ref)
	refused("restore", err)
	_, err = c.e.BeginUpgrade(ctx, ref, UpgradeRequest{})
	refused("upgrade", err)
	refused("delete with a final backup", c.e.Delete(ctx, ref))
	if len(c.plane.calls) != 0 || len(fr.calls) != 0 || len(fr.reqs) != 0 {
		t.Fatalf("something ran for a project homed elsewhere: plane %v, backups %v, restores %v", c.plane.calls, fr.calls, fr.reqs)
	}
	if p, _ := c.reg.GetProject(ctx, ref); p.Status != c.home2.Status {
		t.Fatalf("the refused delete left the status %s, was %s", p.Status, c.home2.Status)
	}

	// The same operations on a project homed here work, as before.
	r, err := c.e.BeginRestore(ctx, c.home1.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, RestoreRequest{}); err != nil {
		t.Fatal(err)
	}

	// Without a backup, a delete needs only the plane and the registry, which the router reaches.
	if err := c.e.DeleteWith(ctx, ref, DeleteOptions{SkipFinalBackup: true}); err != nil {
		t.Fatal(err)
	}
	if !c.plane.has("Delete " + ref) {
		t.Fatalf("plane calls: %v", c.plane.calls)
	}
	if _, err := c.reg.GetProject(ctx, ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the project is still there: %v", err)
	}
}

// notSupportedPlane is a routing plane that, like internal/placement's Router, answers
// ErrNotSupported to the settings of a project homed on another node.
type notSupportedPlane struct{ *cfgPlane }

func (notSupportedPlane) RoutesByHome() {}
func (notSupportedPlane) ApplyPostgresSettings(context.Context, *registry.Project, *secrets.ProjectKeys, bool, func(context.Context)) (bool, error) {
	return false, ErrNotSupported
}

// Resuming a project homed on another node is no failure when the router cannot apply its saved Postgres
// settings: the resume records no config_apply_failed event for every project that runs elsewhere.
func TestResumeOfAProjectHomedElsewhereAppliesNoPostgresSettings(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	cp := &cfgPlane{fakePlane: c.plane}
	c.e = NewEngine(c.cfg, c.reg, c.sec, fakeArts{}, notSupportedPlane{cp}, Options{Fleet: fleet.Fleet{c.tenant}, Settings: &fakeSettings{}, NodeID: "n1"})
	ref := c.home2.Ref
	if err := c.e.Pause(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := c.e.Resume(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if ev := c.events(t, ref); strings.Contains(ev, "project.config_apply_failed") {
		t.Fatalf("events = %s", ev)
	}
	if len(cp.pgCalls) != 0 {
		t.Fatalf("Postgres settings were applied: %v", cp.pgCalls)
	}
	if len(c.tenant.ensured) == 0 || c.tenant.ensured[len(c.tenant.ensured)-1].Ref != ref {
		t.Fatalf("the tenants of the shared services were not refreshed: %+v", c.tenant.ensured)
	}
}

// remoteNodes answers the resources of the node a project is homed on.
type remoteNodes struct {
	res   NodeResources
	err   error
	asked []string
}

func (r *remoteNodes) NodeResources(_ context.Context, p *registry.Project) (NodeResources, error) {
	r.asked = append(r.asked, p.NodeID+" "+p.Ref)
	return r.res, r.err
}

// A resume, a resize and the list of sizes are judged against the node that runs the project, with the
// projects and replicas that node holds, and not against the leader's machine.
func TestRoutedEngineJudgesCapacityAgainstTheHome(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	c.cfg.Compute.Overcommit = 1
	c.e = NewEngine(c.cfg, c.reg, c.sec, fakeArts{}, routedPlane{c.plane}, Options{Fleet: fleet.Fleet{c.tenant}, Backup: c.backup, NodeID: "n1"})
	c.e.SetNode(func() NodeResources { return NodeResources{MemoryBytes: 64 * gib, CPUs: 16} }) // the leader's machine is large
	ref := c.home2.Ref
	remote := &remoteNodes{res: NodeResources{MemoryBytes: 2 * gib, CPUs: 4}}

	// Nothing is known of the home yet: no check, as before.
	if _, err := c.e.Resize(ctx, ref, "medium"); err != nil {
		t.Fatalf("a resize with no way to read the home: %v", err)
	}
	if _, err := c.e.Resize(ctx, ref, "micro"); err != nil {
		t.Fatal(err)
	}

	c.e.SetRemoteNodes(remote)
	_, err := c.e.Resize(ctx, ref, "medium")
	var ce *CapacityError
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "Medium") {
		t.Fatalf("a resize past the home's memory: %v", err)
	}
	if p, _ := c.reg.GetProject(ctx, ref); p.Class != "micro" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("the refused resize left %s %s", p.Class, p.Status)
	}
	if got := remote.asked; len(got) != 1 || got[0] != c.home2.NodeID+" "+ref {
		t.Fatalf("resources asked of %v", got)
	}
	if _, err := c.e.Resize(ctx, ref, "small"); err != nil {
		t.Fatalf("a resize that fits the home: %v", err)
	}

	offers, err := c.e.Offers(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if want := o.Class.MemoryBytes <= 2*gib; o.Fits != want {
			t.Errorf("%s fits = %v on a home with 2 GB, want %v", o.Class.Name, o.Fits, want)
		}
	}

	// Another project of the home holds its memory: a resume that would pass on an empty home does not.
	if _, err := c.e.Resize(ctx, ref, "micro"); err != nil {
		t.Fatal(err)
	}
	if err := c.e.Pause(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.SetProjectNode(ctx, c.home1.Ref, c.home2.NodeID, clusterEpoch(t, c.reg)); err != nil {
		t.Fatal(err)
	}
	other, err := c.reg.GetProject(ctx, c.home1.Ref)
	if err != nil {
		t.Fatal(err)
	}
	setSize := func(name string) {
		t.Helper()
		cl := mustClass(t, name)
		other.Class, other.Limits = cl.Name, cl.Limits()
		if err := c.reg.UpdateProject(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	setSize("small") // 2 GB: more than the home has left beside the resumed project
	if err := c.e.Resume(ctx, ref); !errors.As(err, &ce) {
		t.Fatalf("a resume with no room on the home: %v", err)
	}
	if p, _ := c.reg.GetProject(ctx, ref); p.Status != registry.StatusInactive {
		t.Fatalf("the refused resume left the status %s", p.Status)
	}
	setSize("nano")
	if err := c.e.Resume(ctx, ref); err != nil {
		t.Fatalf("a resume with room on the home: %v", err)
	}

	// A home that cannot be asked refuses the change, naming the node.
	remote.err = errors.New("connection refused")
	if _, err := c.e.Resize(ctx, ref, "small"); err == nil || !strings.Contains(err.Error(), "node "+c.home2.NodeID) || errors.As(err, &ce) {
		t.Fatalf("a resize with an unreachable home: %v", err)
	}
}

func clusterEpoch(t *testing.T, reg registry.Registry) int64 {
	t.Helper()
	cl, err := reg.GetCluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cl.Epoch
}

// SettleUpgrades leaves the projects of other nodes alone unless the plane routes to them, and does
// nothing on a follower.
func TestSettleUpgradesLeavesProjectsOfOtherNodesAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		plane    func(*clusterHarness) Plane
		readOnly bool
		settles  bool
	}{
		{"plain plane", func(c *clusterHarness) Plane { return c.plane }, false, false},
		{"routing plane", func(c *clusterHarness) Plane { return routedPlane{c.plane} }, false, true},
		{"follower", func(c *clusterHarness) Plane { return routedPlane{c.plane} }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClusterHarness(t, "n1")
			c.e = NewEngine(c.cfg, c.reg, c.sec, fakeArts{}, tc.plane(c), Options{Fleet: fleet.Fleet{c.tenant}, NodeID: "n1", ReadOnly: tc.readOnly})
			ref := c.home2.Ref
			if err := c.reg.SetProjectStatus(ctx, ref, registry.StatusUpgrading); err != nil {
				t.Fatal(err)
			}
			got := c.e.SettleUpgrades(ctx)
			if settled := len(got) > 0; settled != tc.settles {
				t.Fatalf("settled %v, want settling = %v", got, tc.settles)
			}
			if !tc.settles {
				if p, _ := c.reg.GetProject(ctx, ref); p.Status != registry.StatusUpgrading {
					t.Fatalf("the status moved to %s", p.Status)
				}
			}
		})
	}
}

// orderedConfigPlane notes in the plane's calls when the database restarts.
type orderedConfigPlane struct{ *cfgPlane }

func (o orderedConfigPlane) ApplyPostgresSettings(ctx context.Context, p *registry.Project, k *secrets.ProjectKeys, restart bool, before func(context.Context)) (bool, error) {
	if restart {
		_ = o.fakePlane.rec("Restart " + p.Ref)
	}
	return o.cfgPlane.ApplyPostgresSettings(ctx, p, k, restart, before)
}

// A standby pauses replay when the primary's max_connections and the like rise above its own, so a
// restart of the primary for new Postgres settings restarts the replicas after it.
func TestPostgresSettingsRestartRestartTheReplicas(t *testing.T) {
	ctx := context.Background()
	h, p, fl, ids := replicaHarness(t)
	cp := &cfgPlane{fakePlane: h.plane}
	set := &fakeSettings{}
	rt := &refreshTenant{}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, orderedConfigPlane{cp}, Options{Fleet: fleet.Fleet{rt}, Settings: set, Replicas: fl})
	h.plane.calls = nil

	// A save that restarts the database restarts the replicas, once the primary is back.
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Postgres, ApplyOptions{RestartDatabase: true}); err != nil {
		t.Fatal(err)
	}
	want := "Restart " + p.Ref + ",Replica " + ids[0] + ",Replica " + ids[1]
	if got := inReplicaOrder(h.plane.calls); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	for _, id := range ids {
		if r, _ := h.reg.GetReplica(ctx, id); r.Status != string(registry.StatusActiveHealthy) {
			t.Fatalf("replica %s is %s", id, r.Status)
		}
	}

	// A save that leaves the restart to the user touches no replica.
	h.plane.calls = nil
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Postgres, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(h.plane.calls) != 0 {
		t.Fatalf("calls without a restart: %v", h.plane.calls)
	}

	// A replica that does not come back is marked, tells the fleet and leaves an event; the save succeeds.
	h.plane.calls = nil
	fl.failing[ids[0]] = errors.New("node n2 is unreachable")
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Postgres, ApplyOptions{RestartDatabase: true}); err != nil {
		t.Fatalf("the save failed because of a replica: %v", err)
	}
	if r, _ := h.reg.GetReplica(ctx, ids[0]); r.Status != string(registry.StatusActiveUnhealthy) {
		t.Fatalf("the replica that failed is %s", r.Status)
	}
	if len(fl.failed) != 1 || !strings.Contains(fl.failed[0], "Postgres settings") {
		t.Fatalf("failed = %v", fl.failed)
	}
	evs, err := h.reg.ListEvents(ctx, p.Ref, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || ev.Kind == EventReplicaRestartFailed
	}
	if !found {
		t.Fatalf("no %s event in %+v", EventReplicaRestartFailed, evs)
	}
}
