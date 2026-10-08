package placement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// fakeReplicaPlane is a ReplicaPlane that reports the stages of a setup and answers from fields.
type fakeReplicaPlane struct {
	mu      sync.Mutex
	calls   []string
	targets []lifecycle.ReplicaTarget
	seeds   []lifecycle.ReplicaCreateOptions
	promote lifecycle.PromoteOptions
	obs     lifecycle.ReplicaObservation
	// failure injection, by method name
	err map[string]error
	// failAfter makes CreateReplica fail after reporting that stage; "" fails before any.
	failAfter  lifecycle.ReplicaStage
	failCreate error
	// block, when set, holds CreateReplica after the Seeding stage until it is closed or the context ends.
	block chan struct{}
	// onStage sees each stage on the setup's goroutine.
	onStage func(lifecycle.ReplicaStage)
}

func newFakeReplicaPlane() *fakeReplicaPlane {
	return &fakeReplicaPlane{err: map[string]error{}, obs: lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleReplica, PostgresUp: true, InRecovery: true, ReceiverStatus: "streaming", PostgRESTReady: true}}
}

func (f *fakeReplicaPlane) rec(call string, t *lifecycle.ReplicaTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if t != nil {
		f.targets = append(f.targets, *t)
	}
	return f.err[strings.Fields(call)[0]]
}

func (f *fakeReplicaPlane) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *fakeReplicaPlane) CreateReplica(ctx context.Context, t lifecycle.ReplicaTarget, o lifecycle.ReplicaCreateOptions) error {
	if err := f.rec("create "+t.Project.Ref, &t); err != nil {
		return err
	}
	f.mu.Lock()
	f.seeds = append(f.seeds, o)
	f.mu.Unlock()
	if f.failAfter == "" && f.failCreate != nil {
		return f.failCreate
	}
	for _, s := range []lifecycle.ReplicaStage{lifecycle.StageLaunched, lifecycle.StageSeeding, lifecycle.StageSeeded, lifecycle.StageStarted} {
		if o.Progress != nil {
			o.Progress(s)
		}
		if f.onStage != nil {
			f.onStage(s)
		}
		if s == lifecycle.StageSeeding && f.block != nil {
			select {
			case <-f.block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if f.failAfter == s {
			return f.failCreate
		}
	}
	return nil
}
func (f *fakeReplicaPlane) StartReplica(_ context.Context, t lifecycle.ReplicaTarget) error {
	return f.rec("start "+t.Project.Ref, &t)
}
func (f *fakeReplicaPlane) StartReplicaDatabase(_ context.Context, t lifecycle.ReplicaTarget) error {
	return f.rec("start-db "+t.Project.Ref, &t)
}
func (f *fakeReplicaPlane) StartReplicaAPI(_ context.Context, t lifecycle.ReplicaTarget) error {
	return f.rec("start-api "+t.Project.Ref, &t)
}
func (f *fakeReplicaPlane) StopReplica(_ context.Context, ref string) error {
	return f.rec("stop "+ref, nil)
}
func (f *fakeReplicaPlane) RemoveReplica(_ context.Context, ref string) error {
	return f.rec("remove "+ref, nil)
}
func (f *fakeReplicaPlane) ObserveReplica(_ context.Context, _ lifecycle.ReplicaTarget) lifecycle.ReplicaObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.obs
}
func (f *fakeReplicaPlane) PromoteReplica(_ context.Context, t lifecycle.ReplicaTarget, o lifecycle.PromoteOptions) error {
	f.mu.Lock()
	f.promote = o
	f.mu.Unlock()
	return f.rec("promote "+t.Project.Ref, &t)
}
func (f *fakeReplicaPlane) DemoteToReplica(_ context.Context, t lifecycle.ReplicaTarget) error {
	return f.rec("demote "+t.Project.Ref, &t)
}

type agentEnv struct {
	a     *NodeAgent
	plane *fakeReplicaPlane
	reg   *registry.Memory
	cfg   *config.Config
}

func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "demo", Class: "micro", Engine: registry.EnginePostgres}); err != nil {
		t.Fatal(err)
	}
	pl := newFakeReplicaPlane()
	a := NewNodeAgent(AgentOptions{
		Cfg: cfg, Plane: pl, Registry: reg, Members: members("n2", "n1", 5),
		Keys:   func(context.Context, string) (*secrets.ProjectKeys, error) { return testKeys(), nil },
		Seeder: func(context.Context, lifecycle.ReplicaSeedPlan) error { return nil },
		Poll:   time.Millisecond, StallTimeout: 200 * time.Millisecond,
	})
	return &agentEnv{a: a, plane: pl, reg: reg, cfg: cfg}
}

func (e *agentEnv) spec() peerapi.InstanceSpec {
	return peerapi.InstanceSpec{Identifier: testReplicaID(), Ref: testRef, BackupID: "b1", Epoch: 5}
}

