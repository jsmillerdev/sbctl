package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/registry"
)

// A branch is a project created with CreateRequest.Branch: the registry row carries the
// parent from the moment it exists, and a parent cannot be deleted while it has branches.
func TestBranchCreateAndParentDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.create(t)
	kid, err := h.e.Create(ctx, CreateRequest{Name: "feature", Ref: "bbbbbbbbbbbbbbbbbbbb", Class: "micro", Branch: &registry.BranchInfo{
		ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "feature", State: registry.BranchCreatingProject}})
	if err != nil {
		t.Fatal(err)
	}
	if kid.Branch == nil || kid.Branch.ParentRef != parent.Ref || kid.Branch.Name != "feature" {
		t.Fatalf("branch = %+v", kid)
	}
	got, _ := h.reg.GetProject(ctx, kid.Ref)
	if got.Branch == nil || got.Branch.ID != "6f9619ff-8b86-4011-b42d-00c04fc964ff" {
		t.Fatalf("stored branch = %+v", got.Branch)
	}

	// The refused delete touches nothing: no backup, no unit removed, the row stays.
	callsBefore := len(h.plane.calls)
	err = h.e.Delete(ctx, parent.Ref)
	if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), kid.Ref) {
		t.Fatalf("delete of a parent with a branch: %v", err)
	}
	if len(h.plane.calls) != callsBefore || len(h.backup.calls) != 0 {
		t.Fatalf("the refused delete did work: calls=%v backups=%v", h.plane.calls[callsBefore:], h.backup.calls)
	}
	if p, err := h.reg.GetProject(ctx, parent.Ref); err != nil || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("parent after the refused delete: %+v %v", p, err)
	}

	if err := h.e.Delete(ctx, kid.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Delete(ctx, parent.Ref); err != nil {
		t.Fatalf("delete of the parent once its branches are gone: %v", err)
	}
}
