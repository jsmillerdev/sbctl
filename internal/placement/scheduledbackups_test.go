package placement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/registry"
)

// scheduledEnv is a leader (n1) with two projects: one homed on n2 and one homed here.
type scheduledEnv struct {
	*opsEnv
	reg    *registry.Memory
	local  *fakeBackups // the leader's backup service
	s      *ScheduledBackups
	now    time.Time
	remote string // the ref homed on n2
	here   string // the ref homed on n1
}

func newScheduledEnv(t *testing.T) *scheduledEnv {
	t.Helper()
	ctx := context.Background()
	e := newOpsEnv(t)
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	env := &scheduledEnv{opsEnv: e, reg: reg, local: &fakeBackups{}, now: time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC),
		remote: testRef, here: "bcdefghijklmnopqrstu"}
	for _, ref := range []string{env.remote, env.here} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetProjectNode(ctx, env.remote, "n2", 1); err != nil {
		t.Fatal(err)
	}
	e.ops.Recorder = env.local
	env.s = &ScheduledBackups{Registry: reg, Self: func() string { return "n1" }, Now: func() time.Time { return env.now },
		Routed: &RoutedBackups{Self: func() string { return "n1" }, Resolver: RegistryResolver{Reg: reg}, Ops: e.ops, Local: env.local}}
	return env
}

