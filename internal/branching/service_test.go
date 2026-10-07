package branching

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
)

func TestCreateSchemaOnlyReplaysMigrationsAndSeed(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.svc.SetSeed(ctx, parentRef, "insert into t values (1)"); err != nil {
		t.Fatal(err)
	}
	b := h.create("feature/login", func(in *CreateInput) { in.GitBranch = "feature/login" })
	h.mustState(b, registry.BranchMigrationsPassed)
	if b.WithData || b.CloneMethod != MethodSchema || b.ParentRef != parentRef || b.GitBranch != "feature/login" || b.Persistent {
		t.Fatalf("branch = %+v", b)
	}
	if got := h.db.applied[b.Ref]; len(got) != 3 || got[0] != "20260101000000" || got[2] != "20260103000000" {
		t.Fatalf("replayed %v", got)
	}
	if got := h.db.scripts[b.Ref]; len(got) != 1 || got[0] != "insert into t values (1)" {
		t.Fatalf("seed ran as %v", got)
	}
	if !strings.Contains(b.Detail, "3 migration(s)") || !strings.Contains(b.Detail, "seed") {
		t.Fatalf("detail = %q", b.Detail)
	}
	// The default 7 day lifetime starts when the branch is ready.
	if b.ExpiresAt == nil || !b.ExpiresAt.Equal(h.clock().Add(7*24*time.Hour)) {
		t.Fatalf("expires at %v", b.ExpiresAt)
	}
	req := h.eng.creates[0]
	if req.Class != "micro" || req.Branch == nil || req.Branch.ParentRef != parentRef || req.Seed != nil || req.Name != "feature/login" {
		t.Fatalf("create request = %+v", req)
	}

	list, err := h.svc.List(ctx, parentRef)
	if err != nil || len(list) != 2 || !list[0].IsDefault || list[0].Name != "main" || list[0].Ref != parentRef || list[1].Name != "feature/login" {
		t.Fatalf("list = %+v %v", list, err)
	}
	// Listing through the branch's own ref gives the family.
	list2, err := h.svc.List(ctx, b.Ref)
	if err != nil || len(list2) != 2 {
		t.Fatalf("list via branch ref: %+v %v", list2, err)
	}
	for _, key := range []string{b.ID, b.Ref} {
		got, err := h.svc.Resolve(ctx, key)
		if err != nil || got.Name != "feature/login" {
			t.Fatalf("resolve %s: %+v %v", key, got, err)
		}
	}
	if got, err := h.svc.Get(ctx, parentRef, "feature/login"); err != nil || got.ID != b.ID {
		t.Fatalf("get: %+v %v", got, err)
	}
	if def, err := h.svc.Get(ctx, parentRef, "main"); err != nil || !def.IsDefault || def.ID != defaultBranchID(parentRef) {
		t.Fatalf("get main: %+v %v", def, err)
	}
	if def, err := h.svc.Resolve(ctx, defaultBranchID(parentRef)); err != nil || !def.IsDefault {
		t.Fatalf("resolve default id: %+v %v", def, err)
	}
	if _, err := h.svc.Get(ctx, parentRef, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// The parent saw the creation.
	evs, _ := h.reg.ListEvents(ctx, parentRef, 10)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if !strings.Contains(strings.Join(kinds, " "), "branch.created") || !strings.Contains(strings.Join(kinds, " "), "branch.create.succeeded") {
		t.Fatalf("parent events = %v", kinds)
	}
}

func TestCreateReturnsBeforeTheWorkIsDone(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.hold = make(chan struct{})
	b, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchCreatingProject || b.ProjectStatus != registry.StatusComingUp {
		t.Fatalf("early view = %+v", b)
	}
	// A second operation on it is refused while it is busy.
	if _, err := h.svc.Delete(context.Background(), b.Ref, DeleteOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete while creating: %v", err)
	}
	if _, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("merge while creating: %v", err)
	}
	close(h.eng.hold)
	done := h.wait(b.Ref)
	h.mustState(done, registry.BranchMigrationsPassed)
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Branching.MaxPerProject = 2 })
	ctx := context.Background()
	for _, name := range []string{"", "main", "a b", "../x", "-x", "x/", strings.Repeat("a", 101)} {
		if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: name}); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "x", IsDefault: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("is_default: %v", err)
	}
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "x", NotifyURL: "ftp://x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("notify url: %v", err)
	}
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "x", DesiredInstanceSize: "gigantic"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("size: %v", err)
	}
	if _, err := h.svc.Create(ctx, "system", CreateInput{Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("system: %v", err)
	}
	if _, err := h.svc.Create(ctx, "zzzzzzzzzzzzzzzzzzzz", CreateInput{Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	a := h.create("a", nil)
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "a"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := h.svc.Create(ctx, a.Ref, CreateInput{Name: "child"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("branch of a branch: %v", err)
	}
	h.create("b", nil)
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "c"}); !errors.Is(err, ErrLimit) {
		t.Errorf("limit: %v", err)
	}
	// A parent that is not running cannot be branched.
	_ = h.reg.SetProjectStatus(ctx, parentRef, registry.StatusInactive)
	if _, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "d"}); !errors.Is(err, ErrConflict) {
		t.Errorf("paused parent: %v", err)
	}
}