// settle waits until the setup has ended and returns the status.
func (e *agentEnv) settle(t *testing.T) peerapi.InstanceStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in := e.a.get(testReplicaID())
		if in == nil {
			t.Fatal("no instance")
		}
		if _, _, _, running := e.a.snapshot(in); !running {
			st, err := e.a.Observe(context.Background(), testReplicaID())
			if err != nil {
				t.Fatal(err)
			}
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("the setup did not end")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEnsureSetsTheReplicaUpStepByStep(t *testing.T) {
	e := newAgentEnv(t)
	var steps []string
	e.plane.onStage = func(lifecycle.ReplicaStage) {
		in := e.a.get(testReplicaID())
		step, _, _, _ := e.a.snapshot(in)
		steps = append(steps, step)
	}
	e.plane.block = make(chan struct{})
	st, err := e.a.Ensure(context.Background(), e.spec())
	if err != nil {
		t.Fatal(err)
	}
	// It answers at once, while the base backup downloads.
	if st.Identifier != testReplicaID() || st.Ref != testRef || st.Error != "" {
		t.Fatalf("Ensure = %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, err := e.a.Observe(context.Background(), testReplicaID()); err == nil && s.Step == StepInitiated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the setup never reached the download")
		}
		time.Sleep(time.Millisecond)
	}
	// The same request again changes nothing.
	if _, err := e.a.Ensure(context.Background(), e.spec()); err != nil {
		t.Fatal(err)
	}
	close(e.plane.block)
	st = e.settle(t)
	if st.Step != StepCompleted || st.Error != "" || st.Role != "replica" || !st.PostgresUp || !st.PostgRESTReady {
		t.Fatalf("after the setup: %+v", st)
	}
	if want := []string{StepLaunched, StepInitiated, StepDownloaded, StepDownloaded}; strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if got := e.plane.all(); got != "create "+testRef+",start-api "+testRef {
		t.Fatalf("plane calls = %s", got)
	}
	if s := e.plane.seeds[0]; s.BackupID != "b1" || s.NoUpstream || s.Seeder == nil {
		t.Fatalf("create options = %+v", s)
	}
	if e.plane.targets[0].Project.Ref != testRef || e.plane.targets[0].Keys == nil || e.plane.targets[0].Identifier != testReplicaID() {
		t.Fatalf("target = %+v", e.plane.targets[0])
	}
	// The state is on disk, in the project's directory.
	b, err := os.ReadFile(filepath.Join(e.cfg.Paths().Project(testRef), "replica.json"))
	if err != nil || !strings.Contains(string(b), StepCompleted) {
		t.Fatalf("replica.json = %s, %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(e.cfg.Paths().Project(testRef), "replica.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
}

func TestEnsureReportsEachFailureAsTheStepThatFailed(t *testing.T) {
	boom := errors.New("connection reset by peer")
	for _, tc := range []struct {
		name  string
		setup func(e *agentEnv)
		want  string
	}{
		{"launching", func(e *agentEnv) { e.plane.failCreate = boom }, "1_read_replica_instance_launch_failed"},
		{"downloading", func(e *agentEnv) { e.plane.failCreate, e.plane.failAfter = boom, lifecycle.StageSeeding }, "3_download_base_backup_failed"},
		{"starting the standby", func(e *agentEnv) { e.plane.failCreate, e.plane.failAfter = boom, lifecycle.StageSeeded }, "4_replay_wal_archives_failed"},
		{"streaming", func(e *agentEnv) {
			e.plane.obs.ReceiverStatus, e.plane.obs.ReplayLSN = "", "0/3000000" // stalls
		}, "4_replay_wal_archives_failed"},
		{"starting PostgREST", func(e *agentEnv) { e.plane.err["start-api"] = boom }, "5_complete_read_replica_setup_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgentEnv(t)
			tc.setup(e)
			if _, err := e.a.Ensure(context.Background(), e.spec()); err != nil {
				t.Fatal(err)
			}
			st := e.settle(t)
			if st.Error != tc.want || st.Detail == "" || st.Step == StepCompleted {
				t.Fatalf("status = %+v, want error %s", st, tc.want)
			}
			// A failed setup stays failed: the same request does not start another download.
			calls := e.plane.all()
			again, err := e.a.Ensure(context.Background(), e.spec())
			if err != nil || again.Error != tc.want || e.plane.all() != calls {
				t.Fatalf("second Ensure: %+v %v; calls %s -> %s", again, err, calls, e.plane.all())
			}
		})
	}
}

func TestEnsureRefusesWhatIsNotAReplica(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	// The project is homed on this node.
	if err := e.reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Ensure(ctx, e.spec()); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), "never on the home") {
		t.Fatalf("a replica on the home: %v", err)
	}
	if err := e.reg.SetProjectNode(ctx, testRef, "n1", 1); err != nil {
		t.Fatal(err)
	}
	// A stale leader.
	s := e.spec()
	s.Epoch = 4
	if _, err := e.a.Ensure(ctx, s); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch: %v", err)
	}
	// Names that do not fit.
	s = e.spec()
	s.Ref = "zzzzzzzzzzzzzzzzzzzz"
	if _, err := e.a.Ensure(ctx, s); err == nil {
		t.Fatal("an identifier of another project")
	}
	s = e.spec()
	s.Identifier = "nonsense"
	if _, err := e.a.Ensure(ctx, s); err == nil {
		t.Fatal("a malformed identifier")
	}
	// An unknown project.
	other := "zzzzzzzzzzzzzzzzzzzz"
	s = peerapi.InstanceSpec{Identifier: registry.ReplicaIdentifier(other, "us-east-1", "abc123"), Ref: other, Epoch: 5}
	if _, err := e.a.Ensure(ctx, s); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	// A second replica of the same project on this node.
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	s = e.spec()
	s.Identifier = registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999")
	if _, err := e.a.Ensure(ctx, s); !errors.Is(err, lifecycle.ErrInvalidState) {
		t.Fatalf("a second replica of the project: %v", err)
	}
	if strings.Count(e.plane.all(), "create") != 1 {
		t.Fatalf("calls = %s", e.plane.all())
	}
}

