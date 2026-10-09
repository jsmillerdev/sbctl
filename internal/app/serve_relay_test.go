package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// The WAL relay starts before the boot decision only on a node whose system cluster is a standby: its
// start asks the relay for history files, and the decision waits for it.
func TestSystemIsStandby(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	if systemIsStandby(cfg) {
		t.Fatal("a node with no system cluster is not a standby")
	}
	data := cfg.Paths().PostgresData(config.SystemRef)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if systemIsStandby(cfg) {
		t.Fatal("a primary's data directory is not a standby's")
	}
	if err := os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !systemIsStandby(cfg) {
		t.Fatal("standby.signal makes the system cluster a standby's")
	}
}