func TestInstanceSizeMapsToClass(t *testing.T) {
	h := newHarness(t, nil)
	for size, want := range map[string]string{"": "micro", "pico": "micro", "nano": "micro", "micro": "micro", "small": "small", "medium": "medium", "large": "large", "2xlarge": "large", "24xlarge_optimized_memory": "large"} {
		got, err := h.svc.classFor(size)
		if err != nil || got != want {
			t.Errorf("classFor(%q) = %q, %v; want %q", size, got, err, want)
		}
		if _, err := lifecycle.ClassFor(got); err != nil {
			t.Errorf("class %q is not a lifecycle class: %v", got, err)
		}
	}
}

func TestPersistentBranchDoesNotExpire(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("keep", func(in *CreateInput) { in.Persistent = true })
	if b.ExpiresAt != nil || !b.Persistent {
		t.Fatalf("persistent branch = %+v", b)
	}
	e := h.create("short", func(in *CreateInput) { in.TTL = time.Hour })
	if e.ExpiresAt == nil || !e.ExpiresAt.Equal(h.clock().Add(time.Hour)) {
		t.Fatalf("ttl = %v", e.ExpiresAt)
	}
	n := h.create("never", func(in *CreateInput) { in.TTL = -1 })
	if n.ExpiresAt != nil {
		t.Fatalf("negative ttl should not expire: %v", n.ExpiresAt)
	}
}

func TestCreateFailureIsVisible(t *testing.T) {
	h := newHarness(t, nil)
	// Migration replay fails: the branch stays, MIGRATIONS_FAILED with the cause.
	h.db.applyErr["*"] = errors.New("syntax error at or near \"selec\"")
	got := h.create("bad", nil)
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "selec") || got.CloneMethod != MethodSchema {
		t.Fatalf("branch = %+v", got)
	}
}

func TestCreateEngineFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.createErr = errors.New("units did not start")
	b, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "broken"})
	if err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "units did not start") || got.ProjectStatus != registry.StatusInitFailed {
		t.Fatalf("branch = %+v", got)
	}
	// It can still be deleted.
	if _, err := h.svc.Delete(context.Background(), b.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestWithDataNeedsAWay(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.detect = func(string, string) (string, string, string) { return "", "ext4", "ext4 does not support file cloning" }
	_, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "d", WithData: true})
	if err == nil || !strings.Contains(err.Error(), "ext4 does not support file cloning") || !strings.Contains(err.Error(), "no backup backend") {
		t.Fatalf("err = %v", err)
	}
	// The user can fix it (pick a filesystem, configure a backup backend): a 400, not a 500.
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("with_data without a way is not ErrInvalid: %v", err)
	}
	if ps, _ := h.reg.ListProjects(context.Background()); len(ps) != 1 {
		t.Fatalf("a failed with_data left projects behind: %+v", ps)
	}
}

func TestWithDataCloneUsesParentCredentialsThenRotates(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.detect = func(string, string) (string, string, string) { return MethodClonefile, "apfs", "" }
	var cloned []string
	h.svc.clone = func(_ context.Context, parent, method, dst string) (*CloneStats, error) {
		cloned = append(cloned, parent+" "+method)
		return &CloneStats{Method: method, FS: "apfs", Files: 10, Bytes: 1 << 20, TotalMillis: 12, WALSegments: 2}, nil
	}
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsPassed)
	if !b.WithData || b.CloneMethod != MethodClonefile || !strings.Contains(b.Detail, "clonefile") {
		t.Fatalf("branch = %+v", b)
	}
	req := h.eng.creates[0]
	pk := h.eng.keys[parentRef]
	if req.Seed == nil || req.Keys == nil {
		t.Fatalf("a clone needs a seeder and keys: %+v", req)
	}
	// Role passwords (inside the cloned data) are the parent's until rotation; API keys are new.
	if req.Keys.DBPassword != pk.DBPassword || req.Keys.AdminPassword != pk.AdminPassword || req.Keys.PGSodiumRootKey != pk.PGSodiumRootKey {
		t.Fatal("clone keys must carry the parent's role passwords and pgsodium key")
	}
	if req.Keys.JWTSecret == pk.JWTSecret || req.Keys.PublishableKey == pk.PublishableKey || req.Keys.AnonKey == pk.AnonKey {
		t.Fatal("clone must get its own JWT secret and API keys")
	}
	if err := req.Seed(context.Background(), nil, "/x/data"); err != nil || len(cloned) != 1 || cloned[0] != parentRef+" clonefile" {
		t.Fatalf("seeder: %v %v", err, cloned)
	}
	if len(h.eng.rotated) != 1 || h.eng.rotated[0] != b.Ref {
		t.Fatalf("credentials were not rotated: %v", h.eng.rotated)
	}
	// With data there is no replay.
	if len(h.db.applied[b.Ref]) != 0 {
		t.Fatalf("migrations replayed into a clone: %v", h.db.applied[b.Ref])
	}
}

