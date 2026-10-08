package proxy

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// bareTable is a table over a Memory registry that nothing subscribes to, so that a test decides which
// change the table hears about.
func bareTable(t *testing.T) (*table, *registry.Memory) {
	t.Helper()
	cfg := config.Default()
	cfg.Domain = testDomain
	reg := registry.NewMemory()
	return newTable(cfg, reg, newFakeKeys(), quietLog()), reg
}

func (t *table) replicaIDs(ref string) []string {
	var ids []string
	for _, r := range t.project(ref).replicas {
		ids = append(ids, r.identifier+"="+r.status)
	}
	return ids
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTableAppliesReloadOps: a registry opened read-only cannot tell which row changed, so it sends
// Op "reload" with no key for each table, and the table reads that table again.
func TestTableAppliesReloadOps(t *testing.T) {
	ctx := context.Background()
	tb, reg := bareTable(t)
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	addNode(t, reg, "n2", "eu-1", registry.NodeJoining)
	addReplica(t, reg, repEU, testRef, "n2", string(registry.StatusActiveHealthy))
	if err := reg.PutRoute(ctx, registry.Route{Host: "docs.customer.example", Ref: testRef, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}

	// Nothing is loaded until a table is told to reload; one table at a time.
	if _, ok := tb.lookup(testRef + ".api." + testDomain); ok {
		t.Fatal("a table that never loaded knows a project")
	}
	tb.apply(ctx, registry.Change{Table: "projects", Op: "reload"})
	if p := tb.project(testRef); p.ref != testRef || p.home != "n1" || p.status != registry.StatusActiveHealthy || len(p.replicas) != 0 {
		t.Fatalf("after the projects reload: %+v", p)
	}
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "reload"})
	if got := tb.replicaIDs(testRef); !eq(got, []string{repEU + "=ACTIVE_HEALTHY"}) {
		t.Fatalf("after the replicas reload: %v", got)
	}
	if tb.nodeActive("n2") {
		t.Error("a node the table has not read is active")
	}
	tb.apply(ctx, registry.Change{Table: "nodes", Op: "reload"})
	if tb.nodeActive("n2") || !tb.nodeActive("n1") {
		t.Errorf("after the nodes reload: n1 %v n2 %v", tb.nodeActive("n1"), tb.nodeActive("n2"))
	}
	if tb.routeKind("docs.customer.example") != "" {
		t.Error("a route is known before the routes reload")
	}
	tb.apply(ctx, registry.Change{Table: "routes", Op: "reload"})
	if tb.routeKind("docs.customer.example") != kindCustom {
		t.Error("the routes reload did not load the route")
	}

	// A second reload sees the changes made since: status, node state, a new replica, a removed one.
	if err := reg.SetNodeState(ctx, "n2", registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetReplicaStatus(ctx, repEU, registry.ReplicaInit, "1_started", ""); err != nil {
		t.Fatal(err)
	}
	addNode(t, reg, "n3", "us-1", registry.NodeActive)
	addReplica(t, reg, repUS, testRef, "n3", string(registry.StatusActiveHealthy))
	if err := reg.SetProjectStatus(ctx, testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "replicas", "nodes"} {
		tb.apply(ctx, registry.Change{Table: table, Op: "reload"})
	}
	if got := tb.replicaIDs(testRef); !eq(got, []string{repEU + "=INIT_READ_REPLICA", repUS + "=ACTIVE_HEALTHY"}) {
		t.Errorf("replicas %v", got)
	}
	if !tb.nodeActive("n2") || !tb.nodeActive("n3") {
		t.Error("node states not reloaded")
	}
	if p := tb.project(testRef); p.status != registry.StatusInactive || len(p.replicas) != 2 {
		t.Errorf("project %+v", p)
	}
	if err := reg.DeleteReplica(ctx, repEU); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "reload"})
	if got := tb.replicaIDs(testRef); !eq(got, []string{repUS + "=ACTIVE_HEALTHY"}) {
		t.Errorf("after a delete: %v", got)
	}
	// A project that is gone leaves with its replicas' endpoints.
	if err := reg.DeleteProject(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "projects", Op: "reload"})
	if _, ok := tb.lookup(testRef + ".api." + testDomain); ok {
		t.Error("a deleted project is still routed")
	}
	if tb.routeKind(repUS+".api."+testDomain) != "" || tb.routeKind(testRef+"-lb.api."+testDomain) != "" {
		t.Error("a deleted project's replica endpoint or balancer is still routed")
	}
}

