package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFunctionsDefaultsAndRoot(t *testing.T) {
	c := Default()
	f := c.Functions
	if f.Enabled {
		t.Fatal("Edge Functions are off by default")
	}
	if f.Memory() != 256 || f.WallClock() != 400 || f.IdleTimeout() != 150 || f.CPUSoft() != 1000 || f.CPUHard() != 2000 || f.Parallelism() != 16 || f.Reconcile() != 30 {
		t.Fatalf("defaults: %d %d %d %d %d %d %d", f.Memory(), f.WallClock(), f.IdleTimeout(), f.CPUSoft(), f.CPUHard(), f.Parallelism(), f.Reconcile())
	}
	c.StateDir = "/var/lib/sbctl"
	if got := c.Paths().FunctionsRoot(); got != "/var/lib/sbctl/system/edge-runtime/tenants" {
		t.Fatalf("root %q", got)
	}
}

func TestFunctionsLimitsCanBeSwitchedOffWithANegativeValue(t *testing.T) {
	f := Functions{CPUSoftMs: -1, CPUHardMs: -5, MaxParallelism: -1, MemoryMB: 64}
	if f.CPUSoft() != 0 || f.CPUHard() != 0 || f.Parallelism() != 0 || f.Memory() != 64 {
		t.Fatalf("%d %d %d %d", f.CPUSoft(), f.CPUHard(), f.Parallelism(), f.Memory())
	}
}

func TestFunctionsSectionLoadsFromTOMLAndEnvironment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[functions]\nenabled = true\nmemory_mb = 128\nwall_clock_seconds = 30\nproject_url_template = \"http://{ref}.x.test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBCTL_FUNCTIONS_IDLE_TIMEOUT_SECONDS", "9")
	t.Setenv("SBCTL_FUNCTIONS_MEMORY_MAX", "2G")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Functions
	if !f.Enabled || f.Memory() != 128 || f.WallClock() != 30 || f.IdleTimeout() != 9 || f.MemoryMax != "2G" || f.ProjectURLTemplate != "http://{ref}.x.test" {
		t.Fatalf("%+v", f)
	}
}