func TestFailedRotationStopsTheBranch(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.detect = func(string, string) (string, string, string) { return MethodClonefile, "apfs", "" }
	h.svc.clone = func(_ context.Context, _, method, _ string) (*CloneStats, error) {
		return &CloneStats{Method: method}, nil
	}
	h.svc.rotate = func(context.Context, string) error { return errors.New("socket gone") }
	b := h.create("leaky", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsFailed)
	if !strings.Contains(b.Detail, "socket gone") || !strings.Contains(b.Detail, "stopped") {
		t.Fatalf("detail = %q", b.Detail)
	}
	if h.eng.paused[b.Ref] != 1 {
		t.Fatalf("a branch that kept the parent's credentials must be stopped: %v", h.eng.paused)
	}
}

func TestMerge(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	ctx := context.Background()
	// The branch adds a migration; the parent gets it.
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	run, err := h.svc.Merge(ctx, b.Ref, ActionInput{})
	if err != nil || !IsUUID(run) {
		t.Fatalf("merge: %q %v", run, err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if v := h.db.applied[parentRef]; len(v) != 1 || v[0] != "20260110000000" || !h.db.atomic[parentRef] {
		t.Fatalf("applied to the parent: %v atomic=%v", v, h.db.atomic[parentRef])
	}
	if !strings.Contains(got.Detail, "merged 1 migration") {
		t.Fatalf("detail = %q", got.Detail)
	}
	// Merging again is a no-op.
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got = h.wait(b.Ref)
	if !strings.Contains(got.Detail, "nothing to merge") || len(h.db.applied[parentRef]) != 1 {
		t.Fatalf("second merge: %q %v", got.Detail, h.db.applied[parentRef])
	}
	// The default branch cannot be merged, reset, pushed or deleted.
	for name, f := range map[string]func() error{
		"merge":  func() error { _, err := h.svc.Merge(ctx, parentRef, ActionInput{}); return err },
		"reset":  func() error { _, err := h.svc.Reset(ctx, parentRef, ActionInput{}); return err },
		"push":   func() error { _, err := h.svc.Push(ctx, parentRef, ActionInput{}); return err },
		"delete": func() error { _, err := h.svc.Delete(ctx, parentRef, DeleteOptions{}); return err },
	} {
		if err := f(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s of the default branch: %v", name, err)
		}
	}
}

func TestMergeRefusesDivergence(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	ctx := context.Background()
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	// The parent moved on after the branch was made.
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "hotfix", "create table hf (id int)"))
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "diverged") || !strings.Contains(got.Detail, "20260105000000") {
		t.Fatalf("detail = %q", got.Detail)
	}
	if len(h.db.applied[parentRef]) != 0 {
		t.Fatal("a refused merge changed the parent")
	}
	// Forced, it applies the branch's migration anyway.
	if _, err := h.svc.Merge(ctx, b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	got = h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if v := h.db.applied[parentRef]; len(v) != 1 || v[0] != "20260110000000" {
		t.Fatalf("forced merge applied %v", v)
	}
}

func TestMergeUpToVersion(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "one", "select 1"), mig("20260111000000", "two", "select 2"))
	if _, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{MigrationVersion: "20260110000000"}); err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	if v := h.db.applied[parentRef]; len(v) != 1 || v[0] != "20260110000000" {
		t.Fatalf("applied %v", v)
	}
	if _, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{MigrationVersion: "29990101000000"}); err != nil {
		t.Fatal(err)
	}
	if got := h.wait(b.Ref); got.State != registry.BranchMigrationsFailed || !strings.Contains(got.Detail, "migration_version") {
		t.Fatalf("unknown version: %+v", got)
	}
}

