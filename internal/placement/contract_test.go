package placement

import (
	"context"
	"errors"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

func TestRegistryResolver(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	const ref = "aaaaaaaaaaaaaaaaaaaa"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	r := RegistryResolver{Reg: reg}
	if home, err := r.HomeOf(ctx, ref); err != nil || home != "n1" {
		t.Fatalf("home: %q, %v", home, err)
	}
	if _, err := r.HomeOf(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}
	if role, _ := r.RoleOf(ctx, ref, "n1"); role != RolePrimary {
		t.Fatalf("role on the home: %q", role)
	}
	if role, _ := r.RoleOf(ctx, ref, n2.ID); role != RoleNone {
		t.Fatalf("role before a replica: %q", role)
	}
	id := registry.ReplicaIdentifier(ref, "us-east-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: n2.ID}); err != nil {
		t.Fatal(err)
	}
	if role, _ := r.RoleOf(ctx, ref, n2.ID); role != RoleReplica {
		t.Fatalf("role on a replica node: %q", role)
	}
	if rs, err := r.ReplicasOf(ctx, ref); err != nil || len(rs) != 1 || rs[0].Identifier != id {
		t.Fatalf("replicas: %+v, %v", rs, err)
	}
	if _, err := r.RoleOf(ctx, "zzzzzzzzzzzzzzzzzzzz", "n1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("role of an unknown ref: %v", err)
	}
}