// TestTableReloadOpDropsKeys: Op "reload" on project_secrets drops every cached key, as a full reload does.
func TestTableReloadOpDropsKeys(t *testing.T) {
	ctx := context.Background()
	tb, _ := bareTable(t)
	keys := tb.keys.(*fakeKeys)
	keys.set(testRef, testKeys(t, testRef))
	var dropped []string
	tb.onKeysDropped = func(ref string) { dropped = append(dropped, ref) }
	for i := 0; i < 3; i++ {
		if _, err := tb.projectKeys(ctx, testRef); err != nil {
			t.Fatal(err)
		}
	}
	if keys.callCount(testRef) != 1 {
		t.Fatalf("keys fetched %d times, want 1 (cached)", keys.callCount(testRef))
	}
	tb.apply(ctx, registry.Change{Table: "project_secrets", Op: "reload"})
	if _, err := tb.projectKeys(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if keys.callCount(testRef) != 2 {
		t.Errorf("keys fetched %d times after the reload, want 2", keys.callCount(testRef))
	}
	if len(dropped) != 1 || dropped[0] != "" {
		t.Errorf("onKeysDropped told %q, want one call for all projects", dropped)
	}
}

// TestTableAppliesNodeAndReplicaRows: the events of a registry that does name the row.
func TestTableAppliesNodeAndReplicaRows(t *testing.T) {
	ctx := context.Background()
	tb, reg := bareTable(t)
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "projects", Op: "insert", Key: testRef})
	addNode(t, reg, "n2", "eu-1", registry.NodeActive)
	tb.apply(ctx, registry.Change{Table: "nodes", Op: "insert", Key: "n2"})
	if !tb.nodeActive("n2") {
		t.Error("node row not applied")
	}
	addReplica(t, reg, repEU, testRef, "n2", registry.ReplicaInit)
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "insert", Key: repEU})
	if got := tb.replicaIDs(testRef); !eq(got, []string{repEU + "=INIT_READ_REPLICA"}) {
		t.Fatalf("after an insert: %v", got)
	}
	if err := reg.SetReplicaStatus(ctx, repEU, string(registry.StatusActiveHealthy), registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "update", Key: repEU})
	if got := tb.replicaIDs(testRef); !eq(got, []string{repEU + "=ACTIVE_HEALTHY"}) {
		t.Errorf("after an update: %v", got)
	}
	// The project row arrives after its replica's: the replica is attached when the project is.
	other := "zzzzzzzzzzzzzzzzzzzz"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: other, Name: "q", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	id := other + "-rr-eu-aaaaaa"
	addReplica(t, reg, id, other, "n2", registry.ReplicaInit)
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "insert", Key: id})
	tb.apply(ctx, registry.Change{Table: "projects", Op: "insert", Key: other})
	if got := tb.replicaIDs(other); !eq(got, []string{id + "=INIT_READ_REPLICA"}) {
		t.Errorf("replica of a project that arrived later: %v", got)
	}
	if err := reg.DeleteReplica(ctx, repEU); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "delete", Key: repEU})
	if got := tb.replicaIDs(testRef); len(got) != 0 {
		t.Errorf("after a delete: %v", got)
	}
	// An update for a row that is gone removes it.
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "update", Key: id + "x"})
	if err := reg.SetNodeState(ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	tb.apply(ctx, registry.Change{Table: "nodes", Op: "update", Key: "n2"})
	if tb.nodeActive("n2") {
		t.Error("a node that left is active")
	}
	tb.apply(ctx, registry.Change{Table: "nodes", Op: "delete", Key: "n2"})
	if tb.nodeActive("n2") {
		t.Error("a deleted node is active")
	}
}

// TestSystemStandbyHasNoEndpoint: the standby of the system cluster is a replica row of the system
// project; neither is routable.
func TestSystemStandbyHasNoEndpoint(t *testing.T) {
	ctx := context.Background()
	tb, reg := bareTable(t)
	if err := reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	addNode(t, reg, "n2", "eu-1", registry.NodeActive)
	id := "system-rr-eu-abc123"
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: config.SystemRef, NodeID: "n2", Origin: registry.ReplicaSystem}); err != nil {
		t.Fatal(err)
	}
	if err := tb.reload(ctx); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{id + ".api." + testDomain, "system-lb.api." + testDomain, "system.api." + testDomain} {
		if tb.routeKind(host) != "" {
			t.Errorf("%s is routed", host)
		}
	}
	tb.apply(ctx, registry.Change{Table: "replicas", Op: "insert", Key: id})
	if len(tb.replicas) != 0 {
		t.Error("the system standby entered the table")
	}
}