func TestPush(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	ctx := context.Background()
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "branch work", "create table f (id int)"))
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "hotfix", "create table hf (id int)"))
	before := len(h.db.applied[b.Ref])
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if v := h.db.applied[b.Ref][before:]; len(v) != 1 || v[0] != "20260105000000" {
		t.Fatalf("pushed %v", v)
	}
	// A conflicting version stops a push, and force skips it.
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260110000000", "branch work", "create table other (id int)"), mig("20260106000000", "later", "select 1"))
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if got := h.wait(b.Ref); got.State != registry.BranchMigrationsFailed || !strings.Contains(got.Detail, "20260110000000") {
		t.Fatalf("conflict: %+v", got)
	}
	if _, err := h.svc.Push(ctx, b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchMigrationsPassed)
	if v := h.db.applied[b.Ref]; v[len(v)-1] != "20260106000000" {
		t.Fatalf("forced push applied %v", v)
	}
}

func TestResetSchemaOnlyKeepsRefIDAndKeys(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", func(in *CreateInput) { in.GitBranch = "g"; in.TTL = 5 * time.Hour })
	oldKeys, _ := h.eng.Keys(context.Background(), b.Ref)
	// The parent got a new migration since.
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "new", "select 1"))
	h.db.mu.Lock()
	delete(h.db.migs, b.Ref) // the engine recreated the cluster: empty history
	h.db.mu.Unlock()
	if _, err := h.svc.Reset(context.Background(), b.ID, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if got.Ref != b.Ref || got.ID != b.ID || got.Name != "feat" || got.GitBranch != "g" {
		t.Fatalf("reset changed identity: %+v vs %+v", got, b)
	}
	if h.eng.skipped[b.Ref] != true {
		t.Fatal("reset must not take a final backup")
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*b.ExpiresAt) {
		t.Fatalf("a reset keeps the branch's lifetime: %v -> %v", b.ExpiresAt, got.ExpiresAt)
	}
	nk, _ := h.eng.Keys(context.Background(), b.Ref)
	if nk.JWTSecret != oldKeys.JWTSecret || nk.PublishableKey != oldKeys.PublishableKey {
		t.Fatal("a schema-only reset keeps the branch's credentials")
	}
	if v := h.db.migs[b.Ref]; len(v) != 4 {
		t.Fatalf("history after reset: %v", v)
	}
	// Reset to a version replays only up to it.
	h.db.mu.Lock()
	delete(h.db.migs, b.Ref)
	h.db.mu.Unlock()
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "20260102000000"}); err != nil {
		t.Fatal(err)
	}
	h.mustState(h.wait(b.Ref), registry.BranchMigrationsPassed)
	if v := h.db.migs[b.Ref]; len(v) != 2 || v[1].Version != "20260102000000" {
		t.Fatalf("history after reset to a version: %v", v)
	}
}

func TestResetWithDataGetsNewCredentials(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.detect = func(string, string) (string, string, string) { return MethodReflink, "xfs", "" }
	h.svc.clone = func(_ context.Context, _, method, _ string) (*CloneStats, error) {
		return &CloneStats{Method: method}, nil
	}
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{MigrationVersion: "20260101000000"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("migration_version on a with_data branch: %v", err)
	}
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(b.Ref)
	h.mustState(got, registry.BranchMigrationsPassed)
	if !got.WithData || got.CloneMethod != MethodReflink || len(h.eng.creates) != 2 || h.eng.creates[1].Seed == nil {
		t.Fatalf("reset = %+v creates=%d", got, len(h.eng.creates))
	}
}

