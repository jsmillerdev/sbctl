package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
)

// replicaTenant is a shared service that pools replicas too.
type replicaTenant struct {
	refreshTenant
	mu       sync.Mutex
	replicas []fleet.TenantSpec
}

func (r *replicaTenant) EnsureReplicaTenant(_ context.Context, spec fleet.TenantSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replicas = append(r.replicas, spec)
	return nil
}

func (r *replicaTenant) RemoveReplicaTenant(context.Context, string) error { return nil }

// peerRefresh records what the leader asked the other nodes to drop.
type peerRefresh struct {
	tenants []string
	err     error
}

func (p *peerRefresh) RefreshPeers(_ context.Context, tenant string) error {
	p.tenants = append(p.tenants, tenant)
	return p.err
}

// EnsureTenant is the call that follows a move of a project's home: the project's tenant, then its
// replicas' that are up, then the other nodes drop what they cached. The daemon's sweep at start sends
// no refresh to the other nodes.
func TestEnsureTenantRegistersTheProjectAndItsReplicasAndRefreshesThePeers(t *testing.T) {
	ctx := context.Background()
	h, p, _, ids := replicaHarness(t)
	rt := &replicaTenant{}
	peers := &peerRefresh{}
	h.e.opts.Fleet = fleet.Fleet{rt}
	h.e.SetPeerRefresher(peers)
	// The second replica is still being set up: the replica controller registers it when it is healthy.
	if err := h.reg.SetReplicaStatus(ctx, ids[1], registry.ReplicaInit, "3_initiated_read_replica_setup", ""); err != nil {
		t.Fatal(err)
	}

	if err := h.e.EnsureTenant(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if len(rt.ensured) != 1 || rt.ensured[0].Ref != p.Ref || rt.ensured[0].ReplicaID != "" {
		t.Fatalf("project tenants = %+v", rt.ensured)
	}
	if len(rt.replicas) != 1 || rt.replicas[0].ReplicaID != ids[0] || rt.replicas[0].Ref != p.Ref ||
		rt.replicas[0].DBPort != h.cfg.ReplicaPorts(p.Ref, p.Seq).Postgres || rt.replicas[0].JWTSecret != rt.ensured[0].JWTSecret {
		t.Fatalf("replica tenants = %+v", rt.replicas)
	}
	if got := strings.Join(peers.tenants, ","); got != p.Ref+","+ids[0] {
		t.Fatalf("peers asked to refresh %s", got)
	}

	// The sweep at start registers everything and asks no peer.
	peers.tenants = nil
	if errs := h.e.EnsureTenants(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(peers.tenants) != 0 {
		t.Fatalf("the sweep refreshed peers: %v", peers.tenants)
	}
	if len(rt.replicas) != 2 {
		t.Fatalf("the sweep did not register the replica: %+v", rt.replicas)
	}

	// A peer that cannot be reached is a warning, not a failed registration.
	peers.err = errors.New("n3 did not answer")
	if err := h.e.EnsureTenant(ctx, p.Ref); err != nil {
		t.Fatalf("EnsureTenant with an unreachable peer: %v", err)
	}

	// Nothing to register for the system project or without shared services.
	rt.ensured = nil
	if err := h.e.EnsureTenant(ctx, "system"); err != nil || len(rt.ensured) != 0 {
		t.Fatalf("system: %v %+v", err, rt.ensured)
	}
	h.e.opts.Fleet = nil
	if err := h.e.EnsureTenant(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
}

// The orchestrator holds Lock for the whole move and calls EnsureTenant inside it: the lock is not
// reentrant, so EnsureTenant must not take it. The daemon's sweep holds no lock and takes it.
func TestEnsureTenantRunsUnderTheLockTheOrchestratorHolds(t *testing.T) {
	ctx := context.Background()
	h, p, _, _ := replicaHarness(t)
	rt := &replicaTenant{}
	h.e.opts.Fleet = fleet.Fleet{rt}
	unlock, err := h.e.Lock(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()

	done := make(chan error, 1)
	go func() { done <- h.e.EnsureTenant(ctx, p.Ref) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureTenant waits for the lock its caller holds")
	}
	if len(rt.ensured) != 1 {
		t.Fatalf("project tenants = %+v", rt.ensured)
	}

	// The sweep does not run on a project whose lock is held.
	swept := make(chan struct{})
	go func() { h.e.EnsureTenants(ctx); close(swept) }()
	select {
	case <-swept:
		t.Fatal("the sweep ran on a project that a move holds")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not run after the lock was released")
	}
}

func TestQuiesceTenantAndLockServeTheFailoverOrchestrator(t *testing.T) {
	ctx := context.Background()
	h, _, _, rt := configHarness(t)
	p := h.create(t)

	rt.err = errors.New("realtime down")
	if err := h.e.QuiesceTenant(ctx, p.Ref); err == nil || !strings.Contains(err.Error(), "realtime down") {
		t.Fatalf("QuiesceTenant hides the failure: %v", err)
	}
	rt.err = nil
	if err := h.e.QuiesceTenant(ctx, p.Ref); err != nil || len(rt.quiesced) != 2 {
		t.Fatalf("QuiesceTenant = %v, quiesced %v", err, rt.quiesced)
	}

	// The lock is the one the project's own operations take.
	unlock, err := h.e.Lock(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Pause(ctx, p.Ref) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Pause ran while the move held the lock: %v", err)
	default:
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A password change and a restore refresh the pooler of this node and then of the others.
func TestAPasswordChangeRefreshesThePoolerOnThePeersToo(t *testing.T) {
	ctx := context.Background()
	h, _, _, rt := configHarness(t)
	p := h.create(t)
	peers := &peerRefresh{err: errors.New("n2 did not answer")}
	h.e.SetPeerRefresher(peers)
	if err := h.e.SetDatabasePassword(ctx, p.Ref, "a-new-Password-1"); err != nil {
		t.Fatalf("an unreachable peer failed the change: %v", err)
	}
	if len(rt.refreshed) != 1 || rt.refreshed[0] != p.Ref || len(peers.tenants) != 1 || peers.tenants[0] != p.Ref {
		t.Fatalf("local %v, peers %v", rt.refreshed, peers.tenants)
	}
}

// A project that is moving stays RESTARTING until the move ends, and the move registers it at its new home
// meanwhile (EnsureTenant is its call). The sweep at start leaves any project that is not active to its own
// operation, and so does a paused one when a move asks.
func TestEnsureTenantRegistersAProjectThatIsRestartingBecauseOfAMove(t *testing.T) {
	ctx := context.Background()
	h, p, _, _ := replicaHarness(t)
	rt := &replicaTenant{}
	h.e.opts.Fleet = fleet.Fleet{rt}
	if err := h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusRestarting); err != nil {
		t.Fatal(err)
	}
	if err := h.e.EnsureTenant(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if len(rt.ensured) != 1 || rt.ensured[0].Ref != p.Ref {
		t.Fatalf("a project that is restarting for a move was not registered: %+v", rt.ensured)
	}
	rt.ensured = nil
	if errs := h.e.EnsureTenants(ctx); len(errs) != 0 || len(rt.ensured) != 0 {
		t.Fatalf("the sweep registered a project that is restarting: %v %+v", errs, rt.ensured)
	}
	for _, s := range []registry.Status{registry.StatusInactive, registry.StatusUpgrading, registry.StatusRestoring} {
		if err := h.reg.SetProjectStatus(ctx, p.Ref, s); err != nil {
			t.Fatal(err)
		}
		if err := h.e.EnsureTenant(ctx, p.Ref); err != nil || len(rt.ensured) != 0 {
			t.Fatalf("a %s project was registered by a move: %v %+v", s, err, rt.ensured)
		}
	}
}
