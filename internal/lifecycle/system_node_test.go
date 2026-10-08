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

// unitSup notes the unit each supervisor call names.
type unitSup struct {
	recSup
	named []string
}

func (u *unitSup) Start(_ context.Context, unit string) error {
	u.named = append(u.named, "start "+unit)
	return nil
}
func (u *unitSup) Stop(_ context.Context, unit string) error {
	u.named = append(u.named, "stop "+unit)
	return nil
}

// The leader takes the nightly backup of a project homed on a follower, so a follower never starts a
// timer: asked to, it stops the one an earlier run as the leader left.
func TestAFollowerStartsNoBackupTimer(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Supervisor = config.SupervisorSystemd
	const ref = "abcdefghijklmnopqrst"
	timer := "supavise-basebackup@" + ref + ".timer"

	sup := &unitSup{}
	own := (&OpenOptions{}).timers(cfg, sup)
	if err := own.StartTimer(ctx, ref); err != nil || strings.Join(sup.named, ",") != "start "+timer {
		t.Fatalf("a writable node: %v, %v", sup.named, err)
	}

	sup = &unitSup{}
	follower := (&OpenOptions{ReadOnly: true}).timers(cfg, sup)
	if err := follower.StartTimer(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := follower.StopTimer(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sup.named, ","); got != "stop "+timer+",stop "+timer {
		t.Fatalf("a follower: %s", got)
	}
}
