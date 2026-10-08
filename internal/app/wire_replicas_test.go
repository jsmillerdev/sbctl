package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
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

// recSink is a replicas.ReportSink that records the reports it is handed.
type recSink struct {
	mu   sync.Mutex
	reps []peerapi.Report
	got  chan struct{}
}

func (s *recSink) HandleReport(_ context.Context, rep peerapi.Report) {
	s.mu.Lock()
	s.reps = append(s.reps, rep)
	s.mu.Unlock()
	select {
	case s.got <- struct{}{}:
	default:
	}
}

// What a node reports reaches the controller with the node the leader authenticated, whatever the body
// says, and the intake of the peer API does not wait for the controller.
func TestReportIntakeHandsTheReportOfTheAuthenticatedNodeToTheSink(t *testing.T) {
	sink := &recSink{got: make(chan struct{}, 8)}
	in := newReportIntake(sink)
	reports := cluster.NewReports()
	reports.Subscribe(in.Put)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.Run(ctx)

	// The peer API stores the report under the node of the certificate; the body's Node is not taken.
	reports.Put("n2", peerapi.Report{Node: "n3", Instances: []peerapi.InstanceStatus{{Identifier: "x"}}}, time.Now())
	select {
	case <-sink.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller was not handed the report")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reps) != 1 || sink.reps[0].Node != "n2" || len(sink.reps[0].Instances) != 1 {
		t.Fatalf("reports: %+v", sink.reps)
	}
}

// A slow controller costs the intake nothing, and only the latest report of a node is kept.
func TestReportIntakeNeverBlocksAndKeepsTheLatestOfANode(t *testing.T) {
	release := make(chan struct{})
	sink := &blockingSink{release: release}
	in := newReportIntake(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.Run(ctx)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			in.Put(peerapi.Report{Node: "n2", Epoch: int64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Put blocked behind the controller")
	}
	close(release)
	eventually(t, "the latest report to be handed over", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.epochs) > 0 && sink.epochs[len(sink.epochs)-1] == 49
	})
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.epochs) > 3 {
		t.Fatalf("%d reports of one node were handed over; the intake keeps the latest", len(sink.epochs))
	}
}

type blockingSink struct {
	release chan struct{}
	mu      sync.Mutex
	epochs  []int64
}

func (s *blockingSink) HandleReport(_ context.Context, rep peerapi.Report) {
	<-s.release
	s.mu.Lock()
	s.epochs = append(s.epochs, rep.Epoch)
	s.mu.Unlock()
}

// On a node of a cluster the Management API is given the controller; on a single server it is not.
func TestWireReplicasGivesTheManagementAPIItsControllerOnAClusterNodeOnly(t *testing.T) {
	w := testWire(t)
	if err := wireReplicas(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if w.API.Replicas != nil || w.API.Placement != nil {
		t.Fatal("a single server's Management API got the controller")
	}
	w2 := testWire(t)
	Provide[mesh.Mesh](w2, stubMesh{})
	Provide[placement.Resolver](w2, placement.RegistryResolver{Reg: w2.Node.Registry})
	reports := cluster.NewReports()
	Provide(w2, reports)
	if err := wireReplicas(context.Background(), w2); err != nil {
		t.Fatal(err)
	}
	if w2.API.Replicas == nil || w2.API.Placement == nil {
		t.Fatal("a cluster node's Management API has no controller")
	}
	var names []string
	for _, r := range w2.runners {
		names = append(names, r.name)
	}
	if strings.Join(names, ",") != "replica report intake" {
		t.Fatalf("runners %v: the controller needs node operations and base backups to run", names)
	}
	if r, off := w2.offReason("replica controller"); !off || !strings.Contains(r, "base backups") {
		t.Fatalf("controller off: %q, %v", r, off)
	}
}
