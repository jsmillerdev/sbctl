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
	if f.Memory() != 256 || f.WallClock() != 400 || f.IdleTimeout() != 150 || f.CPUSoft() != 1000 || f.CPUHard() != 2000 || f.Reconcile() != 30 {
		t.Fatalf("defaults: %d %d %d %d %d %d", f.Memory(), f.WallClock(), f.IdleTimeout(), f.CPUSoft(), f.CPUHard(), f.Reconcile())
	}
	// 16 workers in all, 8 per project, one worker per function, 128 requests per project;
	// the unit holds the workers: 16 x (256 + 32) + 256.
	if f.Parallelism() != 1 || f.Workers() != 16 || f.WorkersPerProject() != 8 || f.PerProject() != 128 || f.RuntimeMemoryMax() != "4864M" {
		t.Fatalf("budget: %d %d %d %d %s", f.Parallelism(), f.Workers(), f.WorkersPerProject(), f.PerProject(), f.RuntimeMemoryMax())
	}
	c.StateDir = "/var/lib/sbctl"
	if got := c.Paths().FunctionsRoot(); got != "/var/lib/sbctl/system/edge-runtime/tenants" {
		t.Fatalf("root %q", got)
	}
}

func TestFunctionsLimitsCanBeSwitchedOffWithANegativeValue(t *testing.T) {
	f := Functions{CPUSoftMs: -1, CPUHardMs: -5, MaxWorkers: -1, MaxPerProject: -1, MemoryMB: 64}
	if f.CPUSoft() != 0 || f.CPUHard() != 0 || f.Workers() != 0 || f.WorkersPerProject() != 0 || f.PerProject() != 0 || f.Memory() != 64 || f.RuntimeMemoryMax() != "" {
		t.Fatalf("%d %d %d %d %d %d %q", f.CPUSoft(), f.CPUHard(), f.Workers(), f.WorkersPerProject(), f.PerProject(), f.Memory(), f.RuntimeMemoryMax())
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

func TestFunctionsWorkerBudgetIsRuntimeWideAndSharedByProjects(t *testing.T) {
	for _, c := range []struct {
		name                  string
		f                     Functions
		workers, perProject   int
		memoryMax             string
		maxParallelismPerFunc int
	}{
		{"defaults", Functions{}, 16, 8, "4864M", 1},
		{"explicit workers", Functions{MaxWorkers: 10}, 10, 5, "3136M", 1},
		{"one worker", Functions{MaxWorkers: 1}, 1, 1, "544M", 1},
		{"share larger than the budget", Functions{MaxWorkers: 4, MaxWorkersPerProject: 9}, 4, 4, "1408M", 1},
		{"share", Functions{MaxWorkers: 12, MaxWorkersPerProject: 3}, 12, 3, "3712M", 1},
		{"no share", Functions{MaxWorkers: 12, MaxWorkersPerProject: -1}, 12, 12, "3712M", 1},
		// memory_max decides the budget when max_workers is not set: (2G - 256M) / (256 + 32).
		{"derived from memory_max", Functions{MemoryMax: "2G"}, 6, 3, "2G", 1},
		{"several workers per function cost more", Functions{MaxParallelism: 2, MaxWorkers: 4}, 4, 2, "2560M", 2},
		{"smaller workers", Functions{MemoryMB: 96, MaxWorkers: 20}, 20, 10, "2816M", 1},
		{"memory_max infinity", Functions{MemoryMax: "infinity"}, 16, 8, "infinity", 1},
	} {
		if got := c.f.Workers(); got != c.workers {
			t.Errorf("%s: Workers %d, want %d", c.name, got, c.workers)
		}
		if got := c.f.WorkersPerProject(); got != c.perProject {
			t.Errorf("%s: WorkersPerProject %d, want %d", c.name, got, c.perProject)
		}
		if got := c.f.RuntimeMemoryMax(); got != c.memoryMax {
			t.Errorf("%s: RuntimeMemoryMax %q, want %q", c.name, got, c.memoryMax)
		}
		if got := c.f.Parallelism(); got != c.maxParallelismPerFunc {
			t.Errorf("%s: Parallelism %d, want %d", c.name, got, c.maxParallelismPerFunc)
		}
	}
}

// The budget is what protects the unit: whatever the settings, the workers the main
// service lets live at once must fit into the memory limit the unit gets.
func TestFunctionsWorkersAlwaysFitTheUnitsMemoryLimit(t *testing.T) {
	for _, f := range []Functions{
		{}, {MaxWorkers: 3}, {MaxWorkers: 40}, {MemoryMB: 512}, {MemoryMB: 64, MaxWorkers: 100},
		{MaxParallelism: 3, MaxWorkers: 7}, {MemoryMax: "3G"}, {MemoryMax: "700M"}, {MemoryMax: "3G", MaxWorkers: 5},
	} {
		f.Enabled = true
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
			continue
		}
		limit, finite, err := parseSystemdSize(f.RuntimeMemoryMax())
		if err != nil || !finite {
			t.Errorf("%+v: memory max %q", f, f.RuntimeMemoryMax())
			continue
		}
		used := uint64(f.Workers()*f.WorkerCostMB()+FunctionsRuntimeOverheadMB) << 20
		if used > limit {
			t.Errorf("%+v: %d workers can use %d MB, over the unit's %d MB", f, f.Workers(), used>>20, limit>>20)
		}
	}
}

func TestFunctionsMemoryMaxFollowsTheWorkersItMustHold(t *testing.T) {
	ok := func(f Functions) error { f.Enabled = true; return f.Validate() }
	for name, f := range map[string]Functions{
		"enough":      {MemoryMax: "4864M"},
		"more":        {MemoryMax: "6G"},
		"infinity":    {MemoryMax: "infinity"},
		"uncapped":    {MemoryMax: "1G", MaxWorkers: -1},
		"fewer":       {MemoryMax: "1G", MaxWorkers: 2, MemoryMB: 256},
		"derived":     {MemoryMax: "1G"},
		"unset":       {},
		"lowercase k": {MemoryMax: "6000000k"},
	} {
		if err := ok(f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, f := range map[string]Functions{
		"1G for the defaults": {MemoryMax: "1G", MaxWorkers: 16},
		"one byte short":      {MemoryMax: "4963401727", MaxWorkers: 16},
		"not even one worker": {MemoryMax: "400M"},
		"nonsense":            {MemoryMax: "lots"},
		"negative parallel":   {MaxParallelism: -1},
	} {
		if err := ok(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Disabled functions are not validated: the section may be half edited.
	if err := (Functions{MemoryMax: "1G", MaxWorkers: 16}).Validate(); err != nil {
		t.Errorf("disabled: %v", err)
	}
	c := Default()
	c.Functions = Functions{Enabled: true, MemoryMax: "512M", MaxWorkers: 4}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "memory_max") {
		t.Errorf("Config.Validate: %v", err)
	}
}

func TestFunctionsProxyTokenIsCreatedOnceAndShared(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	got := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			tok, err := LoadFunctionsProxyToken(p)
			if err != nil {
				t.Error(err)
			}
			got <- tok
		}()
	}
	first := <-got
	for i := 1; i < 8; i++ {
		if tok := <-got; tok != first {
			t.Fatalf("two callers got different secrets: %q and %q", first, tok)
		}
	}
	if len(first) != 64 {
		t.Fatalf("secret %q", first)
	}
	fi, err := os.Stat(p.FunctionsProxyTokenFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if again, _ := LoadFunctionsProxyToken(p); again != first {
		t.Fatal("the secret changed on the second load")
	}
	if rel, _ := filepath.Rel(p.System(SvcEdgeRuntime), p.FunctionsProxyTokenFile()); !strings.HasPrefix(rel, "..") {
		t.Fatalf("the token file is inside the unit's state directory: %s", rel)
	}
	if err := os.WriteFile(p.FunctionsProxyTokenFile(), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFunctionsProxyToken(p); err == nil {
		t.Fatal("a damaged token file was accepted")
	}
}