func TestUpdate(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a := h.create("a", nil)
	h.create("b", nil)
	name, git, yes, no := "renamed", "g2", true, false
	got, err := h.svc.Update(ctx, a.Ref, UpdateInput{Name: &name, GitBranch: &git, Persistent: &yes})
	if err != nil || got.Name != "renamed" || got.GitBranch != "g2" || !got.Persistent || got.ExpiresAt != nil {
		t.Fatalf("update: %+v %v", got, err)
	}
	if p, _ := h.reg.GetProject(ctx, a.Ref); p.Name != "renamed" {
		t.Fatalf("project name = %q", p.Name)
	}
	got, err = h.svc.Update(ctx, a.Ref, UpdateInput{Persistent: &no})
	if err != nil || got.Persistent || got.ExpiresAt == nil {
		t.Fatalf("make ephemeral again: %+v %v", got, err)
	}
	taken := "b"
	if _, err := h.svc.Update(ctx, a.Ref, UpdateInput{Name: &taken}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto an existing name: %v", err)
	}
	bad := "no good"
	if _, err := h.svc.Update(ctx, a.Ref, UpdateInput{Name: &bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	review := true
	if got, err := h.svc.Update(ctx, a.Ref, UpdateInput{RequestReview: &review}); err != nil || got.ReviewRequestedAt == nil {
		t.Fatalf("review: %+v %v", got, err)
	}
}

func TestDeleteEphemeralSkipsFinalBackupPersistentKeepsIt(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	e := h.create("eph", nil)
	p := h.create("keep", func(in *CreateInput) { in.Persistent = true })
	if _, err := h.svc.Delete(ctx, e.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Delete(ctx, p.ID, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !h.eng.skipped[e.Ref] || h.eng.skipped[p.Ref] {
		t.Fatalf("skip final backup: ephemeral=%v persistent=%v", h.eng.skipped[e.Ref], h.eng.skipped[p.Ref])
	}
	if list, _ := h.svc.List(ctx, parentRef); len(list) != 1 {
		t.Fatalf("list after delete: %+v", list)
	}
	if _, err := h.svc.Delete(ctx, e.Ref, DeleteOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("soft", func(in *CreateInput) { in.Persistent = true })
	got, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true})
	if err != nil || got.DeletionScheduledAt == nil || !got.DeletionScheduledAt.Equal(h.clock().Add(time.Hour)) {
		t.Fatalf("schedule: %+v %v", got, err)
	}
	if len(h.eng.deletes) != 0 {
		t.Fatal("a scheduled delete must not delete yet")
	}
	// Not due yet: the sweeper leaves it.
	if res, _ := h.svc.Sweep(ctx, false); len(res.Expired) != 0 {
		t.Fatalf("sweep early: %+v", res)
	}
	got, err = h.svc.Restore(ctx, b.Ref)
	if err != nil || got.DeletionScheduledAt != nil {
		t.Fatalf("restore: %+v %v", got, err)
	}
	if _, err := h.svc.Restore(ctx, b.Ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore twice: %v", err)
	}
	if _, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	h.advance(61 * time.Minute)
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Expired) != 1 || res.Expired[0] != b.Ref {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if _, err := h.svc.Resolve(ctx, b.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("branch still there: %v", err)
	}
	// A persistent branch's lapsed soft delete still takes its final backup.
	if h.eng.skipped[b.Ref] {
		t.Fatal("persistent branch deleted without its final backup")
	}
}

func TestSweepDeletesExpiredEphemeralBranches(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	old := h.create("old", func(in *CreateInput) { in.TTL = time.Hour })
	young := h.create("young", func(in *CreateInput) { in.TTL = 3 * time.Hour })
	keep := h.create("keep", func(in *CreateInput) { in.Persistent = true })
	h.advance(90 * time.Minute)
	dry, err := h.svc.Sweep(ctx, true)
	if err != nil || len(dry.Expired) != 1 || len(h.eng.deletes) != 0 {
		t.Fatalf("dry run: %+v %v deletes=%v", dry, err, h.eng.deletes)
	}
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Expired) != 1 || res.Expired[0] != old.Ref || len(res.Failed) != 0 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if !h.eng.skipped[old.Ref] {
		t.Fatal("the sweeper must skip the final backup of an ephemeral branch")
	}
	list, _ := h.svc.List(ctx, parentRef)
	if len(list) != 3 { // main, young, keep
		t.Fatalf("after sweep: %+v", list)
	}
	h.advance(24 * time.Hour)
	res, _ = h.svc.Sweep(ctx, false)
	if len(res.Expired) != 1 || res.Expired[0] != young.Ref {
		t.Fatalf("second sweep: %+v", res)
	}
	if _, err := h.svc.Resolve(ctx, keep.Ref); err != nil {
		t.Fatalf("persistent branch swept: %v", err)
	}
}

func TestSweepMarksInterruptedOperationsFailed(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("x", nil)
	// A crash left the branch busy: the row says so, but no process works on it.
	h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging", nil)
	h.advance(10 * time.Minute)
	if res, _ := h.svc.Sweep(ctx, false); len(res.Stale) != 0 {
		t.Fatalf("too early: %+v", res)
	}
	h.advance(time.Hour)
	res, _ := h.svc.Sweep(ctx, false)
	if len(res.Stale) != 1 {
		t.Fatalf("stale: %+v", res)
	}
	got, _ := h.svc.Resolve(ctx, b.Ref)
	h.mustState(got, registry.BranchMigrationsFailed)
}

func TestDisableBranching(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.create("a", nil)
	h.create("b", nil)
	if err := h.svc.DisableBranching(ctx, parentRef); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.svc.List(ctx, parentRef); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
}

func TestDiff(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create("feat", nil)
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int);", "create index on f (id)"))
	out, err := h.svc.Diff(context.Background(), b.Ref)
	if err != nil || !strings.Contains(out, "create table f (id int);") || !strings.Contains(out, "20260110000000") || strings.Contains(out, "create table t") {
		t.Fatalf("diff: %q %v", out, err)
	}
}

func TestSeedStore(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if s, err := h.svc.GetSeed(ctx, parentRef); err != nil || s != "" {
		t.Fatalf("no seed: %q %v", s, err)
	}
	if err := h.svc.SetSeed(ctx, parentRef, "select 1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.svc.GetSeed(ctx, parentRef); s != "select 1" {
		t.Fatalf("seed = %q", s)
	}
	// An explicit seed on the request wins over the stored one.
	b := h.create("seeded", func(in *CreateInput) { in.Seed = "select 2" })
	if got := h.db.scripts[b.Ref]; len(got) != 1 || got[0] != "select 2" {
		t.Fatalf("scripts = %v", got)
	}
	// A failing seed fails the branch but leaves it for inspection.
	f := h.create("badseed", func(in *CreateInput) { in.Seed = "FAIL" })
	h.mustState(f, registry.BranchMigrationsFailed)
	if !strings.Contains(f.Detail, "seed") {
		t.Fatalf("detail = %q", f.Detail)
	}
}

// cloneHarness is a harness whose with_data branches take the (faked) copy-on-write path.
func cloneHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.svc.detect = func(string, string) (string, string, string) { return MethodReflink, "xfs", "" }
	h.svc.clone = func(_ context.Context, _, method, _ string) (*CloneStats, error) {
		return &CloneStats{Method: method}, nil
	}
	return h
}

