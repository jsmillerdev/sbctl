package replicaid

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

func newReg(t *testing.T) *registry.Memory {
	t.Helper()
	reg := registry.NewMemory()
	ctx := context.Background()
	if err := reg.UpdateNode(ctx, &registry.Node{ID: "n1", Name: "primary", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateNode(ctx, &registry.Node{ID: "n2", Name: "eu", Region: "eu-west-1", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateProject(ctx, &registry.Project{Ref: "system", Name: "system", Class: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func node(t *testing.T, reg registry.Registry, id string) registry.Node {
	t.Helper()
	n, err := reg.GetNode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *n
}

// The join records the row for the node's standby of the system cluster; calling it again for the
// same node (a resumed join) returns the same row.
func TestEnsureSystem(t *testing.T) {
	ctx := context.Background()
	reg := newReg(t)
	r, err := EnsureSystem(ctx, reg, node(t, reg, "n2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Identifier, "system-rr-eu-west-1-") || !registry.ValidReplicaIdentifier(r.Identifier) {
		t.Fatalf("identifier %q", r.Identifier)
	}
	if r.Ref != "system" || r.NodeID != "n2" || r.Origin != registry.ReplicaSystem || r.Status != registry.ReplicaInit || r.InitStep != registry.ReplicaStepRequested {
		t.Fatalf("row: %+v", r)
	}
	again, err := EnsureSystem(ctx, reg, node(t, reg, "n2"), nil)
	if err != nil || again.Identifier != r.Identifier {
		t.Fatalf("second call: %+v %v", again, err)
	}
	if rs, _ := reg.ListReplicas(ctx, "system"); len(rs) != 1 {
		t.Fatalf("rows: %+v", rs)
	}
	if _, err := EnsureSystem(ctx, reg, node(t, reg, "n1"), nil); err == nil {
		t.Fatal("a standby of the system cluster on the leader itself")
	}
	// A node that names no region is in the default one.
	if err := reg.CreateNode(ctx, &registry.Node{ID: "n5", Name: "bare", State: registry.NodeJoining}); err != nil {
		t.Fatal(err)
	}
	if r, err := EnsureSystem(ctx, reg, node(t, reg, "n5"), nil); err != nil || !strings.HasPrefix(r.Identifier, "system-rr-us-east-1-") {
		t.Fatalf("no region: %+v %v", r, err)
	}
}

// Two replicas that draw the same six characters get different identifiers, and a node that holds
// a replica of the project already is a conflict, not a clash.
func TestCreateRetriesAClashAndReportsAConflict(t *testing.T) {
	ctx := context.Background()
	reg := newReg(t)
	ids := []string{"aaaaaa", "aaaaaa", "bbbbbb"}
	short := func() string { id := ids[0]; ids = ids[1:]; return id }
	if err := reg.CreateNode(ctx, &registry.Node{ID: "n3", Name: "eu-b", Region: "eu-west-1", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, reg, "system", node(t, reg, "n2"), registry.ReplicaDefault, short); err != nil {
		t.Fatal(err)
	}
	r, err := Create(ctx, reg, "system", node(t, reg, "n3"), registry.ReplicaDefault, short)
	if err != nil || r.Identifier != "system-rr-eu-west-1-bbbbbb" {
		t.Fatalf("second: %+v %v", r, err)
	}
	if _, err := Create(ctx, reg, "system", node(t, reg, "n3"), registry.ReplicaDefault, func() string { return "cccccc" }); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("a second replica on one node: %v", err)
	}
}

func TestShortIdentifiersAreValid(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := registry.ReplicaIdentifier("aaaaaaaaaaaaaaaaaaaa", "eu-west-1", Short())
		if !registry.ValidReplicaIdentifier(id) {
			t.Fatalf("invalid identifier %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d different identifiers in 200 draws", len(seen))
	}
}

func TestRegionDefaults(t *testing.T) {
	for in, want := range map[string]string{"": "us-east-1", "eu-west-1": "eu-west-1"} {
		if got := Region(registry.Node{Region: in}); got != want {
			t.Errorf("Region(%q) = %q, want %q", in, got, want)
		}
	}
}
