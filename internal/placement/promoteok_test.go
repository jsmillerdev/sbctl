package placement

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

type testArts struct{}

func (testArts) Dir(svc string) (string, error) { return "/art/" + svc, nil }
func (testArts) Tag(svc string) (string, error) { return svc + "-tag", nil }

// upSup reports every unit as running.
type upSup struct{ mu sync.Mutex }

func (*upSup) Render(context.Context, units.Spec) error { return nil }
func (*upSup) Start(context.Context, string) error      { return nil }
func (*upSup) Stop(context.Context, string) error       { return nil }
func (*upSup) Remove(context.Context, string) error     { return nil }
func (*upSup) Status(_ context.Context, u string) (units.Status, error) {
	return units.Status{Unit: u, State: units.StateActive, Since: time.Now().Add(-time.Hour)}, nil
}

// standbySQL answers as a standby that promotes.
type standbySQL struct {
	mu       sync.Mutex
	promoted bool
}

func (s *standbySQL) Ping(context.Context, lifecycle.ClusterAddr) error { return nil }
func (s *standbySQL) Status(_ context.Context, a lifecycle.ClusterAddr) (lifecycle.ClusterStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lifecycle.ClusterStatus{InRecovery: !s.promoted}, nil
}
func (s *standbySQL) ReplayedTo(context.Context, lifecycle.ClusterAddr, string) (bool, error) {
	return true, nil
}
func (s *standbySQL) ReplayLSN(context.Context, lifecycle.ClusterAddr) (string, error) {
	return "0/1", nil
}
func (s *standbySQL) RetryInterval(context.Context, lifecycle.ClusterAddr) (time.Duration, error) {
	return time.Millisecond, nil
}
func (s *standbySQL) Promote(context.Context, lifecycle.ClusterAddr, time.Duration) error {
	s.mu.Lock()
	s.promoted = true
	s.mu.Unlock()
	return nil
}
func (s *standbySQL) Checkpoint(context.Context, lifecycle.ClusterAddr) error { return nil }

// What PromoteReplica writes into promote.ok is what the WAL relay of the backup package reads.
func TestPromoteOKIsWhatTheBackupPackageParses(t *testing.T) {
	cfg := config.Default()
	// A short path: the plane refuses a unix socket path the OS would not accept (macOS allows 103 bytes).
	dir, err := os.MkdirTemp("/tmp", "sbp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg.StateDir = dir
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	pl := lifecycle.NewPostgresPlane(cfg, &upSup{}, testArts{}, registry.NewMemory(), lifecycle.PlaneOptions{ClusterSQL: &standbySQL{}})
	p := &registry.Project{Ref: testRef, Seq: 3, Class: "micro", Engine: registry.EnginePostgres, Limits: cfg.Defaults}
	tgt := lifecycle.ReplicaTarget{Identifier: testReplicaID(), Project: p, Keys: testKeys()}
	data := cfg.Paths().PostgresData(testRef)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"PG_VERSION", "standby.signal"} {
		if err := os.WriteFile(filepath.Join(data, f), []byte("17\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pl.PromoteReplica(context.Background(), tgt, lifecycle.PromoteOptions{Epoch: 42}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.Paths().PromoteOK(testRef))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, backup.FormatPromoteOK(42)) {
		t.Fatalf("promote.ok = %q, backup.FormatPromoteOK writes %q", b, backup.FormatPromoteOK(42))
	}
	if n, err := backup.ParsePromoteOK(b); err != nil || n != 42 {
		t.Fatalf("ParsePromoteOK = %d, %v", n, err)
	}
}
