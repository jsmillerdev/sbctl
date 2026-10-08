package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
)

// clusterHarness is a harness with a second node and one project homed on each node.
type clusterHarness struct {
	*harness
	home1, home2 *registry.Project // homed on n1 and on n2
}

func newClusterHarness(t *testing.T, nodeID string) *clusterHarness {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	a, err := h.e.Create(ctx, CreateRequest{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.e.Create(ctx, CreateRequest{Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := h.reg.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(ctx, b.Ref, n2.ID, cl.Epoch); err != nil {
		t.Fatal(err)
	}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup, NodeID: nodeID})
	h.plane.calls, h.tenant.ensured = nil, nil
	b, _ = h.reg.GetProject(ctx, b.Ref)
	return &clusterHarness{harness: h, home1: a, home2: b}
}

func TestEngineLeavesProjectsOfOtherNodesToThem(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		node string
		want func(c *clusterHarness) []string
	}{
		{"n1", func(c *clusterHarness) []string { return []string{c.home1.Ref} }},
		{"n2", func(c *clusterHarness) []string { return []string{c.home2.Ref} }},
		{"", func(c *clusterHarness) []string { return []string{c.home1.Ref, c.home2.Ref} }},
	} {
		t.Run("node "+tc.node, func(t *testing.T) {
			c := newClusterHarness(t, tc.node)
			if errs := c.e.StartActive(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			var started []string
			for _, call := range c.plane.calls {
				if ref, ok := strings.CutPrefix(call, "Start "); ok {
					started = append(started, ref)
				}
			}
			if got, want := strings.Join(started, ","), strings.Join(tc.want(c), ","); !sameSet(got, want) {
				t.Fatalf("StartActive started %s, want %s", got, want)
			}
			if errs := c.e.EnsureTenants(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			var ensured []string
			for _, ts := range c.tenant.ensured {
				ensured = append(ensured, ts.Ref)
			}
			if got, want := strings.Join(ensured, ","), strings.Join(tc.want(c), ","); !sameSet(got, want) {
				t.Fatalf("EnsureTenants registered %s, want %s", got, want)
			}
		})
	}
}

func sameSet(a, b string) bool {
	x, y := strings.Split(a, ","), strings.Split(b, ",")
	if len(x) != len(y) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range x {
		seen[s] = true
	}
	for _, s := range y {
		if !seen[s] {
			return false
		}
	}
	return true
}

func TestRecoverSettlesOnlyTheProjectsOfThisNode(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n1")
	for _, p := range []*registry.Project{c.home1, c.home2} {
		if err := c.reg.SetProjectStatus(ctx, p.Ref, registry.StatusPausing); err != nil {
			t.Fatal(err)
		}
	}
	got := c.e.Recover(ctx)
	if len(got) != 1 || got[0].Ref != c.home1.Ref || got[0].To != registry.StatusInactive {
		t.Fatalf("recovered = %+v", got)
	}
	if p, _ := c.reg.GetProject(ctx, c.home2.Ref); p.Status != registry.StatusPausing {
		t.Fatalf("a project of another node was settled: %s", p.Status)
	}
	for _, call := range c.plane.calls {
		if strings.Contains(call, c.home2.Ref) {
			t.Fatalf("the plane was driven for a project of another node: %v", c.plane.calls)
		}
	}
}

func TestReadOnlyEngineSettlesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	if err := h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusPausing); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{ReadOnly: true, NodeID: "n1"})
	h.plane.calls = nil
	if got := e.Recover(ctx); got != nil {
		t.Fatalf("a read-only Engine recovered %+v", got)
	}
	if cur, _ := h.reg.GetProject(ctx, p.Ref); cur.Status != registry.StatusPausing || len(h.plane.calls) != 0 {
		t.Fatalf("status %s, calls %v", cur.Status, h.plane.calls)
	}
	// Its lock is the process mutex only: taking it twice in turn works and needs no registry connection.
	unlock, err := e.lock(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestSetPlaneSwapsWhatTheEngineDrives(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	other := newFakePlane()
	h.e.SetPlane(other)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if !other.has("Stop "+p.Ref) || h.plane.has("Stop "+p.Ref) {
		t.Fatalf("calls: new %v, old %v", other.calls, h.plane.calls)
	}
}

func TestCapacityCountsReplicasOnTheNode(t *testing.T) {
	ctx := context.Background()
	c := newClusterHarness(t, "n2")
	c.node(8, 4, 1) // an 8 GB node, no overcommit
	c.e.opts.NodeID = "n2"
	// n2 is the home of one project and holds the replica of the other.
	id := registry.ReplicaIdentifier(c.home1.Ref, "us-east-1", "abc123")
	if err := c.reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: c.home1.Ref, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	cp, known, err := c.e.Capacity(ctx, "")
	if err != nil || !known {
		t.Fatal(known, err)
	}
	mem := projectMemory(c.home1)
	if cp.Projects != 1 || cp.Replicas != 1 || cp.CommittedBytes != projectMemory(c.home2)+mem {
		t.Fatalf("capacity = %+v", cp)
	}
	if got := cp.Summary(); !strings.Contains(got, "1 projects and 1 replica") {
		t.Fatalf("summary = %q", got)
	}
	// The replica holds its project's memory, so a size that does not fit beside it is refused.
	if err := (Capacity{Node: NodeResources{MemoryBytes: 4 * gib, CPUs: 4}, Overcommit: 1, BudgetBytes: 2 * gib, CommittedBytes: 2 * gib, Projects: 1, Replicas: 1}).Fits(mustClass(t, "small")); err == nil || !strings.Contains(err.Error(), "after 1 other projects and 1 replica") {
		t.Fatalf("fits = %v", err)
	}
	// A failed or going-down replica holds nothing; neither does the replica of a paused project.
	if err := c.reg.SetReplicaStatus(ctx, id, registry.ReplicaInitError, "3_download_base_backup_failed", "x"); err != nil {
		t.Fatal(err)
	}
	if cp, _, _ = c.e.Capacity(ctx, ""); cp.Replicas != 0 {
		t.Fatalf("a failed replica counted: %+v", cp)
	}
	if err := c.reg.SetReplicaStatus(ctx, id, string(registry.StatusActiveHealthy), "6_completed_read_replica_setup", ""); err != nil {
		t.Fatal(err)
	}
	if cp, _, _ = c.e.Capacity(ctx, c.home1.Ref); cp.Replicas != 0 {
		t.Fatalf("the excluded project's replica counted: %+v", cp)
	}
	if err := c.reg.SetProjectStatus(ctx, c.home1.Ref, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if cp, _, _ = c.e.Capacity(ctx, ""); cp.Replicas != 0 {
		t.Fatalf("the replica of a paused project counted: %+v", cp)
	}
	// On n1 the same registry says: one project homed here, no replicas.
	if err := c.reg.SetProjectStatus(ctx, c.home1.Ref, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	c.e.opts.NodeID = "n1"
	if cp, _, _ = c.e.Capacity(ctx, ""); cp.Projects != 1 || cp.Replicas != 0 || cp.CommittedBytes != mem {
		t.Fatalf("n1 capacity = %+v", cp)
	}
}

// fakeFleet is a ReplicaFleet that logs into the plane's call list, so that an order can be asserted.
type fakeFleet struct {
	mu      sync.Mutex
	h       *harness
	rows    []registry.Replica
	failing map[string]error
	failed  []string
	classes []string
}

func (f *fakeFleet) Replicas(_ context.Context, ref string) ([]registry.Replica, error) {
	return f.h.reg.ListReplicas(context.Background(), ref)
}

func (f *fakeFleet) Restart(_ context.Context, r registry.Replica, class string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.classes = append(f.classes, class)
	_ = f.h.plane.rec("Replica " + r.Identifier)
	return f.failing[r.Identifier]
}

func (f *fakeFleet) Failed(_ context.Context, r registry.Replica, cause error) {
	f.failed = append(f.failed, r.Identifier+": "+cause.Error())
}

func replicaHarness(t *testing.T) (*harness, *registry.Project, *fakeFleet, []string) {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)
	h.node(32, 8, 3)
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	n3 := &registry.Node{Name: "third", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n3); err != nil {
		t.Fatal(err)
	}
	p := h.create(t)
	fl := &fakeFleet{h: h, failing: map[string]error{}}
	h.e.opts.Replicas = fl
	var ids []string
	for i, n := range []string{n2.ID, n3.ID} {
		id := registry.ReplicaIdentifier(p.Ref, "us-east-1", "abc12"+string(rune('0'+i)))
		if err := h.reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: p.Ref, NodeID: n}); err != nil {
			t.Fatal(err)
		}
		if err := h.reg.SetReplicaStatus(ctx, id, string(registry.StatusActiveHealthy), registry.ReplicaStepDone, ""); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	h.plane.calls = nil
	return h, p, fl, ids
}

func TestResizeRestartsReplicasBeforeAGrowingPrimary(t *testing.T) {
	h, p, fl, ids := replicaHarness(t)
	if _, err := h.e.Resize(context.Background(), p.Ref, "small"); err != nil {
		t.Fatal(err)
	}
	want := "Replica " + ids[0] + ",Replica " + ids[1] + ",Stop " + p.Ref + ",Start " + p.Ref
	if got := strings.Join(h.plane.calls, ","); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	if strings.Join(fl.classes, ",") != "small,small" {
		t.Fatalf("classes = %v", fl.classes)
	}
	for _, id := range ids {
		if r, _ := h.reg.GetReplica(context.Background(), id); r.Status != string(registry.StatusActiveHealthy) {
			t.Fatalf("replica %s is %s", id, r.Status)
		}
	}
}

func TestResizeRestartsReplicasAfterAShrinkingPrimary(t *testing.T) {
	h, p, _, ids := replicaHarness(t)
	if _, err := h.e.Resize(context.Background(), p.Ref, "large"); err != nil {
		t.Fatal(err)
	}
	h.plane.calls = nil
	if _, err := h.e.Resize(context.Background(), p.Ref, "small"); err != nil {
		t.Fatal(err)
	}
	want := "Stop " + p.Ref + ",Start " + p.Ref + ",Replica " + ids[0] + ",Replica " + ids[1]
	if got := strings.Join(h.plane.calls, ","); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
}

func TestAReplicaThatDoesNotComeBackDoesNotStopThePrimary(t *testing.T) {
	ctx := context.Background()
	h, p, fl, ids := replicaHarness(t)
	fl.failing[ids[0]] = errors.New("node n2 is unreachable")
	// A replica still being set up is left alone.
	if err := h.reg.SetReplicaStatus(ctx, ids[1], registry.ReplicaInit, "3_initiated_read_replica_setup", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Resize(ctx, p.Ref, "small"); err != nil {
		t.Fatalf("the resize failed because of a replica: %v", err)
	}
	if got := strings.Join(h.plane.calls, ","); got != "Replica "+ids[0]+",Stop "+p.Ref+",Start "+p.Ref {
		t.Fatalf("calls = %s", got)
	}
	if r, _ := h.reg.GetReplica(ctx, ids[0]); r.Status != string(registry.StatusActiveUnhealthy) {
		t.Fatalf("the replica that failed is %s", r.Status)
	}
	if r, _ := h.reg.GetReplica(ctx, ids[1]); r.Status != registry.ReplicaInit {
		t.Fatalf("the replica in setup is %s", r.Status)
	}
	if len(fl.failed) != 1 || !strings.Contains(fl.failed[0], "unreachable") {
		t.Fatalf("failed = %v", fl.failed)
	}
	if ev := h.events(t, p.Ref); !strings.Contains(ev, EventReplicaResizeFailed) || !strings.Contains(ev, EventResized) {
		t.Fatalf("events = %s", ev)
	}
}

func TestAFailedGrowRestoresTheReplicasToTheOldSize(t *testing.T) {
	h, p, fl, _ := replicaHarness(t)
	h.plane.failOnce["Start"] = errors.New("start: boom")
	if _, err := h.e.Resize(context.Background(), p.Ref, "small"); err == nil {
		t.Fatal("the resize succeeded")
	}
	// The replicas grew first and went back when the primary did not follow.
	var replicaCalls []string
	for _, c := range h.plane.calls {
		if strings.HasPrefix(c, "Replica ") {
			replicaCalls = append(replicaCalls, c)
		}
	}
	if len(replicaCalls) != 4 {
		t.Fatalf("replica calls = %v (all calls %v)", replicaCalls, h.plane.calls)
	}
	if got := strings.Join(fl.classes, ","); got != "small,small,"+DefaultClass+","+DefaultClass {
		t.Fatalf("classes = %s", got)
	}
}

func TestResizeWithoutAReplicaFleetIsUnchanged(t *testing.T) {
	h := newHarness(t)
	h.node(16, 4, 3)
	p := h.create(t)
	h.plane.calls = nil
	if _, err := h.e.Resize(context.Background(), p.Ref, "small"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.plane.calls, ","); got != "Stop "+p.Ref+",Start "+p.Ref {
		t.Fatalf("calls = %s", got)
	}
}
