package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

type noOps struct{ placement.InstanceOps }

type noBackups struct{}

func (noBackups) EnsureBase(context.Context, string, time.Duration) (*registry.Backup, error) {
	return nil, errors.New("not used")
}

// A node with no cluster gets the service, answers a setup request with the refusal Studio shows,
// and runs nothing.
func TestWireReplicasOnASingleNode(t *testing.T) {
	w := testWire(t)
	ctx := context.Background()
	const ref = "aaaaaaaaaaaaaaaaaaaa"
	if err := w.Node.Registry.CreateProject(ctx, &registry.Project{Ref: ref, Name: "a", Class: "small", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	w.Cfg.Backup.Backend = "s3://bucket/prefix"
	if err := wireReplicas(ctx, w); err != nil {
		t.Fatal(err)
	}
	svc, ok := Get[replicas.Service](w)
	if !ok {
		t.Fatal("no replicas.Service provided")
	}
	if _, ok := Get[replicas.Remover](w); !ok {
		t.Fatal("no replicas.Remover provided")
	}
	if _, ok := Get[replicas.ReportSink](w); !ok {
		t.Fatal("no replicas.ReportSink provided")
	}
	var ue *replicas.UserError
	if err := svc.Setup(ctx, ref, "eu-west-1"); !errors.As(err, &ue) || ue.Msg != "No Supavise server is joined in eu-west-1." {
		t.Fatalf("Setup = %v", err)
	}
	if len(w.runners) != 0 {
		t.Fatalf("a single node started %d runners", len(w.runners))
	}
}

// With the node operations and the base backups in place, the controller runs in the daemon's group.
func TestWireReplicasRunsTheControllerWhenItCanReachNodes(t *testing.T) {
	w := testWire(t)
	Provide[placement.InstanceOps](w, noOps{})
	Provide[backup.BaseBackupEnsurer](w, noBackups{})
	if err := wireReplicas(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 1 || w.runners[0].name != "replicas" {
		t.Fatalf("runners: %+v", w.runners)
	}
	// Stopping the daemon is not an error of the controller.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.runners[0].fn(ctx); err != nil {
		t.Fatalf("the controller returned %v at shutdown", err)
	}
}

// Without base backups the controller does not run; the service still answers.
func TestWireReplicasNeedsBaseBackups(t *testing.T) {
	w := testWire(t)
	Provide[placement.InstanceOps](w, noOps{})
	if err := wireReplicas(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 0 {
		t.Fatalf("runners: %+v", w.runners)
	}
	if _, ok := Get[replicas.Service](w); !ok {
		t.Fatal("no service")
	}
}

// The leader knows its own machine and nothing of the others.
func TestLocalRoom(t *testing.T) {
	w := testWire(t)
	Provide[cluster.Membership](w, cluster.Solo(registry.Node{ID: "n1", Name: "primary"}))
	w.Cfg.StateDir = t.TempDir()
	room := localRoom(w)
	self := room(context.Background(), registry.Node{ID: "n1"})
	if !self.DiskKnown || self.FreeDiskBytes <= 0 || self.Resources.MemoryBytes < 0 {
		t.Fatalf("own room: %+v", self)
	}
	if other := room(context.Background(), registry.Node{ID: "n2"}); other != (replicas.Room{}) {
		t.Fatalf("a remote node's room is unknown, got %+v", other)
	}
}
