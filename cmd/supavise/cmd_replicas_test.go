package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
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
