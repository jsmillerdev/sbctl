package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/registry"
)

// clusterRegistry is a two-server cluster: n1 ("primary" in the memory registry) leads and holds
// projects a and system, n2 holds b; a has a replica on n2 and b has one on n1.
func clusterRegistry(t *testing.T) (registry.Registry, []registry.Project) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	org, err := reg.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateNode(ctx, &registry.Node{ID: "n2", Name: "second", State: registry.NodeActive, Version: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []registry.Project{{Ref: "system", Name: "system"}, {Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "a"}, {Ref: "bbbbbbbbbbbbbbbbbbbb", Name: "b"}} {
		p.OrgID = org.ID
		if err := reg.CreateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetProjectNode(ctx, "bbbbbbbbbbbbbbbbbbbb", "n2", 1); err != nil {
		t.Fatal(err)
	}
	for _, r := range []registry.Replica{
		{Identifier: registry.ReplicaIdentifier("aaaaaaaaaaaaaaaaaaaa", "us-east-1", "aaaaaa"), Ref: "aaaaaaaaaaaaaaaaaaaa", NodeID: "n2", Origin: "manual", Status: "ACTIVE_HEALTHY"},
		{Identifier: registry.ReplicaIdentifier("bbbbbbbbbbbbbbbbbbbb", "us-east-1", "bbbbbb"), Ref: "bbbbbbbbbbbbbbbbbbbb", NodeID: "n1", Origin: "manual", Status: "ACTIVE_HEALTHY"},
	} {
		if err := reg.CreateReplica(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := reg.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return reg, rows
}

func refsOf(rows []registry.Project) []string {
	var out []string
	for _, p := range rows {
		out = append(out, p.Ref)
	}
	slices.Sort(out)
	return out
}

// On the leader the upgrade is of the projects homed there, with the system project; the standbys
// that other servers hold of them are what a release without WAL compatibility waits for.
func TestClusterViewOfTheLeader(t *testing.T) {
	reg, rows := clusterRegistry(t)
	cfg := config.Default()
	cfg.Node.Name = "primary"
	view, here, err := clusterView(context.Background(), cfg, reg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if view == nil || view.Self != "n1" || !view.Leader {
		t.Fatalf("view: %+v", view)
	}
	if got := refsOf(here); !slices.Equal(got, []string{"aaaaaaaaaaaaaaaaaaaa", "system"}) {
		t.Errorf("projects here: %v", got)
	}
	if !slices.Equal(view.Elsewhere, []string{"bbbbbbbbbbbbbbbbbbbb"}) {
		t.Errorf("elsewhere: %v", view.Elsewhere)
	}
	if len(view.Standbys) != 1 || view.Standbys[0].Node != "n2" || view.Standbys[0].Version != "v1.0.0" || !slices.Equal(view.Standbys[0].Refs, []string{"aaaaaaaaaaaaaaaaaaaa", "system"}) {
		t.Errorf("standbys: %+v (n2 follows the system cluster and holds a replica of a; its own b is not ours)", view.Standbys)
	}
}

// A follower has its own projects only, no system project, and the standbys that the leader holds.
func TestClusterViewOfAFollower(t *testing.T) {
	reg, rows := clusterRegistry(t)
	cfg := config.Default()
	cfg.Node.Name = "second"
	view, here, err := clusterView(context.Background(), cfg, reg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if view == nil || view.Self != "n2" || view.Leader {
		t.Fatalf("view: %+v", view)
	}
	if got := refsOf(here); !slices.Equal(got, []string{"bbbbbbbbbbbbbbbbbbbb"}) {
		t.Errorf("projects here: %v", got)
	}
	if len(view.Standbys) != 1 || view.Standbys[0].Node != "n1" || !slices.Equal(view.Standbys[0].Refs, []string{"bbbbbbbbbbbbbbbbbbbb"}) {
		t.Errorf("standbys: %+v (the leader holds a replica of b; the system cluster is not ours to lead)", view.Standbys)
	}
	if !slices.Equal(view.Elsewhere, []string{"aaaaaaaaaaaaaaaaaaaa"}) {
		t.Errorf("elsewhere: %v (the system project is the leader's and is not listed)", view.Elsewhere)
	}
}

// A single server, and a server the registry does not know in a cluster, are told apart: the first
// keeps every project, the second stops.
func TestClusterViewOfASingleServerAndOfAStranger(t *testing.T) {
	ctx := context.Background()
	single := registry.NewMemory()
	rows, _ := single.ListProjects(ctx)
	view, here, err := clusterView(ctx, config.Default(), single, rows)
	if err != nil || view != nil || len(here) != len(rows) {
		t.Errorf("single server: %+v %d %v", view, len(here), err)
	}

	reg, rows := clusterRegistry(t)
	cfg := config.Default()
	cfg.Node.Name = "nobody"
	if _, _, err := clusterView(ctx, cfg, reg, rows); err == nil {
		t.Error("a server the cluster registry does not know was taken for the founder")
	}
}

// nodesFail is a registry whose node list cannot be read.
type nodesFail struct {
	registry.Registry
	err error
}

func (n nodesFail) ListNodes(context.Context) ([]registry.Node, error) { return nil, n.err }

// Only a registry without the cluster tables reads as a single server. Any other failure to read the
// nodes is an error: a clustered server that took it for a single one would plan and roll out the
// projects homed on the others.
func TestClusterViewTreatsOnlyMissingTablesAsASingleServer(t *testing.T) {
	ctx := context.Background()
	reg, rows := clusterRegistry(t)
	cfg := config.Default()
	cfg.Node.Name = "primary"

	legacy := nodesFail{reg, fmt.Errorf("list nodes: %w", &pgconn.PgError{Code: "42P01", Message: `relation "supavise.nodes" does not exist`})}
	view, here, err := clusterView(ctx, cfg, legacy, rows)
	if err != nil || view != nil || len(here) != len(rows) {
		t.Errorf("a registry without the cluster tables: %+v %d %v", view, len(here), err)
	}

	for name, e := range map[string]error{
		"connection":            errors.New("write: connection reset by peer"),
		"permission":            &pgconn.PgError{Code: "42501", Message: "permission denied for table nodes"},
		"cancelled":             context.DeadlineExceeded,
		"text without the code": errors.New(`relation "supavise.nodes" does not exist`), // text alone is not the code
	} {
		view, here, err := clusterView(ctx, cfg, nodesFail{reg, e}, rows)
		if err == nil || view != nil || here != nil {
			t.Errorf("%s: a failed read gave %+v %d %v, want an error", name, view, len(here), err)
		}
	}
}

// A node that left no longer counts: what is left is a single server again.
func TestClusterViewIgnoresANodeThatLeft(t *testing.T) {
	ctx := context.Background()
	reg, rows := clusterRegistry(t)
	if err := reg.SetNodeState(ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Node.Name = "primary"
	view, here, err := clusterView(ctx, cfg, reg, rows)
	if err != nil || view != nil || len(here) != len(rows) {
		t.Errorf("after n2 left: %+v %d %v", view, len(here), err)
	}
}

// The DSN this run reads the registry through is a plain connection string (no pool size), whichever
// socket answers; with none answering it is the system cluster's own, so that the error is the one
// the commands have always shown.
func TestReadableRegistryDSNIsAPlainConnectionString(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got := readableRegistryDSN(ctx, cfg)
	if got != lifecycle.SystemSocketDSN(cfg, "supavise") || strings.Contains(got, "pool_max_conns") {
		t.Errorf("DSN = %q", got)
	}
}

// The pool size is taken off whatever it is, and the DSNs tried are the ones the node's other
// commands try for the registry (app.RegistryDSNs), in the same order.
func TestWithoutPool(t *testing.T) {
	for in, want := range map[string]string{
		"host=/run/x port=5433 user=supavise pool_max_conns=6":  "host=/run/x port=5433 user=supavise",
		"host=/run/x port=5433 user=supavise pool_max_conns=64": "host=/run/x port=5433 user=supavise",
		"host=/run/x port=5433 user=supavise":                   "host=/run/x port=5433 user=supavise",
	} {
		if got := withoutPool(in); got != want {
			t.Errorf("withoutPool(%q) = %q, want %q", in, got, want)
		}
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	dsns := app.RegistryDSNs(cfg)
	if len(dsns) != 2 || !strings.Contains(dsns[0], fmt.Sprintf("port=%d", cfg.Ports.SystemPostgres)) || withoutPool(dsns[1]) == withoutPool(dsns[0]) {
		t.Errorf("the system cluster's sockets: %v", dsns)
	}
}

// A follower that homes a project is refused by the plan its own registry produces, with the project
// named; the same server without a project upgrades, and a leader is not asked to move anything.
func TestUpgradePlanOfAFollowerFromItsRegistry(t *testing.T) {
	ctx := context.Background()
	reg, rows := clusterRegistry(t)
	cfg := config.Default()
	cfg.Node.Name = "second"
	view, here, err := clusterView(ctx, cfg, reg, rows)
	if err != nil {
		t.Fatal(err)
	}
	node := func(rows []registry.Project) *nodeupgrade.Node {
		old := map[string]string{"gotrue": "auth-v2.100.0-r1", "postgrest": "postgrest-v12.0-r0"}
		n := &nodeupgrade.Node{Version: "v1.0.0", Pins: old, Cluster: view, Verdict: nodeupgrade.VerdictHealthy,
			Projects: nodeupgrade.ProjectsOf(rows, func(string) time.Time { return time.Time{} })}
		for i := range n.Projects {
			n.Projects[i].Status = "ACTIVE_HEALTHY"
		}
		return n
	}
	to := &nodeupgrade.Info{Version: "v1.1.0", Pins: map[string]string{"gotrue": "auth-v2.195.0-r1", "postgrest": "postgrest-v16.4-r0"}}

	p := nodeupgrade.BuildPlan(node(here), to, nodeupgrade.PlanOptions{})
	if !strings.Contains(p.Refusal, "bbbbbbbbbbbbbbbbbbbb") || !strings.Contains(p.Refusal, "supavise projects failover") || p.Rollout() {
		t.Errorf("a follower that homes b: refusal %q, rollout %v", p.Refusal, p.Rollout())
	}
	p = nodeupgrade.BuildPlan(node(nil), to, nodeupgrade.PlanOptions{})
	if p.Refusal != "" || p.Rollout() || !p.NodeChanges() {
		t.Errorf("a follower with no project: refusal %q, rollout %v, node changes %v", p.Refusal, p.Rollout(), p.NodeChanges())
	}
}

// The worker that removes unused artifacts opens the registry for writing, so a follower leaves it
// out and says so; any other node runs it.
func TestCleanupSkipsArtifactGCOnAFollower(t *testing.T) {
	for _, follower := range []bool{false, true} {
		bin, record := stubBinary(t, "")
		h, out, _ := stubHost(bin)
		h.follower = follower
		h.rel = nodeupgrade.Releases{Dir: t.TempDir()}
		h.Cleanup(context.Background(), 3, "v1.1.0")
		got := calls(t, record)
		if follower && (got != "" || !strings.Contains(out.String(), "not removed on a follower")) {
			t.Errorf("follower: calls %q, output %q", got, out.String())
		}
		if !follower && !strings.HasSuffix(got, "artifacts gc --keep 3") {
			t.Errorf("leader: calls %q", got)
		}
	}
}
