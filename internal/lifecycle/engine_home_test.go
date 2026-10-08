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
	if got := strings.Join(h.plane.calls, ","); got != want {
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
