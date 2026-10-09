package app

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// The health checks of a node in a cluster see the projects homed on it, and no others.
func TestNodeSeenByListsTheProjectsHomedHere(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	if err := reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []registry.Project{
		{Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "here", NodeID: "n2"},
		{Ref: "bbbbbbbbbbbbbbbbbbbb", Name: "there", NodeID: "n1"},
		{Ref: "cccccccccccccccccccc", Name: "unhomed", NodeID: ""},
	} {
		p := p
		if err := reg.CreateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	node := &lifecycle.Node{Registry: reg}
	view := nodeSeenBy(node, cluster.BootDecision{Joined: true, SelfID: "n2"})
	ps, err := view.Registry.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, p := range ps {
		refs = append(refs, p.Name)
	}
	// (A project created with no home is homed on the leader, so the third is n1's.)
	if len(refs) != 1 || refs[0] != "here" {
		t.Fatalf("projects seen: %v", refs)
	}
	// The registry itself is untouched, and a project is still found by its ref.
	if all, _ := node.Registry.ListProjects(ctx); len(all) != 3 {
		t.Fatalf("the node's registry lists %d projects", len(all))
	}
	if _, err := view.Registry.GetProject(ctx, "bbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("GetProject through the view: %v", err)
	}
	// A server with no cluster is returned as it is.
	if got := nodeSeenBy(node, cluster.BootDecision{}); got != node {
		t.Fatal("a single server got a view of its registry")
	}
}

// A follower opens its registry read-only, through the socket that answered the role probe.
func TestOpenFollowerAsksForAReadOnlyNode(t *testing.T) {
	var lo lifecycle.OpenOptions
	if err := openFollower(&lo, cluster.BootDecision{Role: cluster.RoleFollower, DSN: "postgres://standby"}); err != nil {
		t.Fatal(err)
	}
	if !lo.ReadOnly || lo.RegistryDSN != "postgres://standby" {
		t.Fatalf("options: %+v", lo)
	}
	lo = lifecycle.OpenOptions{RegistryDSN: "postgres://elsewhere"}
	_ = openFollower(&lo, cluster.BootDecision{DSN: "postgres://standby"})
	if lo.RegistryDSN != "postgres://elsewhere" {
		t.Fatal("the DSN an override named was replaced")
	}
}

// `supavise status` sees a node through the same view; a node whose id is not known is returned as it is.
func TestNodeAtHomeLeavesANodeWithoutAnIdAlone(t *testing.T) {
	n := &lifecycle.Node{Registry: registry.NewMemory()}
	if got := NodeAtHome(n); got != n {
		t.Fatal("a node with no engine got a view of its registry")
	}
}
