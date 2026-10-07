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

// KeepRecord removes everything but the registry row, which ends INIT_FAILED; Recreate builds
// a new cluster over that row, with the same identity. A reset of a branch relies on both.
func TestKeepRecordAndRecreate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.create(t)
	info := &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "feature", State: registry.BranchCreatingProject}
	const ref = "bbbbbbbbbbbbbbbbbbbb"
	kid, err := h.e.Create(ctx, CreateRequest{Name: "feature", Ref: ref, Class: "micro", Branch: info})
	if err != nil {
		t.Fatal(err)
	}
	// Recreate needs a row that is INIT_FAILED, not a running project.
	if _, err := h.e.Create(ctx, CreateRequest{Ref: ref, Class: "micro", Recreate: true}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("recreate of a running project: %v", err)
	}
	if err := h.e.DeleteWith(ctx, ref, DeleteOptions{SkipFinalBackup: true, KeepRecord: true}); err != nil {
		t.Fatal(err)
	}
	kept, err := h.reg.GetProject(ctx, ref)
	if err != nil || kept.Status != registry.StatusInitFailed || kept.Branch == nil || kept.Branch.ID != info.ID || kept.Seq != kid.Seq {
		t.Fatalf("the kept row = %+v %v", kept, err)
	}
	if _, err := h.e.Create(ctx, CreateRequest{Ref: "cccccccccccccccccccc", Class: "micro", Recreate: true}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("recreate of a missing row: %v", err)
	}
	again, err := h.e.Create(ctx, CreateRequest{Name: "ignored", Ref: ref, Class: "small", Recreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != registry.StatusActiveHealthy || again.Name != "feature" || again.Class != "small" || again.Seq != kid.Seq || again.Branch == nil || again.Branch.ID != info.ID {
		t.Fatalf("recreated = %+v", again)
	}
	// A recreated project deletes like any other.
	if err := h.e.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.GetProject(ctx, ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
}
