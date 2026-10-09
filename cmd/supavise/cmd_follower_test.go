package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// answers makes inRecovery answer for the sockets in the map (true: a standby) and fail for the rest.
func answers(t *testing.T, m map[string]bool) {
	t.Helper()
	old := inRecovery
	t.Cleanup(func() { inRecovery = old })
	inRecovery = func(_ context.Context, dsn string) (bool, error) {
		if rec, ok := m[dsn]; ok {
			return rec, nil
		}
		return false, errors.New("no such socket")
	}
}

// The socket that answers decides: a primary on the system port is a leader or a single server, a
// standby on the replica port is a follower, and a system cluster that is down decides nothing.
func TestLocalRegistryFollowsTheSocketThatAnswers(t *testing.T) {
	cfg := config.Default()
	primary, standby := app.RegistryDSNs(cfg)[0], app.RegistryDSNs(cfg)[1]
	ctx := context.Background()

	answers(t, map[string]bool{primary: false})
	if dsn, sb := localRegistry(ctx, cfg); dsn != primary || sb {
		t.Errorf("a primary: %q standby=%v", dsn, sb)
	}
	answers(t, map[string]bool{standby: true})
	if dsn, sb := localRegistry(ctx, cfg); dsn != standby || !sb {
		t.Errorf("a standby: %q standby=%v", dsn, sb)
	}
	answers(t, nil)
	if dsn, sb := localRegistry(ctx, cfg); dsn != "" || sb {
		t.Errorf("nothing answers: %q standby=%v", dsn, sb)
	}
}

// A command that opens the node opens it read-only on a follower, on the standby's socket; a registry
// the operator names is theirs; and a server whose system cluster is down keeps the defaults, whose
// error says what to start.
func TestNodeOptionsOpenAFollowerReadOnly(t *testing.T) {
	cfg := config.Default()
	primary, standby := app.RegistryDSNs(cfg)[0], app.RegistryDSNs(cfg)[1]
	ctx := context.Background()
	t.Setenv(envRegistryDSN, "")

	answers(t, map[string]bool{standby: true})
	if lo := nodeOptions(ctx, cfg, lifecycle.OpenOptions{}); !lo.ReadOnly || lo.RegistryDSN != standby {
		t.Errorf("a follower: read-only=%v dsn=%q", lo.ReadOnly, lo.RegistryDSN)
	}
	answers(t, map[string]bool{primary: false})
	if lo := nodeOptions(ctx, cfg, lifecycle.OpenOptions{}); lo.ReadOnly || lo.RegistryDSN != primary {
		t.Errorf("a leader: read-only=%v dsn=%q", lo.ReadOnly, lo.RegistryDSN)
	}
	answers(t, nil)
	if lo := nodeOptions(ctx, cfg, lifecycle.OpenOptions{}); lo.ReadOnly || lo.RegistryDSN != "" {
		t.Errorf("nothing answers: read-only=%v dsn=%q", lo.ReadOnly, lo.RegistryDSN)
	}

	// SUPAVISE_REGISTRY_DSN names the registry: nothing is asked of the sockets.
	answers(t, map[string]bool{standby: true})
	t.Setenv(envRegistryDSN, "host=elsewhere dbname=supavise")
	if lo := nodeOptions(ctx, cfg, lifecycle.OpenOptions{}); lo.ReadOnly || lo.RegistryDSN != "host=elsewhere dbname=supavise" {
		t.Errorf("an override: read-only=%v dsn=%q", lo.ReadOnly, lo.RegistryDSN)
	}
	t.Setenv(envRegistryDSN, "")
	if lo := nodeOptions(ctx, cfg, lifecycle.OpenOptions{RegistryDSN: "host=given"}); lo.ReadOnly || lo.RegistryDSN != "host=given" {
		t.Errorf("a given DSN: read-only=%v dsn=%q", lo.ReadOnly, lo.RegistryDSN)
	}
}

// A write that a follower's registry refuses says that the command belongs on the leader; any other
// error is left as it is.
func TestFollowerHintExplainsARefusedWrite(t *testing.T) {
	refused := fmt.Errorf("lifecycle: create project: %w: cannot execute INSERT in a read-only transaction", registry.ErrReadOnly)
	err := followerHint(refused)
	if !errors.Is(err, registry.ErrReadOnly) {
		t.Fatalf("the cause is lost: %v", err)
	}
	for _, want := range []string{"cannot execute INSERT", "follows another one", "run it on the leader", "supavise node ls"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	other := errors.New("connection refused")
	if followerHint(other) != other || followerHint(nil) != nil {
		t.Error("an error that is not a refused write was changed")
	}
}
