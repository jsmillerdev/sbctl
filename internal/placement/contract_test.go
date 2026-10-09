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
	r := RegistryResolver{Reg: reg}
	if home, err := r.HomeOf(ctx, ref); err != nil || home != "n1" {
		t.Fatalf("home: %q, %v", home, err)
	}
	if _, err := r.HomeOf(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}
}