// A project homed on a follower gets what its timer would have given it: the files snapshot here, the
// base backup on its home, both as "scheduled", and a row for the base backup in the leader's registry
// (the leader's own projects keep their timer and are left alone).
func TestScheduledBackupsTakeTheBackupOfAProjectHomedElsewhere(t *testing.T) {
	ctx := context.Background()
	e := newScheduledEnv(t)

	if errs := e.s.Once(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(e.local.files) != 1 || e.local.files[0] != e.remote+" "+backup.ReasonScheduled {
		t.Fatalf("files snapshots = %v", e.local.files)
	}
	if len(e.backups.reasons) != 1 || e.backups.reasons[0] != backup.ReasonScheduled || !e.backups.noRecord[0] {
		t.Fatalf("n2 took %v (no record %v)", e.backups.reasons, e.backups.noRecord)
	}
	if len(e.local.reasons) != 0 {
		t.Fatalf("a project homed here was backed up by the schedule: %v", e.local.reasons)
	}
	if len(e.local.recorded) == 0 || e.local.recorded[0].Reason != backup.ReasonScheduled {
		t.Fatalf("recorded %+v", e.local.recorded)
	}

	// The backup is in the registry now: nothing is due until Every has passed.
	if err := e.reg.CreateBackup(ctx, &registry.Backup{Ref: e.remote, Kind: "base", Status: registry.BackupCompleted, StartedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(23 * time.Hour)
	e.s.Once(ctx)
	if len(e.backups.reasons) != 1 {
		t.Fatalf("a backup of 23 hours ago was taken again: %v", e.backups.reasons)
	}
	e.now = e.now.Add(2 * time.Hour)
	e.s.Once(ctx)
	if len(e.backups.reasons) != 2 {
		t.Fatalf("a backup of 25 hours ago was not renewed: %v", e.backups.reasons)
	}
}

// Only a project that runs is backed up; the system project is the node prune timer's concern and
// the leader's own backup is taken by its timer. A node that does not lead does nothing.
func TestScheduledBackupsLeaveOutWhatIsNotTheirs(t *testing.T) {
	ctx := context.Background()
	e := newScheduledEnv(t)

	if err := e.reg.SetProjectStatus(ctx, e.remote, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	e.s.Once(ctx)
	if len(e.backups.reasons) != 0 || len(e.local.files) != 0 {
		t.Fatalf("a paused project was backed up: %v %v", e.backups.reasons, e.local.files)
	}

	if err := e.reg.SetProjectStatus(ctx, e.remote, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	e.s.Leader = func() bool { return false }
	e.s.Once(ctx)
	if len(e.backups.reasons) != 0 {
		t.Fatalf("a node that does not lead took a backup: %v", e.backups.reasons)
	}
	e.s.Leader = func() bool { return true }
	e.s.Once(ctx)
	if len(e.backups.reasons) != 1 {
		t.Fatalf("the leader took %v", e.backups.reasons)
	}
}

// A failure is named in the project's events and tried again after Retry, not every round; the base
// backup is taken although the files snapshot failed, as `backups create` does.
func TestScheduledBackupsRetryAfterAFailure(t *testing.T) {
	ctx := context.Background()
	e := newScheduledEnv(t)

	e.backups.err = errors.New("pg_backup_start: boom")
	errs := e.s.Once(ctx)
	if err := errs[e.remote]; err == nil || !strings.Contains(err.Error(), "node n2") {
		t.Fatalf("errors = %v", errs)
	}
	evs, _ := e.reg.ListEvents(ctx, e.remote, 10)
	if len(evs) != 1 || evs[0].Kind != "backup.failed" || !strings.Contains(string(evs[0].Payload), "boom") {
		t.Fatalf("events = %+v", evs)
	}
	if len(e.backups.reasons) != 1 {
		t.Fatalf("n2 was asked %d times", len(e.backups.reasons))
	}

	e.now = e.now.Add(30 * time.Minute)
	e.s.Once(ctx)
	if len(e.backups.reasons) != 1 {
		t.Fatalf("n2 was asked again after 30 minutes: %d", len(e.backups.reasons))
	}
	e.backups.err = nil
	e.now = e.now.Add(31 * time.Minute)
	if errs := e.s.Once(ctx); len(errs) != 0 || len(e.backups.reasons) != 2 {
		t.Fatalf("after the retry time: %v, asked %d times", errs, len(e.backups.reasons))
	}

	// The files snapshot failing does not keep the base backup from being taken.
	e.local.err = errors.New("storage unreadable")
	e.now = e.now.Add(48 * time.Hour)
	errs = e.s.Once(ctx)
	if err := errs[e.remote]; err == nil || !strings.Contains(err.Error(), "storage unreadable") || len(e.backups.reasons) != 3 {
		t.Fatalf("errors = %v, n2 asked %d times", errs, len(e.backups.reasons))
	}
}

// A registry that runs a hook while a project's backups are listed, which is a step of that
// project's turn in the round.
type hookedReg struct {
	registry.Registry
	onListBackups func(ref string)
}

func (r hookedReg) ListBackups(ctx context.Context, ref string) ([]registry.Backup, error) {
	if r.onListBackups != nil {
		r.onListBackups(ref)
	}
	return r.Registry.ListBackups(ctx, ref)
}

// The round lists the projects once and may run for hours: a project that moved to the leader, was
// paused or was deleted before its turn is left alone, and its former home is not asked.
func TestScheduledBackupsReadTheProjectAgainBeforeItsTurn(t *testing.T) {
	ctx := context.Background()
	e := newScheduledEnv(t)
	others := map[string]func(){}
	for _, ref := range []string{"cdefghijklmnopqrstuv", "defghijklmnopqrstuvw", "efghijklmnopqrstuvwx"} {
		if err := e.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
		if err := e.reg.SetProjectNode(ctx, ref, "n2", 1); err != nil {
			t.Fatal(err)
		}
	}
	others["cdefghijklmnopqrstuv"] = func() {
		if err := e.reg.SetProjectNode(ctx, "cdefghijklmnopqrstuv", "n1", 1); err != nil {
			t.Error(err)
		}
	}
	others["defghijklmnopqrstuvw"] = func() { _ = e.reg.SetProjectStatus(ctx, "defghijklmnopqrstuvw", registry.StatusInactive) }
	others["efghijklmnopqrstuvwx"] = func() { _ = e.reg.DeleteProject(ctx, "efghijklmnopqrstuvwx") }
	// The first project's turn changes the three that come after it.
	e.s.Registry = hookedReg{Registry: e.reg, onListBackups: func(ref string) {
		if ref != e.remote {
			return
		}
		for _, f := range others {
			f()
		}
	}}

	if errs := e.s.Once(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(e.local.files) != 1 || e.local.files[0] != e.remote+" "+backup.ReasonScheduled || len(e.backups.reasons) != 1 {
		t.Fatalf("files snapshots %v, n2 took %v", e.local.files, e.backups.reasons)
	}
}

// hangingFiles is a backup service whose files snapshot waits for its context.
type hangingFiles struct{ *fakeBackups }

func (h hangingFiles) BackupFiles(ctx context.Context, ref string, fo backup.FilesOptions) (*backup.FilesResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One project whose backup hangs ends at its deadline and the projects after it get their turn.
func TestScheduledBackupsBoundEachProject(t *testing.T) {
	ctx := context.Background()
	e := newScheduledEnv(t)
	const second = "cdefghijklmnopqrstuv"
	if err := e.reg.CreateProject(ctx, &registry.Project{Ref: second, Name: second, Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetProjectNode(ctx, second, "n2", 1); err != nil {
		t.Fatal(err)
	}
	e.s.Routed.Local = hangingFiles{e.local}
	e.s.Timeout = 30 * time.Millisecond

	start := time.Now()
	errs := e.s.Once(ctx)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the round took %s", time.Since(start))
	}
	for _, ref := range []string{e.remote, second} {
		if err := errs[ref]; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("errors = %v", errs)
		}
	}
	// The deadline of the first did not end the round, and the failure is in the history of each.
	evs, _ := e.reg.ListEvents(ctx, second, 10)
	if len(evs) != 1 || evs[0].Kind != "backup.failed" {
		t.Fatalf("events of the second = %+v", evs)
	}
}

// A schedule that is not set up reports it and does nothing.
func TestScheduledBackupsNeedTheirSeams(t *testing.T) {
	s := &ScheduledBackups{Registry: registry.NewMemory()}
	if errs := s.Once(context.Background()); errs[""] == nil {
		t.Fatalf("errors = %v", errs)
	}
}

// A seam the wiring left nil is reported by the round, not found by the first backup of a project
// homed elsewhere as a nil dereference.
func TestScheduledBackupsReportEachSeamTheWiringLeftNil(t *testing.T) {
	for name, unset := range map[string]func(*ScheduledBackups){
		"registry":     func(s *ScheduledBackups) { s.Registry = nil },
		"self":         func(s *ScheduledBackups) { s.Self = nil },
		"routed":       func(s *ScheduledBackups) { s.Routed = nil },
		"routed.local": func(s *ScheduledBackups) { s.Routed.Local = nil },
		"routed.self":  func(s *ScheduledBackups) { s.Routed.Self = nil },
		"routed.ops":   func(s *ScheduledBackups) { s.Routed.Ops = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newScheduledEnv(t)
			unset(e.s)
			if errs := e.s.Once(context.Background()); errs[""] == nil {
				t.Fatalf("errors = %v", errs)
			}
			if len(e.backups.reasons) != 0 || len(e.local.files) != 0 {
				t.Fatalf("a round that is not set up took backups: %v %v", e.backups.reasons, e.local.files)
			}
		})
	}
}
