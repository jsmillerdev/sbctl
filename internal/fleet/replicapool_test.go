package fleet

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

type recordingPeers struct {
	ids []string
	err error
}

func (r *recordingPeers) RefreshPeers(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return r.err
}

func replicaPoolerRig(t *testing.T) (ReplicaPooler, *replicaStub, *stubTenant, *recordingPeers, *[]string) {
	t.Helper()
	n := newTestNode(t)
	n.project(t, testRef)
	ctx := context.Background()
	n.cfg.Ports.ProjectBase, n.cfg.Ports.ReplicaBase = 30000, 12000
	if err := n.reg.CreateNode(ctx, &registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	if err := n.reg.CreateReplica(ctx, &registry.Replica{Identifier: testReplica, Ref: testRef, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	order := new([]string)
	sv := &replicaStub{stubTenant: stubTenant{name: config.SvcSupavisor, order: order}}
	other := &stubTenant{name: config.SvcRealtime, order: order}
	peers := &recordingPeers{}
	return ReplicaPooler{Deps: n.deps(), Fleet: Fleet{sv, other}, Peers: peers}, sv, other, peers, order
}

// A replica gets its Supavisor tenant: the project's own tenant first (it sets the pgbouncer
// role's password that the standby replicates), then the replica's, and then the other nodes are
// asked to drop what they cached. Realtime and Storage are not asked.
func TestReplicaPoolerEnsuresTheProjectsTenantThenTheReplicas(t *testing.T) {
	p, sv, _, peers, order := replicaPoolerRig(t)
	if err := p.EnsureReplicaTenant(context.Background(), testRef, testReplica); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*order, ",") != "ensure supavisor" {
		t.Fatalf("tenant calls: %v (only the Supavisor tenant of the project is ensured)", *order)
	}
	if len(sv.ensured) != 1 || sv.ensured[0] != testReplica {
		t.Fatalf("replica tenants ensured: %v", sv.ensured)
	}
	if len(peers.ids) != 1 || peers.ids[0] != testReplica {
		t.Fatalf("peers refreshed for %v", peers.ids)
	}
}

func TestReplicaPoolerRemoves(t *testing.T) {
	p, sv, _, peers, _ := replicaPoolerRig(t)
	if err := p.RemoveReplicaTenant(context.Background(), testReplica); err != nil {
		t.Fatal(err)
	}
	if len(sv.removed) != 1 || sv.removed[0] != testReplica || len(peers.ids) != 1 {
		t.Fatalf("removed %v, refreshed %v", sv.removed, peers.ids)
	}
	// A failure of the service is the caller's to retry, and the peers are not asked.
	sv.err = errors.New("supavisor is down")
	peers.ids = nil
	if err := p.RemoveReplicaTenant(context.Background(), testReplica); err == nil || len(peers.ids) != 0 {
		t.Fatalf("remove with Supavisor down = %v, refreshed %v", err, peers.ids)
	}
}

// A node that does not answer the refresh is a warning: the tenant is made, and the call would
// only be repeated for nothing.
func TestReplicaPoolerToleratesAPeerThatDoesNotAnswer(t *testing.T) {
	p, _, _, peers, _ := replicaPoolerRig(t)
	peers.err = errors.New("node n3: no session")
	var log strings.Builder
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	if err := p.EnsureReplicaTenant(context.Background(), testRef, testReplica); err != nil {
		t.Fatalf("EnsureReplicaTenant = %v", err)
	}
	if err := p.RemoveReplicaTenant(context.Background(), testReplica); err != nil {
		t.Fatalf("RemoveReplicaTenant = %v", err)
	}
	if !strings.Contains(log.String(), "no session") {
		t.Fatalf("the failure was not logged: %q", log.String())
	}
	p.Peers = nil // outside a cluster there is nobody to ask
	if err := p.EnsureReplicaTenant(context.Background(), testRef, testReplica); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaPoolerRefusesWhatIsNotTheProjectsReplica(t *testing.T) {
	p, sv, _, _, _ := replicaPoolerRig(t)
	ctx := context.Background()
	other := "zzzzzzzzzzzzzzzzzzzz"
	for name, tc := range map[string][2]string{
		"another project": {other, testReplica},
		"the ref itself":  {testRef, testRef},
		"nothing":         {testRef, ""},
		"unknown":         {testRef, testRef + "-rr-eu-west-1-zzzzzz"},
	} {
		if err := p.EnsureReplicaTenant(ctx, tc[0], tc[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := p.RemoveReplicaTenant(ctx, testRef); err == nil {
		t.Error("removing the project's ref as a replica was accepted")
	}
	if len(sv.ensured) != 0 || len(sv.removed) != 0 {
		t.Fatalf("calls for bad input: ensured %v removed %v", sv.ensured, sv.removed)
	}
	// A fleet with no pooler cannot make a pooler tenant, and says so.
	p.Fleet = Fleet{&stubTenant{name: config.SvcRealtime, order: new([]string)}}
	if err := p.EnsureReplicaTenant(ctx, testRef, testReplica); err == nil {
		t.Error("a fleet without Supavisor was accepted")
	}
}
