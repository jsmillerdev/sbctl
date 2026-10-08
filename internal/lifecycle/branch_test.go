package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
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

// Delete and a branch create race on different locks. Either order ends with no branch on a
// deleted parent: a create that finds the parent GOING_DOWN removes its row, and a delete that
// finds a branch row after flipping the status backs out.
func TestBranchCreateRefusesAParentGoingDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.create(t)
	if err := h.reg.SetProjectStatus(ctx, parent.Ref, registry.StatusGoingDown); err != nil {
		t.Fatal(err)
	}
	const ref = "bbbbbbbbbbbbbbbbbbbb"
	_, err := h.e.Create(ctx, CreateRequest{Name: "late", Ref: ref, Class: "micro", Branch: &registry.BranchInfo{
		ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "late", State: registry.BranchCreatingProject}})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("create on a parent going down: %v", err)
	}
	if _, err := h.reg.GetProject(ctx, ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the refused branch left a row: %v", err)
	}
	if len(h.plane.calls) != 0 && strings.Contains(strings.Join(h.plane.calls, ","), ref) {
		t.Fatalf("units were made for a refused branch: %v", h.plane.calls)
	}
}

// goingDownRegistry runs a hook the moment a project is set GOING_DOWN, standing for a branch create
// that inserts its row between Delete's first look at the branches and its status change.
type goingDownRegistry struct {
	registry.Registry
	onGoingDown func()
}

func (r *goingDownRegistry) SetProjectStatus(ctx context.Context, ref string, st registry.Status) error {
	err := r.Registry.SetProjectStatus(ctx, ref, st)
	if err == nil && st == registry.StatusGoingDown && r.onGoingDown != nil {
		f := r.onGoingDown
		r.onGoingDown = nil
		f()
	}
	return err
}

func TestDeleteBacksOutWhenABranchLandsAfterTheFirstLook(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	parent := h.create(t)
	const ref = "bbbbbbbbbbbbbbbbbbbb"
	rr := &goingDownRegistry{Registry: h.reg}
	rr.onGoingDown = func() {
		org, _ := h.reg.GetOrganization(ctx, "default")
		_ = h.reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: "late", Class: "micro", Status: registry.StatusComingUp,
			Branch: &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "late", State: registry.BranchCreatingProject}})
	}
	h.e.reg = rr
	callsBefore := len(h.plane.calls)
	err := h.e.Delete(ctx, parent.Ref)
	if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), ref) {
		t.Fatalf("delete with a branch that landed late: %v", err)
	}
	if len(h.plane.calls) != callsBefore || len(h.backup.calls) != 0 {
		t.Fatalf("the refused delete did work: calls=%v backups=%v", h.plane.calls[callsBefore:], h.backup.calls)
	}
	if p, err := h.reg.GetProject(ctx, parent.Ref); err != nil || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("parent after the refused delete: %+v %v", p, err)
	}
}