func TestResetRefusesBeforeRemovingAnything(t *testing.T) {
	h := cloneHarness(t)
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	// The data disk lost its clone support since the branch was made.
	h.svc.detect = func(string, string) (string, string, string) { return "", "ext4", "ext4 does not support file cloning" }
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "ext4") {
		t.Fatalf("reset = %v", err)
	}
	if len(h.eng.deletes) != 0 {
		t.Fatalf("the refused reset removed %v", h.eng.deletes)
	}
	got, err := h.svc.Resolve(context.Background(), b.ID)
	if err != nil || got.State != registry.BranchMigrationsPassed || got.ProjectStatus != registry.StatusActiveHealthy {
		t.Fatalf("branch after the refused reset: %+v %v", got, err)
	}
}

func TestResetWithoutABaseBackupIsRefusedFirst(t *testing.T) {
	h := cloneHarness(t)
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	store, err := backup.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.svc.bk, err = backup.New(backup.Options{Store: store, Registry: h.reg, Config: h.cfg})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.detect = func(string, string) (string, string, string) { return "", "ext4", "ext4 does not support file cloning" }
	// A base-backup reset is planned, but the parent has no base backup to restore.
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset = %v", err)
	}
	if len(h.eng.deletes) != 0 {
		t.Fatalf("the refused reset removed %v", h.eng.deletes)
	}
	if got, err := h.svc.Resolve(context.Background(), b.ID); err != nil || got.State != registry.BranchMigrationsPassed {
		t.Fatalf("branch after the refused reset: %+v %v", got, err)
	}
}

