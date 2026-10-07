package branching

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

const mib = 1 << 20

// parentData gives the parent a data directory whose apparent size is n bytes.
func parentData(t *testing.T, h *harness, ref string, n int64) {
	t.Helper()
	dir := filepath.Join(h.cfg.Paths().ProjectService(ref, config.SvcPostgres), "data")
	if err := os.MkdirAll(filepath.Join(dir, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "base", "16384"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil { // sparse: the check reads apparent sizes
		t.Fatal(err)
	}
}

func projectCount(t *testing.T, h *harness) int {
	t.Helper()
	ps, err := h.reg.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(ps)
}

// A with_data create needs 1.2 times the parent's data plus the reserve on the state disk, and
// is refused with a message that says so before anything exists.
func TestWithDataRefusedWithoutFreeDisk(t *testing.T) {
	h := cloneHarness(t)
	h.cfg.Branching.DiskReserveMB = 1
	parentData(t, h, parentRef, 10*mib)
	need := int64(10*mib/10*12 + 1*mib)
	if got := h.svc.requiredDisk(10 * mib); got != need {
		t.Fatalf("requiredDisk = %d, want %d", got, need)
	}

	h.svc.freeBytes = func(string) int64 { return need - 1 }
	_, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "big", WithData: true})
	if !errors.Is(err, ErrInsufficientDisk) || !errors.Is(err, ErrConflict) {
		t.Fatalf("create with too little disk = %v", err)
	}
	for _, want := range []string{"not enough free disk", "needs about 13.0 MiB", "its data is 10.0 MiB", "disk_reserve_mb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if len(h.eng.creates) != 0 || projectCount(t, h) != 1 {
		t.Fatalf("a refused create left work behind: creates=%d projects=%d", len(h.eng.creates), projectCount(t, h))
	}

	// Exactly enough passes.
	h.svc.freeBytes = func(string) int64 { return need }
	b := h.create("fits", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsPassed)
}

// The same check guards the base-backup path (and a node whose way to get data is the backup).
func TestDiskCheckAppliesToTheBaseBackupPathToo(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Branching.Clone = "backup"; c.Branching.DiskReserveMB = 1 })
	h.svc.freeBytes = func(string) int64 { return 0 }
	_, err := h.svc.Create(context.Background(), parentRef, CreateInput{Name: "b", WithData: true})
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("create = %v", err)
	}
}

// Only a branch with data copies anything, so a schema-only branch is not held back by the disk.
func TestSchemaOnlyCreateIgnoresTheDiskCheck(t *testing.T) {
	h := newHarness(t, nil)
	parentData(t, h, parentRef, 10*mib)
	h.svc.freeBytes = func(string) int64 { return 0 }
	b := h.create("schema", nil)
	h.mustState(b, registry.BranchMigrationsPassed)
}

// An unreadable free-space figure does not block creation.
func TestDiskCheckSkippedWhenFreeSpaceIsUnknown(t *testing.T) {
	h := cloneHarness(t)
	h.svc.freeBytes = func(string) int64 { return -1 }
	b := h.create("unknown", func(in *CreateInput) { in.WithData = true })
	h.mustState(b, registry.BranchMigrationsPassed)
}

// The default reserve is 2 GB, and the 1.2 factor applies to the parent's data.
func TestDiskReserveDefault(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.svc.requiredDisk(0); got != 2<<30 {
		t.Fatalf("default reserve = %d", got)
	}
	if got := h.svc.requiredDisk(100 * mib); got != 120*mib+2<<30 {
		t.Fatalf("100 MiB parent needs %d", got)
	}
}

// A reset is refused for lack of disk before the old cluster is touched. A base-backup branch's
// own copy is given back by the reset, so it counts; a copy-on-write branch's does not.
func TestResetRefusedWithoutFreeDisk(t *testing.T) {
	h := cloneHarness(t)
	h.cfg.Branching.DiskReserveMB = 1
	parentData(t, h, parentRef, 10*mib)
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	parentData(t, h, b.Ref, 10*mib) // the branch's own copy

	h.svc.freeBytes = func(string) int64 { return 4 * mib } // need 13 MiB
	_, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{})
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("reset of a reflink branch = %v", err)
	}
	if len(h.eng.deletes) != 0 {
		t.Fatalf("the refused reset removed %v", h.eng.deletes)
	}
	got, err := h.svc.Resolve(context.Background(), b.ID)
	if err != nil || got.State != registry.BranchMigrationsPassed || got.ProjectStatus != registry.StatusActiveHealthy {
		t.Fatalf("branch after the refused reset: %+v %v", got, err)
	}

	// Its copy is private: 4 MiB free plus the 10 MiB the reset gives back is enough.
	h.svc.setState(context.Background(), b.Ref, registry.BranchMigrationsPassed, "ok", func(bi *registry.BranchInfo) { bi.CloneMethod = MethodBackup })
	if _, err := h.svc.Reset(context.Background(), b.Ref, ActionInput{}); errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("reset of a base-backup branch did not count its own copy: %v", err)
	}
	h.wait(b.Ref)
}

func TestEgressPolicyOfANewBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		supervisor string
		withData   bool
		allow      bool
		want       string
		detail     []string
	}{
		{"systemd default", config.SupervisorSystemd, true, false, registry.EgressPending, []string{"egress denied", "pg_cron jobs paused", PausedCronTable}},
		{"systemd opt-out", config.SupervisorSystemd, true, true, registry.EgressAllowed, []string{"egress NOT blocked (created with allow_egress)", "pg_cron jobs left active"}},
		{"exec cannot enforce", config.SupervisorExec, true, false, registry.EgressUnenforced, []string{"egress NOT blocked", "exec supervisor cannot confine", "pg_cron jobs paused"}},
		{"exec opt-out", config.SupervisorExec, true, true, registry.EgressAllowed, []string{"allow_egress"}},
		{"schema only has nothing to isolate", config.SupervisorSystemd, false, true, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := cloneHarness(t)
			h.cfg.Supervisor = tc.supervisor
			b := h.create("e", func(in *CreateInput) { in.WithData, in.AllowEgress = tc.withData, tc.allow })
			h.mustState(b, registry.BranchMigrationsPassed)
			if b.Egress != tc.want {
				t.Fatalf("egress = %q, want %q (%s)", b.Egress, tc.want, b.Detail)
			}
			for _, d := range tc.detail {
				if !strings.Contains(b.Detail, d) {
					t.Errorf("detail lacks %q: %s", d, b.Detail)
				}
			}
			if tc.want == "" && strings.Contains(b.Detail, "egress") {
				t.Errorf("a schema-only branch reports egress: %s", b.Detail)
			}
		})
	}
}

// keep_cron_jobs keeps the jobs of a branch whose egress is still denied, and says so.
func TestKeepCronJobsIsReported(t *testing.T) {
	h := cloneHarness(t)
	h.cfg.Branching.KeepCronJobs = true
	b := h.create("e", func(in *CreateInput) { in.WithData = true })
	if !strings.Contains(b.Detail, "egress denied") || !strings.Contains(b.Detail, "pg_cron jobs left active") {
		t.Fatalf("detail = %s", b.Detail)
	}
}

// A reset gives the new cluster the same policy: the opt-out stays, a denial starts over as
// pending (it is applied after the new cluster's first start) and a branch that could not be
// confined is tried again.
func TestResetKeepsTheEgressPolicy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, before, want string }{
		{"opt-out", registry.EgressAllowed, registry.EgressAllowed},
		{"denied", registry.EgressDenied, registry.EgressPending},
		{"unenforced", registry.EgressUnenforced, registry.EgressPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := cloneHarness(t)
			b := h.create("data", func(in *CreateInput) { in.WithData = true })
			if err := h.reg.SetBranchEgress(ctx, b.Ref, registry.EgressPending, tc.before); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.Reset(ctx, b.Ref, ActionInput{}); err != nil {
				t.Fatal(err)
			}
			got := h.wait(b.Ref)
			h.mustState(got, registry.BranchMigrationsPassed)
			if got.Egress != tc.want {
				t.Fatalf("after reset egress = %q, want %q", got.Egress, tc.want)
			}
			// The cluster was created with the policy that the row carries: pending, so that
			// its first start runs open.
			if last := h.eng.creates[len(h.eng.creates)-1]; last.Branch == nil || last.Branch.Egress == registry.EgressDenied {
				t.Fatalf("reset created the cluster with %+v", last.Branch)
			}
		})
	}
}

func TestDenyEgressRecordsTheDenial(t *testing.T) {
	h := cloneHarness(t)
	ctx := context.Background()
	b := h.create("data", func(in *CreateInput) { in.WithData = true })
	if b.Egress != registry.EgressPending {
		t.Fatalf("egress = %q", b.Egress)
	}
	if err := h.svc.denyEgress(ctx, b.Ref); err != nil {
		t.Fatal(err)
	}
	p, err := h.reg.GetProject(ctx, b.Ref)
	if err != nil || p.Branch.Egress != registry.EgressDenied || p.Branch.State != b.State {
		t.Fatalf("after denyEgress: %+v %v", p.Branch, err)
	}
	if err := h.svc.denyEgress(ctx, parentRef); err == nil {
		t.Fatal("denyEgress accepted a project that is not a branch")
	}
}