func TestEnsureAdmitsTheReplicaOnThisNodesRoom(t *testing.T) {
	e := newAgentEnv(t)
	var asked []int64
	refuse := true
	e.a.o.Admit = func(_ context.Context, p *registry.Project, id string, bytes int64) error {
		asked = append(asked, bytes)
		if refuse {
			return &lifecycle.CapacityError{Message: "this node cannot run a Micro project"}
		}
		return nil
	}
	// The newest complete base backup is what the replica is seeded from, and its size is what is judged.
	for i, b := range []registry.Backup{
		{Ref: testRef, Kind: "base", Status: registry.BackupCompleted, Location: "file:///b/x/base/old", SizeBytes: 111},
		{Ref: testRef, Kind: "base", Status: registry.BackupRunning, Location: "file:///b/x/base/running", SizeBytes: 5},
		{Ref: testRef, Kind: "base", Status: registry.BackupCompleted, Location: "file:///b/x/base/new/", SizeBytes: 222},
	} {
		b := b
		_ = i
		if err := e.reg.CreateBackup(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
	}
	s := e.spec()
	s.BackupID = ""
	// A refusal is an error the leader can tell from a failed setup, and it leaves no trace: no
	// instance, no file, nothing for the plane.
	_, err := e.a.Ensure(context.Background(), s)
	var ce *lifecycle.CapacityError
	if !errors.Is(err, ErrNoRoom) || !errors.As(err, &ce) || !strings.Contains(err.Error(), "cannot run") || len(asked) != 1 || asked[0] != 222 {
		t.Fatalf("a refused replica: %v, asked %v", err, asked)
	}
	if e.a.get(testReplicaID()) != nil || e.a.recorded(testRef) != "" {
		t.Fatal("a refused replica was recorded")
	}
	if _, err := os.Stat(e.a.filePath(testRef)); !os.IsNotExist(err) {
		t.Fatalf("replica.json after a refusal: %v", err)
	}
	if e.plane.all() != "" {
		t.Fatalf("a refused replica reached the plane: %s", e.plane.all())
	}
	// The disk is a room too.
	e.a.o.Admit = func(context.Context, *registry.Project, string, int64) error {
		return fmt.Errorf("%w: the disk has 1 GiB free", lifecycle.ErrReplicaDisk)
	}
	if _, err := e.a.Ensure(context.Background(), s); !errors.Is(err, ErrNoRoom) || !errors.Is(err, lifecycle.ErrReplicaDisk) {
		t.Fatalf("no disk: %v", err)
	}
	// Any other failure of the check is an error too, and is not a lack of room.
	e.a.o.Admit = func(context.Context, *registry.Project, string, int64) error { return errors.New("registry down") }
	if _, err := e.a.Ensure(context.Background(), s); err == nil || errors.Is(err, ErrNoRoom) || e.a.recorded(testRef) != "" {
		t.Fatalf("a failed check: %v", err)
	}
	// Asking again once there is room starts the replica.
	e.a.o.Admit = func(_ context.Context, _ *registry.Project, _ string, bytes int64) error {
		asked = append(asked, bytes)
		return nil
	}
	if _, err := e.a.Ensure(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if st := e.settle(t); st.Step != StepCompleted || st.Error != "" {
		t.Fatalf("after room was made: %+v", st)
	}
	// A named base backup is looked up by its id.
	e2 := newAgentEnv(t)
	if err := e2.reg.CreateBackup(context.Background(), &registry.Backup{Ref: testRef, Kind: "base", Status: registry.BackupCompleted, Location: "file:///b/x/base/20261001T000000Z-ab12", SizeBytes: 333}); err != nil {
		t.Fatal(err)
	}
	if n := e2.a.backupBytes(context.Background(), peerapi.InstanceSpec{Ref: testRef, BackupID: "20261001T000000Z-ab12"}); n != 333 {
		t.Fatalf("backupBytes = %d", n)
	}
	if n := e2.a.backupBytes(context.Background(), peerapi.InstanceSpec{Ref: testRef, BackupID: "nope"}); n != 0 {
		t.Fatalf("backupBytes of an unknown backup = %d", n)
	}
}

func TestASetupCutOffByARestartResumes(t *testing.T) {
	ctx := context.Background()
	// The daemon died while the base backup downloaded: the next request starts over from nothing.
	e := newAgentEnv(t)
	e.plane.block = make(chan struct{})
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { s, _ := e.a.Observe(ctx, testReplicaID()); return s.Step == StepInitiated })
	// A new process: nothing in memory, the file is there. (The old setup is stopped as a restart would.)
	old := e.a
	oldInst := old.get(testReplicaID())
	old.mu.Lock()
	cancel := oldInst.cancel
	old.mu.Unlock()
	cancel()
	waitFor(t, func() bool { _, _, _, running := old.snapshot(oldInst); return !running })

	e2 := &agentEnv{plane: newFakeReplicaPlane(), reg: e.reg, cfg: e.cfg}
	e2.a = NewNodeAgent(AgentOptions{Cfg: e.cfg, Plane: e2.plane, Registry: e.reg, Members: members("n2", "n1", 5),
		Keys: func(context.Context, string) (*secrets.ProjectKeys, error) { return testKeys(), nil }, Poll: time.Millisecond})
	st, err := e2.a.Observe(ctx, testReplicaID())
	if err != nil || st.Step != StepInitiated || st.Error != "" {
		t.Fatalf("after the restart: %+v %v", st, err)
	}
	if _, err := e2.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	if st = e2.settle(t); st.Step != StepCompleted {
		t.Fatalf("resumed setup: %+v", st)
	}
	if got := e2.plane.all(); got != "remove "+testRef+",create "+testRef+",start-api "+testRef {
		t.Fatalf("a resumed download starts over: %s", got)
	}

	// The daemon died after the base backup was extracted: the standby starts, nothing downloads again.
	e3 := newAgentEnv(t)
	in := &instance{spec: e3.spec(), step: StepDownloaded}
	e3.a.mu.Lock()
	e3.a.insts[testReplicaID()] = in
	e3.a.mu.Unlock()
	e3.a.persist(in)
	e3.a.mu.Lock()
	delete(e3.a.insts, testReplicaID())
	e3.a.mu.Unlock()
	if _, err := e3.a.Ensure(ctx, e3.spec()); err != nil {
		t.Fatal(err)
	}
	if st = e3.settle(t); st.Step != StepCompleted {
		t.Fatalf("resumed after the extraction: %+v", st)
	}
	if got := e3.plane.all(); got != "start-db "+testRef+",start-api "+testRef {
		t.Fatalf("calls = %s", got)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAnArchiveOnlyStandbyCompletesWithoutStreamingOrAPI(t *testing.T) {
	e := newAgentEnv(t)
	e.plane.obs.ReceiverStatus = ""
	s := e.spec()
	s.NoUpstream = true
	if _, err := e.a.Ensure(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	st := e.settle(t)
	if st.Step != StepCompleted || st.Error != "" || !e.plane.seeds[0].NoUpstream || strings.Contains(e.plane.all(), "start-api") {
		t.Fatalf("status %+v, calls %s", st, e.plane.all())
	}
}

func TestRemoveStopsASetupAndRefusesToRemoveAHome(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	e.plane.block = make(chan struct{})
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { s, _ := e.a.Observe(ctx, testReplicaID()); return s.Step == StepInitiated })
	// Removing a replica that is being set up stops the setup, then deletes the directory.
	if err := e.a.Remove(ctx, testReplicaID()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.plane.all(), "remove "+testRef) {
		t.Fatalf("calls = %s", e.plane.all())
	}
	if e.a.get(testReplicaID()) != nil {
		t.Fatal("the instance is still known")
	}
	// Removing one that is not there succeeds.
	if err := e.a.Remove(ctx, testReplicaID()); err != nil {
		t.Fatal(err)
	}
	// A cluster that has become the primary here is never removed as a replica.
	e.plane.obs.Role = lifecycle.ReplicaRolePrimary
	n := strings.Count(e.plane.all(), "remove")
	if err := e.a.Remove(ctx, testReplicaID()); !errors.Is(err, lifecycle.ErrInvalidState) || strings.Count(e.plane.all(), "remove") != n {
		t.Fatalf("remove of a primary: %v", err)
	}
	// Nor is the home's data, whatever the observation says.
	e.plane.obs.Role = lifecycle.ReplicaRoleReplica
	if err := e.reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Remove(ctx, testReplicaID()); !errors.Is(err, lifecycle.ErrInvalidState) || strings.Count(e.plane.all(), "remove") != n {
		t.Fatalf("remove on the home: %v", err)
	}
}

func TestDoRunsTheReplicaActions(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	e.plane.mu.Lock()
	e.plane.calls, e.plane.targets = nil, nil
	e.plane.mu.Unlock()
	id := testReplicaID()

	// Restart renders from the size the leader names, not from the node's copy of the registry.
	if _, err := e.a.Do(ctx, id, peerapi.ActionRestart, peerapi.InstanceAction{Epoch: 5, Class: "large"}); err != nil {
		t.Fatal(err)
	}
	if e.plane.all() != "stop "+testRef+",start "+testRef {
		t.Fatalf("restart: %s", e.plane.all())
	}
	p := e.plane.targets[0].Project
	if p.Class != "large" || p.Limits.MemoryMax != largeMemory(t) {
		t.Fatalf("restart rendered %s / %+v", p.Class, p.Limits)
	}
	if row, _ := e.reg.GetProject(ctx, testRef); row.Class != "micro" {
		t.Fatalf("the registry copy changed: %s", row.Class)
	}
	if _, err := e.a.Do(ctx, id, peerapi.ActionRestart, peerapi.InstanceAction{Epoch: 5, Class: "bogus"}); err == nil {
		t.Fatal("an unknown size")
	}

	for _, act := range []peerapi.Action{peerapi.ActionStop, peerapi.ActionStart} {
		if _, err := e.a.Do(ctx, id, act, peerapi.InstanceAction{Epoch: 5}); err != nil {
			t.Fatalf("%s: %v", act, err)
		}
	}
	if _, err := e.a.Do(ctx, id, "explode", peerapi.InstanceAction{Epoch: 5}); !errors.Is(err, lifecycle.ErrInvalidState) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err := e.a.Do(ctx, id, peerapi.ActionStop, peerapi.InstanceAction{Epoch: 4}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch: %v", err)
	}

	// Promote hands the options to the plane and ends the replica instance.
	e.plane.obs.Role = lifecycle.ReplicaRolePrimary
	e.plane.obs.InRecovery = false
	st, err := e.a.Do(ctx, id, peerapi.ActionPromote, peerapi.InstanceAction{Epoch: 5, WaitLSN: "0/3000100", DrainArchive: true, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	if st.Role != "primary" {
		t.Fatalf("after promote: %+v", st)
	}
	if o := e.plane.promote; o.Epoch != 5 || o.WaitLSN != "0/3000100" || !o.DrainArchive || o.Timeout != 30*time.Second {
		t.Fatalf("promote options = %+v", o)
	}
	if e.a.get(id) != nil {
		t.Fatal("a promoted cluster is still a replica instance")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Paths().Project(testRef), "replica.json")); !os.IsNotExist(err) {
		t.Fatalf("replica.json stayed: %v", err)
	}

	// Demote makes the old home a replica, with the identifier the controller gave it.
	newID := registry.ReplicaIdentifier(testRef, "us-east-1", "new123")
	e.plane.obs.Role = lifecycle.ReplicaRoleReplica
	e.plane.obs.InRecovery = true
	if st, err = e.a.Do(ctx, newID, peerapi.ActionDemote, peerapi.InstanceAction{Epoch: 5}); err != nil || st.Role != "replica" || st.Step != StepCompleted {
		t.Fatalf("demote: %+v %v", st, err)
	}
	if in := e.a.get(newID); in == nil {
		t.Fatal("the demoted cluster is not recorded as a replica")
	}
}

// The registry names the new home before the old one is demoted: a demotion of the node the registry
// still names the home would stop its primary.
func TestDemoteIsRefusedWhileTheRegistryNamesThisNodeTheHome(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	id := testReplicaID()
	if err := e.reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Do(ctx, id, peerapi.ActionDemote, peerapi.InstanceAction{Epoch: 5}); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), "n2 is still its home") {
		t.Fatalf("a demotion of the home: %v", err)
	}
	if e.plane.all() != "" {
		t.Fatalf("the plane was driven: %s", e.plane.all())
	}
	if err := e.reg.SetProjectNode(ctx, testRef, "n1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Do(ctx, id, peerapi.ActionDemote, peerapi.InstanceAction{Epoch: 5}); err != nil {
		t.Fatalf("a demotion once the home moved: %v", err)
	}
	if e.plane.all() != "demote "+testRef {
		t.Fatalf("calls = %s", e.plane.all())
	}
}

func TestDoRefusesWhileTheReplicaIsBeingSetUp(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	e.plane.block = make(chan struct{})
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { s, _ := e.a.Observe(ctx, testReplicaID()); return s.Step == StepInitiated })
	for _, act := range []peerapi.Action{peerapi.ActionPromote, peerapi.ActionRestart, peerapi.ActionStop} {
		if _, err := e.a.Do(ctx, testReplicaID(), act, peerapi.InstanceAction{Epoch: 5}); !errors.Is(err, lifecycle.ErrInvalidState) {
			t.Fatalf("%s during the setup: %v", act, err)
		}
	}
	close(e.plane.block)
	e.settle(t) // the setup writes its state in the test's directory
}

func TestObserveOfAReplicaTheNodeHasNoRecordOf(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	st, err := e.a.Observe(ctx, testReplicaID())
	if err != nil || st.Role != "absent" || st.Step != registry.ReplicaStepRequested {
		t.Fatalf("nothing here: %+v %v", st, err)
	}
	// The file was lost but the cluster runs: it is a complete replica.
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleReplica, PostgresUp: true, InRecovery: true, ReceiverStatus: "streaming", ReceiveLSN: "0/5", ReplayLSN: "0/5"}
	if st, err = e.a.Observe(ctx, testReplicaID()); err != nil || st.Step != StepCompleted || st.Role != "replica" || st.ReplayLSN != "0/5" {
		t.Fatalf("a running cluster: %+v %v", st, err)
	}
	if _, err := e.a.Observe(ctx, "nonsense"); err == nil {
		t.Fatal("a malformed identifier")
	}
}

