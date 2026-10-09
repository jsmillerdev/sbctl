package health

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

func TestSystemStandbyIsTheMarkOfAFollower(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	if systemStandby(cfg) {
		t.Fatal("a node with no system cluster is not a follower")
	}
	data := cfg.Paths().PostgresData(config.SystemRef)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if systemStandby(cfg) {
		t.Fatal("a primary's data directory is not a standby's")
	}
	if err := os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !systemStandby(cfg) {
		t.Fatal("standby.signal marks a follower")
	}
}
