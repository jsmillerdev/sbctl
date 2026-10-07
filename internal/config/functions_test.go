package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFunctionsDefaultsAndRoot(t *testing.T) {
	c := Default()
	f := c.Functions
	if f.Enabled {
		t.Fatal("Edge Functions are off by default")
	}
	if f.Memory() != 256 || f.WallClock() != 400 || f.IdleTimeout() != 150 || f.CPUSoft() != 1000 || f.CPUHard() != 2000 || f.Parallelism() != 16 || f.PerProject() != 8 || f.WorkersPerProject() != 15 || f.RuntimeMemoryMax() != "4352M" || f.Reconcile() != 30 {
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
	t.Setenv("SBCTL_FUNCTIONS_MEMORY_MAX", "3G")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Functions
	if !f.Enabled || f.Memory() != 128 || f.WallClock() != 30 || f.IdleTimeout() != 9 || f.MemoryMax != "3G" || f.ProjectURLTemplate != "http://{ref}.x.test" {
		t.Fatalf("%+v", f)
	}
}

func TestFunctionsPerProjectCapLeavesRoomForOthers(t *testing.T) {
	for _, c := range []struct {
		f    Functions
		want int
	}{
		{Functions{}, 8},
		{Functions{MaxParallelism: 8}, 4},
		{Functions{MaxParallelism: 1}, 1},
		{Functions{MaxParallelism: 3}, 1},
		{Functions{MaxParallelism: -1}, DefaultFunctionsMaxPerProject},
		{Functions{MaxPerProject: 2}, 2},
		{Functions{MaxPerProject: -1}, 0},
	} {
		if got := c.f.PerProject(); got != c.want {
			t.Errorf("%+v: PerProject %d, want %d", c.f, got, c.want)
		}
	}
}

func TestFunctionsWorkersPerProjectNeverTakeTheWholePool(t *testing.T) {
	for par, want := range map[int]int{0: 15, 16: 15, 2: 1, 1: 1, -1: 0} {
		if got := (Functions{MaxParallelism: par}).WorkersPerProject(); got != want {
			t.Errorf("max_parallelism %d: %d workers per project, want %d", par, got, want)
		}
	}
}

func TestFunctionsMemoryMaxFollowsTheWorkersItMustHold(t *testing.T) {
	f := Functions{MaxParallelism: 4, MemoryMB: 100}
	if got := f.RuntimeMemoryMax(); got != "656M" {
		t.Fatalf("derived memory max %q, want 656M (4 x 100 + 256)", got)
	}
	if got := (Functions{MaxParallelism: -1}).RuntimeMemoryMax(); got != "" {
		t.Fatalf("uncapped workers and no memory_max: %q, want the [defaults] limit (empty)", got)
	}
	if got := (Functions{MemoryMax: "5G"}).RuntimeMemoryMax(); got != "5G" {
		t.Fatalf("explicit value: %q", got)
	}
	ok := func(f Functions) error { f.Enabled = true; return f.Validate() }
	for name, f := range map[string]Functions{
		"enough":      {MemoryMax: "4352M"},
		"more":        {MemoryMax: "5G"},
		"infinity":    {MemoryMax: "infinity"},
		"uncapped":    {MemoryMax: "1G", MaxParallelism: -1},
		"fewer":       {MemoryMax: "1G", MaxParallelism: 2, MemoryMB: 256},
		"unset":       {},
		"lowercase k": {MemoryMax: "5000000k"},
	} {
		if err := ok(f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, f := range map[string]Functions{
		"1G for the defaults": {MemoryMax: "1G"},
		"one byte short":      {MemoryMax: "4563402751"},
		"nonsense":            {MemoryMax: "lots"},
	} {
		if err := ok(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Disabled functions are not validated: the section may be half edited.
	if err := (Functions{MemoryMax: "1G"}).Validate(); err != nil {
		t.Errorf("disabled: %v", err)
	}
	c := Default()
	c.Functions = Functions{Enabled: true, MemoryMax: "512M"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "memory_max") {
		t.Errorf("Config.Validate: %v", err)
	}
}