func TestStartLocalStartsTheCompleteReplicasOfThisNode(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	id := testReplicaID()
	if err := e.reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: testRef, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	// Still being set up: left for the leader's next request.
	e.a.StartLocal(ctx)
	if e.plane.all() != "" {
		t.Fatalf("started an unfinished replica: %s", e.plane.all())
	}
	if err := e.reg.SetReplicaStatus(ctx, id, string(registry.StatusActiveHealthy), StepCompleted, ""); err != nil {
		t.Fatal(err)
	}
	e.a.StartLocal(ctx)
	if e.plane.all() != "start "+testRef {
		t.Fatalf("calls = %s", e.plane.all())
	}
	// A failure is logged and does not stop the others.
	e.plane.err["start"] = errors.New("no")
	e.a.StartLocal(ctx)
	// ObserveAll lists what the registry says this node holds.
	if all := e.a.ObserveAll(ctx); len(all) != 1 || all[0].Identifier != id {
		t.Fatalf("ObserveAll = %+v", all)
	}
}

// A replica row that is still in the registry for a cluster that was promoted (the machine restarted
// between the promotion and the move of the home) is not started from the replica's spec: the cluster
// is the project's primary, and a writable cluster on the replica port would run beside the real one.
func TestStartLocalLeavesAClusterThatIsNoStandbyOfTheReplica(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	id := testReplicaID()
	if err := e.reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: testRef, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetReplicaStatus(ctx, id, string(registry.StatusActiveHealthy), StepCompleted, ""); err != nil {
		t.Fatal(err)
	}
	dir := e.cfg.Paths().PostgresData(testRef)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conninfo := func(name string) string {
		return "primary_conninfo = 'host=127.0.0.1 port=1 application_name=''" + name + "'''\n"
	}
	write("PG_VERSION", "17\n")

	// A promoted cluster: no standby.signal.
	e.a.StartLocal(ctx)
	if e.plane.all() != "" {
		t.Fatalf("started a promoted cluster as a replica: %s", e.plane.all())
	}
	// A standby that follows as another replica.
	write("standby.signal", "")
	write("postgresql.auto.conf", conninfo(registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999")))
	e.a.StartLocal(ctx)
	if e.plane.all() != "" {
		t.Fatalf("started the standby of another replica: %s", e.plane.all())
	}
	// This replica's standby.
	write("postgresql.auto.conf", conninfo(id))
	e.a.StartLocal(ctx)
	if e.plane.all() != "start "+testRef {
		t.Fatalf("calls = %s", e.plane.all())
	}
}

func largeMemory(t *testing.T) string {
	t.Helper()
	c, err := lifecycle.ClassFor("large")
	if err != nil {
		t.Fatal(err)
	}
	return c.Limits().MemoryMax
}

// recordingSeeder is a backup.ReplicaSeeder (the contract the backup package implements).
type recordingSeeder struct {
	plan backup.ReplicaSeedPlan
	err  error
}

func (r *recordingSeeder) SeedReplica(_ context.Context, plan backup.ReplicaSeedPlan) error {
	r.plan = plan
	return r.err
}

// The plane's seeder hands the backup service's seeder every field of the plan.
func TestSeederFromPassesThePlanToTheBackupSeeder(t *testing.T) {
	rs := &recordingSeeder{}
	plan := lifecycle.ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID(), DataDir: "/d", BackupID: "b1", PrimaryPort: 20009, ReplicationPassword: "pw"}
	if err := SeederFrom(rs)(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if rs.plan != (backup.ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID(), DataDir: "/d", BackupID: "b1", PrimaryPort: 20009, ReplicationPassword: "pw"}) {
		t.Fatalf("plan = %+v", rs.plan)
	}
	rs.err = errors.New("no base backup")
	if err := SeederFrom(rs)(context.Background(), plan); err == nil || err.Error() != "no base backup" {
		t.Fatalf("error = %v", err)
	}
}

// newAgent is a second agent over the same node: a daemon that restarted keeps nothing in memory.
func (e *agentEnv) newAgent() *agentEnv {
	n := &agentEnv{plane: newFakeReplicaPlane(), reg: e.reg, cfg: e.cfg}
	n.a = NewNodeAgent(AgentOptions{Cfg: e.cfg, Plane: n.plane, Registry: e.reg, Members: members("n2", "n1", 5),
		Keys:   func(context.Context, string) (*secrets.ProjectKeys, error) { return testKeys(), nil },
		Seeder: func(context.Context, lifecycle.ReplicaSeedPlan) error { return nil },
		Poll:   time.Millisecond, StallTimeout: 200 * time.Millisecond})
	return n
}

func TestEnsureFindsTheReplicaThatARestartedDaemonForgot(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	e.settle(t)

	// A request for another replica of the project after the restart finds the first by its file, and
	// the file is not written over.
	n := e.newAgent()
	other := e.spec()
	other.Identifier = registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999")
	if _, err := n.a.Ensure(ctx, other); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), testReplicaID()) {
		t.Fatalf("a second replica after a restart: %v", err)
	}
	b, err := os.ReadFile(n.a.filePath(testRef))
	if err != nil || !strings.Contains(string(b), testReplicaID()) || strings.Contains(string(b), "zzz999") {
		t.Fatalf("replica.json = %s, %v", b, err)
	}
	if n.plane.all() != "" {
		t.Fatalf("the second request reached the plane: %s", n.plane.all())
	}
	// The same replica is still the same.
	if st, err := n.a.Ensure(ctx, e.spec()); err != nil || st.Step != StepCompleted {
		t.Fatalf("the same replica again: %+v %v", st, err)
	}
}

