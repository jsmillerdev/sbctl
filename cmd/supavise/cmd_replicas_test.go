package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
	"github.com/supavise/supavise/internal/replicas/replicaid"
)

const replicaTestRef = "aaaaaaaaaaaaaaaaaaaa"

// replicaTestEnv points the replicas commands at a registry in memory: a leader n1 with one small
// project, and the nodes n2 "eu" (eu-west-1) and n3 "ap" (ap-south-1).
func replicaTestEnv(t *testing.T) (*registry.Memory, string) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	cfg := config.Default()
	cfg.Backup.Backend = "s3://bucket/prefix"
	if err := reg.UpdateNode(ctx, &registry.Node{ID: "n1", Name: "primary", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []registry.Node{{ID: "n2", Name: "eu", Region: "eu-west-1"}, {ID: "n3", Name: "ap", Region: "ap-south-1"}} {
		n.State = registry.NodeActive
		if err := reg.CreateNode(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.CreateProject(ctx, &registry.Project{Ref: replicaTestRef, Name: "app", Class: "small", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), replicas.SnapshotName)
	old := openReplicas
	openReplicas = func(context.Context) (*replicasEnv, error) {
		return &replicasEnv{svc: replicas.New(replicas.Options{Registry: reg, Config: cfg}), reg: reg, snapshot: snap, close: func() {}}, nil
	}
	t.Cleanup(func() { openReplicas = old })
	return reg, snap
}

func TestReplicasAddLsRm(t *testing.T) {
	reg, snap := replicaTestEnv(t)
	ctx := context.Background()

	out, err := run(t, "replicas", "ls")
	if err != nil || !strings.Contains(out, "no read replicas") {
		t.Fatalf("ls on an empty cluster: %q %v", out, err)
	}

	out, err = run(t, "replicas", "add", replicaTestRef, "--region", "eu-west-1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	rs, _ := reg.ListReplicas(ctx, replicaTestRef)
	if len(rs) != 1 || rs[0].NodeID != "n2" || rs[0].Status != registry.ReplicaInit || rs[0].InitStep != replicas.StepRequested {
		t.Fatalf("rows: %+v", rs)
	}
	id := rs[0].Identifier
	if !strings.Contains(out, "requested "+id+" on node n2") {
		t.Fatalf("add printed %q", out)
	}

	// ls shows the row, and the lag from the daemon's snapshot.
	lag := 3.0
	b, _ := json.Marshal(replicas.Snapshot{At: time.Now(), Replicas: []replicas.SnapshotEntry{{Identifier: id, Receiver: "streaming", LagSeconds: &lag}}})
	if err := os.WriteFile(snap, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "replicas", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"IDENTIFIER", id, replicaTestRef, "n2 (eu)", "eu-west-1", "manual", "INIT_READ_REPLICA", "0_requested", "3s"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	out, err = run(t, "replicas", "ls", replicaTestRef, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []replicaRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("json: %q %v", out, err)
	}
	if r := rows[0]; r.Identifier != id || r.NodeName != "eu" || r.Receiver != "streaming" || r.LagSeconds == nil || *r.LagSeconds != 3 || r.Region != "eu-west-1" {
		t.Fatalf("row: %+v", r)
	}
	if out, _ := run(t, "replicas", "ls", "bbbbbbbbbbbbbbbbbbbb"); !strings.Contains(out, "no read replicas") {
		t.Fatalf("ls of another project: %q", out)
	}

	// add --node by name; the region must match the node's.
	if _, err := run(t, "replicas", "add", replicaTestRef, "--region", "eu-west-1", "--node", "ap"); err == nil || !strings.Contains(err.Error(), "node ap is in ap-south-1, not in eu-west-1") {
		t.Fatalf("add with a node in another region: %v", err)
	}
	if out, err := run(t, "replicas", "add", replicaTestRef, "--region", "ap-south-1", "--node", "ap"); err != nil || !strings.Contains(out, "on node n3") {
		t.Fatalf("add --node: %q %v", out, err)
	}

	// The refusals of the service come back as its message.
	if _, err := run(t, "replicas", "add", replicaTestRef, "--region", "eu-west-1"); err == nil || err.Error() != "This project already has a replica on eu." {
		t.Fatalf("second add: %v", err)
	}
	if _, err := run(t, "replicas", "add", replicaTestRef, "--region", "sa-east-1"); err == nil || err.Error() != "No Supavise server is joined in sa-east-1." {
		t.Fatalf("add in an empty region: %v", err)
	}

	// rm
	if _, err := run(t, "replicas", "rm", "--yes", "not-an-identifier"); err == nil || !strings.Contains(err.Error(), "is not a replica identifier") {
		t.Fatalf("rm of garbage: %v", err)
	}
	if _, err := run(t, "replicas", "rm", "--yes", replicaTestRef+"-rr-eu-west-1-zzzzzz"); err == nil || !strings.Contains(err.Error(), "no read replica") {
		t.Fatalf("rm of an unknown replica: %v", err)
	}
	out, err = run(t, "replicas", "rm", "--yes", id)
	if err != nil || !strings.Contains(out, "removing "+id) {
		t.Fatalf("rm: %q %v", out, err)
	}
	if r, _ := reg.GetReplica(ctx, id); r == nil || r.Status != string(registry.StatusGoingDown) {
		t.Fatalf("row after rm: %+v", r)
	}
}

// Without --yes, rm asks; an empty answer, or a stdin that is no terminal, changes nothing.
func TestReplicasRmAsksFirst(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	if _, err := run(t, "replicas", "add", replicaTestRef, "--region", "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	rs, _ := reg.ListReplicas(context.Background(), replicaTestRef)
	if _, err := run(t, "replicas", "rm", rs[0].Identifier); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("rm without --yes: %v", err)
	}
	if r, _ := reg.GetReplica(context.Background(), rs[0].Identifier); r.Status == string(registry.StatusGoingDown) {
		t.Fatal("removed without being confirmed")
	}
}

// While the command waits, the daemon can make rows of its own (the default replicas). The command
// prints the identifier of the row its request made, not the first new one it finds.
func TestReplicasAddPrintsItsOwnRow(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	re, err := openReplicas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n3, _ := reg.GetNode(ctx, "n3")
	re.svc = beforeSetup{Service: re.svc, do: func() {
		if _, err := replicaid.Create(ctx, reg, replicaTestRef, *n3, registry.ReplicaDefault, nil); err != nil {
			t.Error(err)
		}
	}}
	var out strings.Builder
	if err := addReplica(ctx, &out, re, replicaTestRef, "eu-west-1", ""); err != nil {
		t.Fatal(err)
	}
	rs, _ := reg.ListReplicas(ctx, replicaTestRef)
	var manual registry.Replica
	for _, r := range rs {
		if r.Origin == registry.ReplicaManual {
			manual = r
		}
	}
	if len(rs) != 2 || manual.NodeID != "n2" || !strings.Contains(out.String(), "requested "+manual.Identifier+" on node n2") {
		t.Fatalf("rows %+v, printed %q", rs, out.String())
	}
}

// beforeSetup runs do just before the request it wraps.
type beforeSetup struct {
	replicas.Service
	do func()
}

func (b beforeSetup) Setup(ctx context.Context, ref, region string) error {
	b.do()
	return b.Service.Setup(ctx, ref, region)
}

func TestStepText(t *testing.T) {
	for in, want := range map[replicaRow]string{
		{Step: replicas.StepDone}:                                "done",
		{Step: replicas.StepInitiated}:                           replicas.StepInitiated,
		{Step: replicas.StepStarted, Error: replicas.FailLaunch}: replicas.StepStarted + " (" + replicas.FailLaunch + ")",
	} {
		if got := stepText(in); got != want {
			t.Errorf("stepText(%+v) = %q, want %q", in, got, want)
		}
	}
}

// A follower's registry is read-only: the command says where to run.
func TestReplicasWriteOnAFollower(t *testing.T) {
	if got := leaderOnly(registry.ErrReadOnly); got == nil || !strings.Contains(got.Error(), "run the command on the leader") {
		t.Fatalf("leaderOnly = %v", got)
	}
	other := errors.New("boom")
	if leaderOnly(other) != other {
		t.Fatal("another error must pass through")
	}
}

// A node that names no region is in the default one, for --node as for the service.
func TestReplicasAddNodeWithoutRegion(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	if err := reg.CreateNode(context.Background(), &registry.Node{ID: "n4", Name: "bare", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "replicas", "add", replicaTestRef, "--region", "eu-west-1", "--node", "bare"); err == nil || !strings.Contains(err.Error(), "node bare is in us-east-1, not in eu-west-1") {
		t.Fatalf("add with the wrong region: %v", err)
	}
	if out, err := run(t, "replicas", "add", replicaTestRef, "--region", config.DefaultRegion, "--node", "bare"); err != nil || !strings.Contains(out, "on node n4") {
		t.Fatalf("add --node: %q %v", out, err)
	}
}

// A delete or an in-place restore removes the project's replicas first, as the Management API does. This
// process cannot reach the other nodes: it marks the replicas GOING_DOWN, stops with their names, and
// goes on once the controller has removed them.
func TestRemoveReplicasFirstMarksTheReplicasAndStopsWhileTheyRemain(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	cfg := config.Default()

	// No replicas: nothing to wait for.
	if err := removeReplicasFirst(ctx, reg, cfg, replicaTestRef); err != nil {
		t.Fatalf("a project with no replicas: %v", err)
	}

	id := registry.ReplicaIdentifier(replicaTestRef, "eu-west-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: replicaTestRef, NodeID: "n2", Origin: registry.ReplicaManual,
		Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	err := removeReplicasFirst(ctx, reg, cfg, replicaTestRef)
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "still being removed") {
		t.Fatalf("a replica that cannot be reached: %v", err)
	}
	var pe *replicas.PendingError
	if errors.As(err, &pe) {
		t.Fatal("the pending error leaks out of the command: it is explained")
	}
	r, err := reg.GetReplica(ctx, id)
	if err != nil || r.Status != "GOING_DOWN" {
		t.Fatalf("the replica after the refusal: %+v, %v", r, err)
	}

	// The controller removed it (its row is gone): the command goes on.
	if err := reg.DeleteReplica(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := removeReplicasFirst(ctx, reg, cfg, replicaTestRef); err != nil {
		t.Fatalf("after the removal: %v", err)
	}
}

// readOnlyReplicas is a registry that refuses the writes of a replica's status, as the copy on a follower does.
type readOnlyReplicas struct{ registry.Registry }

func (readOnlyReplicas) SetReplicaStatus(context.Context, string, string, string, string) error {
	return registry.ErrReadOnly
}

func (readOnlyReplicas) SetReplicaStatusUnlessGoingDown(context.Context, string, string, string, string) (bool, error) {
	return false, registry.ErrReadOnly
}

// On a follower nothing can mark a replica GOING_DOWN, and no daemon here will finish the removal: the
// command sends the operator to the leader and does not promise that running it again will help.
func TestRemoveReplicasFirstOnAFollowerSendsYouToTheLeader(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	id := registry.ReplicaIdentifier(replicaTestRef, "eu-west-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: replicaTestRef, NodeID: "n2", Origin: registry.ReplicaManual,
		Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	err := removeReplicasFirst(ctx, readOnlyReplicas{reg}, config.Default(), replicaTestRef)
	if !errors.Is(err, registry.ErrReadOnly) {
		t.Fatalf("removal on a follower: %v", err)
	}
	if strings.Contains(err.Error(), "run this command again") {
		t.Errorf("a follower is told to wait for a removal that will not happen: %v", err)
	}
	if got := followerHint(err); got == nil || !strings.Contains(got.Error(), "run it on the leader") {
		t.Errorf("main's explanation: %v", got)
	}
	if r, err := reg.GetReplica(ctx, id); err != nil || r.Status != "ACTIVE_HEALTHY" {
		t.Errorf("the replica was changed: %+v, %v", r, err)
	}
}

// An in-place restore checks the project before it touches its replicas: one that is not running is
// refused with them untouched, as the API refuses it, and a project whose last restore failed may be
// restored again.
func TestPrepareInPlaceRestoreChecksTheProjectBeforeItsReplicas(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	cfg := config.Default()
	id := registry.ReplicaIdentifier(replicaTestRef, "eu-west-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: replicaTestRef, NodeID: "n2", Origin: registry.ReplicaManual,
		Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	if err := prepareInPlaceRestore(ctx, reg, cfg, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("an unknown project: %v", err)
	}
	for _, st := range []registry.Status{registry.StatusInactive, registry.StatusRestoring} {
		if err := reg.SetProjectStatus(ctx, replicaTestRef, st); err != nil {
			t.Fatal(err)
		}
		err := prepareInPlaceRestore(ctx, reg, cfg, replicaTestRef)
		if err == nil || !strings.Contains(err.Error(), "while it is "+string(st)) {
			t.Fatalf("a project that is %s: %v", st, err)
		}
		if r, err := reg.GetReplica(ctx, id); err != nil || r.Status != "ACTIVE_HEALTHY" {
			t.Fatalf("a refused restore touched the replica (%s): %+v, %v", st, r, err)
		}
	}
	if err := reg.SetProjectStatus(ctx, replicaTestRef, registry.StatusRestoreFailed); err != nil {
		t.Fatal(err)
	}
	// Restorable: the replica is marked and the restore waits for the controller to remove it.
	if err := prepareInPlaceRestore(ctx, reg, cfg, replicaTestRef); err == nil || !strings.Contains(err.Error(), "still being removed") {
		t.Fatalf("a project whose restore failed: %v", err)
	}
	if r, err := reg.GetReplica(ctx, id); err != nil || r.Status != "GOING_DOWN" {
		t.Fatalf("the replica after the restore started: %+v, %v", r, err)
	}
}

// The delete never reaches the Engine while a replica remains (a nil Engine would stop the test).
func TestDeleteProjectStopsWhileReplicasRemain(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	id := registry.ReplicaIdentifier(replicaTestRef, "eu-west-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: replicaTestRef, NodeID: "n2", Origin: registry.ReplicaManual,
		Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	n := &lifecycle.Node{Registry: reg, Cfg: config.Default()}
	if err := deleteProject(ctx, n, replicaTestRef, false); err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("delete with a replica: %v", err)
	}
}

// `orgs delete` removes each project's replicas before the project, as the API's organization delete does:
// the organization and the project stay, the replica is marked GOING_DOWN, and the Engine is never
// reached (a nil Engine would stop the test). Deleting the project row would take the replica rows with
// it and leave their instances on the other nodes.
func TestOrgDeleteStopsAtAProjectWhoseReplicasRemain(t *testing.T) {
	reg, _ := replicaTestEnv(t)
	ctx := context.Background()
	if _, err := reg.CreateOrganization(ctx, "keep", "Keep"); err != nil {
		t.Fatal(err)
	}
	scrap, err := reg.CreateOrganization(ctx, "scrap", "Scrap")
	if err != nil {
		t.Fatal(err)
	}
	const ref = "bbbbbbbbbbbbbbbbbbbb"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: scrap.ID, Name: "scrap app", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	id := registry.ReplicaIdentifier(ref, "eu-west-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: "n2", Origin: registry.ReplicaManual,
		Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	n := &lifecycle.Node{Registry: reg, Cfg: config.Default()}
	d := &api.OrgDeleter{Reg: reg, DeleteProject: orgProjectDeleter(n, func(context.Context, string) error {
		t.Error("a branch is not what this organization holds")
		return nil
	}, &out, &errw)}
	_, err = d.Delete(ctx, nil, scrap)
	var oe *api.OrgDeleteError
	if !errors.As(err, &oe) || oe.Ref != ref || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "still being removed") {
		t.Fatalf("org delete with a replica that cannot be reached: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the project is reported deleted: %q", out.String())
	}
	if _, err := reg.GetOrganization(ctx, "scrap"); err != nil {
		t.Errorf("the organization went with a project that was not deleted: %v", err)
	}
	if _, err := reg.GetProject(ctx, ref); err != nil {
		t.Errorf("the project: %v", err)
	}
	if r, err := reg.GetReplica(ctx, id); err != nil || r.Status != "GOING_DOWN" {
		t.Errorf("the replica after the refusal: %+v, %v", r, err)
	}
}

func TestOnlyAnInPlaceRestoreRemovesTheReplicas(t *testing.T) {
	for _, c := range []struct {
		as   string
		want bool
	}{{"", true}, {replicaTestRef, true}, {"bbbbbbbbbbbbbbbbbbbb", false}} {
		if got := restoresInPlace(replicaTestRef, c.as); got != c.want {
			t.Errorf("--as %q: in place = %v, want %v", c.as, got, c.want)
		}
	}
}