// A reset that fails after the old cluster is gone leaves the branch registered, failed, with
// its id, so that the client sees MIGRATIONS_FAILED and can reset again or delete it.
func TestFailedResetLeavesTheBranchRegistered(t *testing.T) {
	cases := map[string]func(h *harness){
		"the engine fails after the row is touched":  func(h *harness) { h.eng.createErr = errors.New("units did not start") },
		"the engine fails before it touches the row": func(h *harness) { h.eng.failBeforeRow = errors.New("lifecycle lock timeout") },
		"the parent's credentials cannot be read":    func(h *harness) { h.eng.keysFailAfter = h.eng.keyCalls + 1 },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := cloneHarness(t)
			ctx := context.Background()
			b := h.create("data", func(in *CreateInput) { in.WithData = true })
			breakIt(h)
			if _, err := h.svc.Reset(ctx, b.ID, ActionInput{}); err != nil {
				t.Fatal(err)
			}
			got := h.wait(b.Ref)
			h.mustState(got, registry.BranchMigrationsFailed)
			byID, err := h.svc.Resolve(ctx, b.ID)
			if err != nil || byID.Ref != b.Ref || byID.ProjectStatus != registry.StatusInitFailed {
				t.Fatalf("the branch after a failed reset: %+v %v", byID, err)
			}
			if h.eng.deletes[len(h.eng.deletes)-1] != b.Ref || h.eng.keptRecord[b.Ref] != true {
				t.Fatalf("the reset must keep the row: deletes=%v kept=%v", h.eng.deletes, h.eng.keptRecord)
			}
			// Repaired, it can be reset again (state INIT_FAILED, MIGRATIONS_FAILED), and keeps its identity.
			h.eng.createErr, h.eng.failBeforeRow, h.eng.keysFailAfter = nil, nil, 0
			if _, err := h.svc.Reset(ctx, b.ID, ActionInput{}); err != nil {
				t.Fatal(err)
			}
			again := h.wait(b.Ref)
			h.mustState(again, registry.BranchMigrationsPassed)
			if again.ID != b.ID || again.Ref != b.Ref || again.ProjectStatus != registry.StatusActiveHealthy {
				t.Fatalf("after the second reset: %+v", again)
			}
			if last := h.eng.creates[len(h.eng.creates)-1]; !last.Recreate {
				t.Fatalf("a reset must recreate over the kept row: %+v", last)
			}
		})
	}
	// And a branch left like that can be deleted.
	h := cloneHarness(t)
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	h.eng.createErr = errors.New("units did not start")
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	if _, err := h.svc.Delete(context.Background(), b.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Resolve(context.Background(), b.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestClonedBranchIsIsolatedBeforeItsCredentialsRotate(t *testing.T) {
	h := cloneHarness(t)
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsPassed)
	if len(h.isolated) != 1 || h.isolated[0] != b.Ref {
		t.Fatalf("isolated = %v", h.isolated)
	}
	// A schema-only branch holds none of the parent's integrations.
	s := h.create("schema", nil)
	if len(h.isolated) != 1 {
		t.Fatalf("a schema-only branch was isolated: %v (%s)", h.isolated, s.Ref)
	}
	// When the isolation fails the branch is stopped, not left with the parent's subscriptions.
	h.isolateErr = errors.New("cannot reach the cluster")
	f := h.create("leaky", func(in *CreateInput) { in.WithData = true })
	h.mustState(f, registry.BranchMigrationsFailed)
	if !strings.Contains(f.Detail, "cannot reach the cluster") || h.eng.paused[f.Ref] != 1 {
		t.Fatalf("detail = %q paused = %v", f.Detail, h.eng.paused)
	}
}

func TestStampManagerRecreatesAndQuarantinesTheRestoredCluster(t *testing.T) {
	h := newHarness(t, nil)
	var seeded string
	seed := func(_ context.Context, _ *registry.Project, dir string) error {
		seeded = dir
		return os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte("restore_command = 'x'\n"), 0o600)
	}
	dir := t.TempDir()
	m := stampManager{Manager: h.eng, info: &registry.BranchInfo{ID: newUUID(), ParentRef: parentRef, Name: "r"}, class: "micro", recreate: true}
	if _, err := h.eng.reg.GetProject(context.Background(), parentRef); err != nil {
		t.Fatal(err)
	}
	// Recreate needs an INIT_FAILED row.
	const ref = "rrrrrrrrrrrrrrrrrrrr"
	if err := h.reg.CreateProject(context.Background(), &registry.Project{Ref: ref, Name: "r", Status: registry.StatusInitFailed, Branch: &registry.BranchInfo{ID: "x", ParentRef: parentRef, Name: "r"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), lifecycle.CreateRequest{Ref: ref, Class: "default", Seed: func(ctx context.Context, p *registry.Project, d string) error { return seed(ctx, p, d) }}); err != nil {
		t.Fatal(err)
	}
	req := h.eng.creates[len(h.eng.creates)-1]
	if !req.Recreate || req.Class != "micro" || req.Branch == nil || req.Seed == nil {
		t.Fatalf("request = %+v", req)
	}
	if err := req.Seed(context.Background(), nil, dir); err != nil || seeded != dir {
		t.Fatalf("seed: %v %q", err, seeded)
	}
	conf, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	for _, want := range []string{"restore_command = 'x'", "max_logical_replication_workers = 0", "cron.launch_active_jobs = off", "pg_net.database_name"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("auto.conf lacks %q:\n%s", want, conf)
		}
	}
}

