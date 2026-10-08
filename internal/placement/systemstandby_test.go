package placement

import (
	"context"
	"errors"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

type recStandbyPlane struct {
	plan   lifecycle.SystemStandbyPlan
	seeder lifecycle.ReplicaSeeder
	calls  []string
	err    error
}

func (p *recStandbyPlane) SeedSystemStandby(_ context.Context, plan lifecycle.SystemStandbyPlan, seeder lifecycle.ReplicaSeeder) error {
	p.calls = append(p.calls, "seed")
	p.plan, p.seeder = plan, seeder
	return p.err
}

func (p *recStandbyPlane) SystemStandbyPreflight(context.Context) error {
	p.calls = append(p.calls, "preflight")
	return p.err
}

func (p *recStandbyPlane) SystemStandbyJoinPreflight(context.Context) error {
	p.calls = append(p.calls, "join-preflight")
	return p.err
}

func TestSystemStandbyHandsTheBootstrapToThePlane(t *testing.T) {
	p := &recStandbyPlane{}
	called := false
	s := SystemStandby{Plane: p, Seeder: func(context.Context, lifecycle.ReplicaSeedPlan) error { called = true; return nil }}
	b := peerapi.SystemBootstrap{Identifier: "system-rr-us-east-1-abc123", BackupID: "20261008T120000Z-abcdef", Leader: "n1", Epoch: 4, ReplicationPassword: "pw"}
	if err := s.Seed(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if want := (lifecycle.SystemStandbyPlan{Identifier: b.Identifier, BackupID: b.BackupID, ReplicationPassword: "pw"}); p.plan != want {
		t.Fatalf("plan = %+v, want %+v", p.plan, want)
	}
	if err := p.seeder(context.Background(), lifecycle.ReplicaSeedPlan{}); err != nil || !called {
		t.Fatalf("the plane was not given the seeder: %v", err)
	}
	boom := errors.New("no base backup")
	p.err = boom
	if err := s.Seed(context.Background(), b); !errors.Is(err, boom) {
		t.Fatalf("seed error = %v", err)
	}
}

// A server that has not joined takes its backend from the leader, so its preflight does not look at
// the one it holds; a rejoin does.
func TestSystemStandbyPreflightDependsOnWhetherTheServerJoined(t *testing.T) {
	for joining, want := range map[bool]string{true: "join-preflight", false: "preflight"} {
		p := &recStandbyPlane{}
		if err := (SystemStandby{Plane: p, Joining: joining}).Preflight(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(p.calls) != 1 || p.calls[0] != want {
			t.Errorf("joining=%v: calls = %v, want %s", joining, p.calls, want)
		}
	}
}
