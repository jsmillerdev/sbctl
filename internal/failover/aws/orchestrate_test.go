package aws_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	failoveraws "github.com/supavise/supavise/internal/failover/aws"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// These tests run the orchestrator and the monitor against the real AWS provider over awsapi's
// fake EC2, with fakes for everything else, and look at the order in which things happened.

type rig struct {
	t       *testing.T
	fake    *awsfake.Server
	reg     *registry.Memory
	members *cluster.Static
	o       *failover.Orchestrator
	mon     *failover.Monitor

	mu       sync.Mutex
	events   []string
	pingErr  error
	probeErr error
	promoted map[string][]string // identifier: the EC2 calls made by the time it was promoted
	clock    time.Time
	alerts   []alerts.Event
}

const sysID = "system-rr-us-east-1-s2s2s2"

func newRig(t *testing.T, mode string) *rig {
	t.Helper()
	ctx := context.Background()
	r := &rig{t: t, fake: awsfake.New(t), reg: registry.NewMemory(), promoted: map[string][]string{}, clock: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	r.pingErr = errors.New("no session")
	r.probeErr = errors.New("connection refused")
	r.fake.AddInstance(awsfake.Instance{ID: "i-n1", PrivateIP: "10.77.0.10", StopPolls: 1})
	r.fake.AddInstance(awsfake.Instance{ID: "i-n2", PrivateIP: "10.77.0.20", SecondaryIPs: []string{"10.77.0.21"}})
	r.fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.9", InstanceID: "i-n1"})
	r.fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-n2", PublicIP: "203.0.113.20", InstanceID: "i-n2"})

	n1, err := r.reg.GetNode(ctx, registry.FounderNodeID)
	must(t, err)
	n1.Region, n1.Version = "us-east-1", "v0.2.0"
	n1.Provider = registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n1", Region: "us-east-1"}}
	must(t, r.reg.UpdateNode(ctx, n1))
	must(t, r.reg.CreateNode(ctx, &registry.Node{ID: "n2", Name: "standby", Region: "us-east-1", Version: "v0.2.0", State: registry.NodeActive,
		Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n2", Region: "us-east-1"}}}))
	must(t, r.reg.SetServiceAddress(ctx, registry.ServiceAddress{IP: "203.0.113.9", AllocationID: "eipalloc-svc"}))
	must(t, r.reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}))
	must(t, r.reg.CreateReplica(ctx, &registry.Replica{Identifier: sysID, Ref: config.SystemRef, NodeID: "n2", Origin: registry.ReplicaSystem, Status: "ACTIVE_HEALTHY", InitStep: registry.ReplicaStepDone}))

	nodes, _ := r.reg.ListNodes(ctx)
	var n2 registry.Node
	for _, n := range nodes {
		if n.ID == "n2" {
			n2 = n
		}
	}
	r.members = cluster.NewStatic(cluster.Snapshot{Self: n2, Nodes: nodes, Leader: "n1", Epoch: 1, Role: cluster.RoleFollower})

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	cfg.Fleet.StorageBackend = "s3"
	cfg.Failover = config.Failover{Mode: mode, Fencing: config.FencingAWS, MaxLagSeconds: 30, GraceSeconds: 90, ProjectGraceSeconds: 180, StopTimeoutSeconds: 5, CooldownMinutes: 60}

	client := r.fake.Client()
	prov := &failoveraws.Provider{
		EC2: func(string) (failoveraws.EC2, error) { return client.EC2, nil },
		Cluster: func(ctx context.Context) (failoveraws.Cluster, error) {
			cl, _ := r.reg.GetCluster(ctx)
			ns, _ := r.reg.ListNodes(ctx)
			return failoveraws.Cluster{Self: n2, Peers: ns, Service: cl.ServiceAddress}, nil
		},
		StopTimeout: time.Second, Poll: time.Millisecond,
	}
	o, err := failover.New(failover.Deps{
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: func() failover.Store { return r.reg }, Members: r.members,
		Instances: (*instances)(r), Peers: (*peers)(r), Marker: &marker{}, Provider: prov,
		Takeover:    takeover{},
		PublicProbe: func(context.Context) error { r.mu.Lock(); defer r.mu.Unlock(); return r.probeErr },
		Notify:      func(_ context.Context, ev alerts.Event) { r.mu.Lock(); r.alerts = append(r.alerts, ev); r.mu.Unlock() },
		Now:         func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock },
		Sleep:       func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	})
	must(t, err)
	r.o = o
	r.mon = failover.NewMonitor(o)
	return r
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.clock = r.clock.Add(d)
	r.mu.Unlock()
}

func (r *rig) ec2() []string { return r.fake.Order("ec2") }

// instances is the standby of the system cluster on n2.
type instances rig

func (i *instances) status(id string, promoted bool) peerapi.InstanceStatus {
	lag := 0.2
	return peerapi.InstanceStatus{Identifier: id, Ref: config.SystemRef, Role: "replica", PostgresUp: true, InRecovery: !promoted, LagSeconds: &lag}
}

func (i *instances) Observe(_ context.Context, _, id string) (peerapi.InstanceStatus, error) {
	r := (*rig)(i)
	r.mu.Lock()
	_, done := r.promoted[id]
	r.mu.Unlock()
	return i.status(id, done), nil
}

func (i *instances) Ensure(context.Context, string, peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	return peerapi.InstanceStatus{}, errors.New("not used")
}

func (i *instances) Do(_ context.Context, node, id string, a peerapi.Action, _ peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	r := (*rig)(i)
	if a != peerapi.ActionPromote {
		return peerapi.InstanceStatus{}, fmt.Errorf("unexpected %s", a)
	}
	// What EC2 had been asked by the time the standby was promoted.
	calls := r.fake.Order("ec2")
	r.mu.Lock()
	r.promoted[id] = calls
	r.events = append(r.events, "promote "+id)
	r.mu.Unlock()
	return i.status(id, true), nil
}

// peers: the old leader does not answer the mesh.
type peers rig

func (p *peers) Ping(context.Context, string) (peerapi.Ping, error) {
	r := (*rig)(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pingErr == nil {
		return peerapi.Ping{Node: "n1", Epoch: 1, Leader: "n1"}, nil
	}
	return peerapi.Ping{}, r.pingErr
}

func (p *peers) Fence(context.Context, string, failover.FenceCall) (peerapi.FenceResponse, error) {
	return peerapi.FenceResponse{}, errors.New("no session")
}

type marker struct {
	mu sync.Mutex
	m  *backup.LeaderMarker
}

func (m *marker) ReadLeaderMarker(context.Context) (*backup.LeaderMarker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.m, nil
}

func (m *marker) WriteLeaderMarker(_ context.Context, lm backup.LeaderMarker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m = &lm
	return nil
}

type takeover struct{}

func (takeover) BecomeLeader(context.Context, int64) error { return nil }

func TestUnplannedFailoverStopsWaitsAssociatesThenPromotes(t *testing.T) {
	r := newRig(t, config.FailoverManual)
	mv, err := r.o.FailoverServer(context.Background(), failover.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone || mv.Kind != registry.MoveFailover {
		t.Fatalf("move: %+v", mv)
	}
	byPromotion, ok := r.promoted[sysID]
	if !ok {
		t.Fatal("the system cluster was not promoted")
	}
	// By the time the standby is promoted EC2 has been told, in this order: stop the old leader,
	// look until it is stopped, associate the service address. (The probe's DryRun calls and the
	// preflight's describes come first and are not part of the order that matters.)
	var real []string
	for _, c := range r.fake.Calls() {
		if c.Service == "ec2" && !c.DryRun {
			real = append(real, c.Action)
		}
	}
	stop := slices.Index(real, "StopInstances")
	assoc := slices.Index(real, "AssociateAddress")
	if stop < 0 || assoc < 0 || stop > assoc {
		t.Fatalf("calls: %v", real)
	}
	described := false
	for _, a := range real[stop+1 : assoc] {
		if a == "DescribeInstances" {
			described = true
		}
	}
	if !described {
		t.Fatalf("the address was associated without waiting for the stop to show: %v", real)
	}
	if !slices.Contains(byPromotion, "ec2:AssociateAddress") || !slices.Contains(byPromotion, "ec2:StopInstances") {
		t.Fatalf("by the promotion EC2 had seen only %v", byPromotion)
	}
	if st := r.fake.InstanceState("i-n1"); st != "stopped" {
		t.Fatalf("n1 is %s", st)
	}
	if a := r.fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-n2" || a.PrivateIP != "10.77.0.21" {
		t.Fatalf("service address: %+v", a)
	}
}

func TestMonitorNeverStopsAPartitionedButRunningPeer(t *testing.T) {
	r := newRig(t, config.FailoverServer)
	ctx := context.Background()
	d := r.mon.Tick(ctx) // the first silent look
	if d.Action != "none" {
		t.Fatalf("first look: %+v", d)
	}
	r.advance(5 * time.Minute) // well past the grace period

	// The mesh is partitioned and the public probe fails, but EC2 says the peer runs and passes its checks.
	d = r.mon.Tick(ctx)
	if d.Action != "none" || d.Reason == "" {
		t.Fatalf("decision: %+v", d)
	}
	for _, c := range r.fake.Order("ec2") {
		if c == "ec2:StopInstances" || c == "ec2:AssociateAddress" {
			t.Fatalf("a running peer was acted on: %v", r.fake.Order("ec2"))
		}
	}
	if st := r.fake.InstanceState("i-n1"); st != "running" {
		t.Fatalf("n1 is %s", st)
	}
	if len(r.promoted) != 0 {
		t.Fatal("something was promoted")
	}

	// The public address answers: the leader serves its clients, whatever the mesh says.
	r.mu.Lock()
	r.probeErr = nil
	r.mu.Unlock()
	r.fake.UpdateInstance("i-n1", func(i *awsfake.Instance) { i.State = "stopped" })
	if d := r.mon.Tick(ctx); d.Action != "none" {
		t.Fatalf("a leader whose address answers is not failed over: %+v", d)
	}
	if len(r.promoted) != 0 {
		t.Fatal("something was promoted")
	}
}

func TestMonitorFailsOverADeadLeaderOnceEveryGateSaysSo(t *testing.T) {
	r := newRig(t, config.FailoverServer)
	ctx := context.Background()
	r.mon.Tick(ctx)
	r.advance(2 * time.Minute)
	// EC2 reports the peer stopped: the machine is down.
	r.fake.UpdateInstance("i-n1", func(i *awsfake.Instance) { i.State = "stopped" })
	d := r.mon.Tick(ctx)
	if d.Action != "server" || d.Err != nil {
		t.Fatalf("decision: %+v", d)
	}
	if _, ok := r.promoted[sysID]; !ok {
		t.Fatal("the system cluster was not promoted")
	}
	if a := r.fake.AddressOf("eipalloc-svc"); a.InstanceID != "i-n2" {
		t.Fatalf("service address: %+v", a)
	}
	// And it does not do it again: the cooldown and the changed leader both say no.
	if d := r.mon.Tick(ctx); d.Action != "none" {
		t.Fatalf("a second look: %+v", d)
	}
}

func TestAnImpairedRunningPeerIsFencedBeforeAnythingIsPromoted(t *testing.T) {
	r := newRig(t, config.FailoverServer)
	ctx := context.Background()
	r.fake.UpdateInstance("i-n1", func(i *awsfake.Instance) { i.Impaired = true })
	r.mon.Tick(ctx)
	r.advance(2 * time.Minute)
	d := r.mon.Tick(ctx)
	if d.Action != "server" || d.Err != nil {
		t.Fatalf("decision: %+v", d)
	}
	order := r.fake.Order("ec2")
	stop, assoc := slices.Index(order, "ec2:StopInstances"), slices.Index(order, "ec2:AssociateAddress")
	if stop < 0 || assoc < stop {
		t.Fatalf("an impaired peer must be stopped, and before the address moves: %v", order)
	}
	if st := r.fake.InstanceState("i-n1"); st != "stopped" {
		t.Fatalf("n1 is %s", st)
	}
}