func TestQuarantineSettingsComeAndGo(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "postgresql.auto.conf")
	// The parent's own entries (even for the same key, and without a final newline) survive.
	if err := os.WriteFile(conf, []byte("# Do not edit this file manually!\nmax_logical_replication_workers = '8'\nwork_mem = '8MB'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeQuarantine(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(conf)
	if i, j := strings.LastIndex(string(got), "max_logical_replication_workers = 8"), strings.LastIndex(string(got), "max_logical_replication_workers = 0"); j < i {
		t.Fatalf("the quarantine value must come last:\n%s", got)
	}
	if err := clearQuarantine(dir); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(conf)
	if want := "# Do not edit this file manually!\nwork_mem = '8MB'\nmax_logical_replication_workers = '8'\n"; string(got) != want {
		t.Fatalf("after clearing:\n%q\nwant\n%q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, quarantineSaved)); !os.IsNotExist(err) {
		t.Fatalf("the saved-lines file is left behind: %v", err)
	}

	// ALTER SYSTEM (a restore resets its recovery settings with it) rewrites the file: comments and
	// the marker are gone, values are quoted, duplicates collapse. The parent's own value for a
	// quarantined key is gone too, which is why it was saved first.
	dir2 := t.TempDir()
	conf2 := filepath.Join(dir2, "postgresql.auto.conf")
	if err := os.WriteFile(conf2, []byte("max_logical_replication_workers = '8'\nwork_mem = '8MB'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeQuarantine(dir2); err != nil {
		t.Fatal(err)
	}
	rewritten := "# Do not edit this file manually!\nwork_mem = '8MB'\nmax_logical_replication_workers = '0'\ncron.launch_active_jobs = 'off'\npg_net.database_name = 'sbctl_quarantine_no_such_database'\n"
	if err := os.WriteFile(conf2, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearQuarantine(dir2); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(conf2)
	if want := "# Do not edit this file manually!\nwork_mem = '8MB'\nmax_logical_replication_workers = '8'\n"; string(got2) != want {
		t.Fatalf("after clearing a rewritten file:\n%q\nwant\n%q", got2, want)
	}

	// No auto.conf at all is fine.
	empty := t.TempDir()
	if err := clearQuarantine(empty); err != nil {
		t.Fatal(err)
	}
	if err := writeQuarantine(empty); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(empty, "postgresql.auto.conf")); !strings.Contains(string(b), "cron.launch_active_jobs") {
		t.Fatalf("written: %q", b)
	}
}

// A branch interrupted by a crash is recovered at once when the process that started the
// operation is known to be gone, not only after staleAfter.
func TestAbandonedOperationIsRecoveredWithoutWaiting(t *testing.T) {
	deadPID := func() int {
		cmd := exec.Command("true")
		if err := cmd.Run(); err != nil {
			t.Skipf("cannot start a process to get a dead pid: %v", err)
		}
		return cmd.Process.Pid
	}()
	ctx := context.Background()
	host, _ := os.Hostname()
	cases := []struct {
		name  string
		owner map[string]any
		gone  bool
	}{
		{"dead pid on this host", map[string]any{"host": host, "pid": deadPID, "instance": "old"}, true},
		{"an earlier service of this process", map[string]any{"host": host, "pid": os.Getpid(), "instance": "earlier"}, true},
		{"a live process on this host", map[string]any{"host": host, "pid": os.Getppid(), "instance": "other"}, false},
		{"another host", map[string]any{"host": host + "-elsewhere", "pid": deadPID, "instance": "old"}, false},
		{"no record", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil) // its own clock: the time rule below moves it
			b := h.create("x", nil)
			// An operation that started, and never finished.
			if c.owner != nil {
				h.svc.event(ctx, b.Ref, "branch.merge.started", map[string]any{"operation": "merge", "owner": c.owner})
			} else {
				h.svc.event(ctx, b.Ref, "branch.merge.started", map[string]any{"operation": "merge"})
			}
			h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging", nil)
			// No time has passed: only a known-dead owner makes it abandoned.
			_, mergeErr := h.svc.Merge(ctx, b.Ref, ActionInput{})
			if c.gone != (mergeErr == nil) {
				t.Fatalf("merge after the crash: %v (abandoned = %v)", mergeErr, c.gone)
			}
			if !c.gone {
				res, _ := h.svc.Sweep(ctx, false)
				if len(res.Stale) != 0 {
					t.Fatalf("swept a branch whose owner may be alive: %+v", res)
				}
				h.advance(time.Hour)
				if res, _ := h.svc.Sweep(ctx, false); len(res.Stale) != 1 {
					t.Fatalf("the time rule still applies: %+v", res)
				}
				return
			}
			// Merge ran and finished: the branch is idle again, not stale.
			h.wait(b.Ref)
			if got, _ := h.svc.Resolve(ctx, b.Ref); busy(got.State) {
				t.Fatalf("state = %s", got.State)
			}
			// The sweeper fails a fresh abandoned operation without waiting, too.
			h.svc.event(ctx, b.Ref, "branch.push.started", map[string]any{"operation": "push", "owner": c.owner})
			h.svc.setState(ctx, b.Ref, registry.BranchRunningMigration, "pushing", nil)
			if res, _ := h.svc.Sweep(ctx, false); len(res.Stale) != 1 || res.Stale[0] != b.Ref {
				t.Fatalf("sweep: %+v", res)
			}
			if got, _ := h.svc.Resolve(ctx, b.Ref); got.State != registry.BranchMigrationsFailed {
				t.Fatalf("state = %s", got.State)
			}
		})
	}
}