func TestRemoveRemovesOnlyTheReplicaThisNodeHolds(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	other := registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999")
	calls := e.plane.all()
	if err := e.a.Remove(ctx, other); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), testReplicaID()) {
		t.Fatalf("remove of another replica of the project: %v", err)
	}
	// A daemon that forgot the replica finds it by its file just the same.
	n := e.newAgent()
	if err := n.a.Remove(ctx, other); !errors.Is(err, lifecycle.ErrInvalidState) {
		t.Fatalf("remove of another replica after a restart: %v", err)
	}
	if e.plane.all() != calls || n.plane.all() != "" {
		t.Fatalf("a refused removal reached the plane: %s / %s", e.plane.all(), n.plane.all())
	}
	if _, err := os.Stat(e.a.filePath(testRef)); err != nil {
		t.Fatalf("replica.json went: %v", err)
	}
	if err := e.a.Remove(ctx, testReplicaID()); err != nil {
		t.Fatal(err)
	}
}

// writeCluster makes ref's data directory on this node look like a cluster, with the files given.
func writeCluster(t *testing.T, e *agentEnv, files map[string]string) {
	t.Helper()
	dir := e.cfg.Paths().PostgresData(testRef)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveRefusesAPrimaryWhateverTheClusterAnswers(t *testing.T) {
	ctx := context.Background()
	id := testReplicaID()
	standbyConf := func(name string) string {
		return "primary_conninfo = 'host=127.0.0.1 port=20009 user=r password=''x'' application_name=''" + name + "'' sslmode=disable'\n"
	}

	// A promoted replica that was stopped: nothing answers, and nothing is recorded.
	e := newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n", "postgresql.auto.conf": "work_mem = '8MB'\n"})
	if err := e.a.Remove(ctx, id); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("remove of a stopped primary: %v", err)
	}
	if strings.Contains(e.plane.all(), "remove") {
		t.Fatalf("the plane removed it: %s", e.plane.all())
	}

	// The same for a replica whose setup was complete and whose promotion died after standby.signal went.
	e = newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	in := &instance{spec: e.spec(), step: StepCompleted}
	e.a.persist(in)
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n"})
	if err := e.a.Remove(ctx, id); !errors.Is(err, lifecycle.ErrInvalidState) || strings.Contains(e.plane.all(), "remove") {
		t.Fatalf("remove of a complete replica that is a primary on disk: %v", err)
	}

	// A setup that has not finished has no standby.signal yet, and is cleaned up.
	e = newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	e.a.persist(&instance{spec: e.spec(), step: StepInitiated})
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n"})
	if err := e.a.Remove(ctx, id); err != nil || !strings.Contains(e.plane.all(), "remove") {
		t.Fatalf("remove of a setup in progress: %v (%s)", err, e.plane.all())
	}

	// A standby that follows under another identifier is not this one's to delete.
	e = newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n", "standby.signal": "", "postgresql.auto.conf": standbyConf(registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999"))})
	if err := e.a.Remove(ctx, id); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), "zzz999") {
		t.Fatalf("remove of a standby that follows as another replica: %v", err)
	}
	// Its own identifier, or none (an archive-only standby), is removed.
	e = newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n", "standby.signal": "", "postgresql.auto.conf": standbyConf(id)})
	if err := e.a.Remove(ctx, id); err != nil || !strings.Contains(e.plane.all(), "remove") {
		t.Fatalf("remove of its own standby: %v (%s)", err, e.plane.all())
	}
	e = newAgentEnv(t)
	e.plane.obs = lifecycle.ReplicaObservation{Role: lifecycle.ReplicaRoleAbsent}
	writeCluster(t, e, map[string]string{"PG_VERSION": "17\n", "standby.signal": "", "postgresql.auto.conf": "work_mem = '8MB'\n"})
	if err := e.a.Remove(ctx, id); err != nil || !strings.Contains(e.plane.all(), "remove") {
		t.Fatalf("remove of an archive-only standby: %v (%s)", err, e.plane.all())
	}
}

