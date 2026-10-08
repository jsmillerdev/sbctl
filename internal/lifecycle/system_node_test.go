package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

func TestSelfNode(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	cfg := config.Default()
	cfg.Node.Name = "primary" // the founder's name until the daemon renames it
	if id, err := SelfNode(ctx, reg, cfg, false); err != nil || id != registry.FounderNodeID {
		t.Fatalf("founder: %q, %v", id, err)
	}
	// A server whose host name is not in the registry yet is the founder when it can write the registry.
	cfg.Node.Name = "ip-10-0-0-5"
	if id, err := SelfNode(ctx, reg, cfg, false); err != nil || id != registry.FounderNodeID {
		t.Fatalf("unnamed founder: %q, %v", id, err)
	}
	// A joined node is found by name.
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	cfg.Node.Name = "second"
	for _, ro := range []bool{false, true} {
		if id, err := SelfNode(ctx, reg, cfg, ro); err != nil || id != n2.ID {
			t.Fatalf("joined node (read-only %v): %q, %v", ro, id, err)
		}
	}
	// A follower that is not in the registry does not guess.
	cfg.Node.Name = "stranger"
	if id, err := SelfNode(ctx, reg, cfg, true); err == nil || id != "" || !strings.Contains(err.Error(), `"stranger"`) {
		t.Fatalf("follower missing from the registry: %q, %v", id, err)
	}
}

func TestFollowerRegistryDSNUsesTheStandbysPort(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = "/var/lib/supavise"
	leader, follower := RegistryDSN(cfg), FollowerRegistryDSN(cfg)
	if !strings.Contains(leader, "port=5433") || !strings.Contains(follower, "port=10000") {
		t.Fatalf("leader %s\nfollower %s", leader, follower)
	}
	if strings.ReplaceAll(follower, "port=10000", "port=5433") != leader {
		t.Fatalf("the two differ by more than the port:\n%s\n%s", leader, follower)
	}
}