func TestStandbyName(t *testing.T) {
	dir := t.TempDir()
	for conf, want := range map[string]string{
		"primary_conninfo = 'host=h application_name=''abc-rr-us-east-1-abc123'' sslmode=disable'\n": "abc-rr-us-east-1-abc123",
		"primary_conninfo = 'application_name=plain'\n":                                              "plain",
		"work_mem = '8MB'\n":                          "",
		"primary_conninfo = 'host=h'\n":               "",
		"# primary_conninfo = 'application_name=x'\n": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := standbyName(dir); got != want {
			t.Errorf("standbyName(%q) = %q, want %q", conf, got, want)
		}
	}
	if got := standbyName(t.TempDir()); got != "" {
		t.Errorf("no file: %q", got)
	}
}

func TestDoRefusesAReplicaActionOnTheHome(t *testing.T) {
	ctx := context.Background()
	e := newAgentEnv(t)
	if _, err := e.a.Ensure(ctx, e.spec()); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	// The registry moved the home here (a stale or misdirected request must not render the replica's
	// spec over the primary, nor stop it).
	if err := e.reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	e.plane.mu.Lock()
	e.plane.calls, e.plane.targets = nil, nil
	e.plane.mu.Unlock()
	id := testReplicaID()
	for _, act := range []peerapi.Action{peerapi.ActionStart, peerapi.ActionRestart, peerapi.ActionStop} {
		if _, err := e.a.Do(ctx, id, act, peerapi.InstanceAction{Epoch: 5}); !errors.Is(err, lifecycle.ErrInvalidState) || !strings.Contains(err.Error(), "is its home") {
			t.Errorf("%s on the home: %v", act, err)
		}
	}
	// A promotion of a cluster that is not a promoted replica is refused...
	if _, err := e.a.Do(ctx, id, peerapi.ActionPromote, peerapi.InstanceAction{Epoch: 5}); !errors.Is(err, lifecycle.ErrInvalidState) {
		t.Fatalf("promote on the home: %v", err)
	}
	if e.plane.all() != "" {
		t.Fatalf("the plane was asked: %s", e.plane.all())
	}
	if e.a.get(id) == nil {
		t.Fatal("the replica instance went with a refused promotion")
	}
	// ...and one repeated after the switchover found the home here and the cluster a primary has
	// nothing left to do, but ends the instance.
	e.plane.obs.Role, e.plane.obs.InRecovery = lifecycle.ReplicaRolePrimary, false
	st, err := e.a.Do(ctx, id, peerapi.ActionPromote, peerapi.InstanceAction{Epoch: 5})
	if err != nil || st.Role != "primary" {
		t.Fatalf("a repeated promotion: %+v %v", st, err)
	}
	if strings.Contains(e.plane.all(), "promote") || e.a.get(id) != nil {
		t.Fatalf("calls %s, instance %v", e.plane.all(), e.a.get(id))
	}
}
