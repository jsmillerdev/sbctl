package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

func TestServicesForPutsTheEdgeRuntimeBeforeStudioWhenEnabled(t *testing.T) {
	cfg := config.Default()
	if got := ServicesFor(cfg); strings.Join(got, ",") != strings.Join(Services, ",") {
		t.Fatalf("disabled: %v", got)
	}
	cfg.Functions.Enabled = true
	got := ServicesFor(cfg)
	want := []string{config.SvcPGMeta, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcEdgeRuntime, config.SvcStudio}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("enabled: %v", got)
	}
	if len(Services) != 5 {
		t.Fatal("ServicesFor must not change Services")
	}
}

func edgeRig(t *testing.T, enabled bool) *managerRig {
	t.Helper()
	r := newManagerRig(t, func(d *Deps) {
		d.Cfg.Functions.Enabled = enabled
		d.Cfg.Ports.EdgeRuntime = freePorts(t, 1)[0]
		a := allArtifacts()
		a[config.SvcEdgeRuntime] = "/art/edge-runtime"
		d.Artifacts = a
	})
	h := serveHealth(t, r.n.cfg.Ports.EdgeRuntime, edgeRuntimeHealthPath)
	r.health[config.SvcEdgeRuntime] = h
	r.sup.hooks[unitOf(config.SvcEdgeRuntime)] = func() { h.setUp(true) }
	return r
}

func TestEdgeRuntimeSpec(t *testing.T) {
	r := edgeRig(t, true)
	cfg := r.n.cfg
	cfg.Functions.MemoryMB, cfg.Functions.WallClockSeconds, cfg.Functions.IdleTimeoutSeconds = 128, 20, 9
	cfg.Functions.CPUSoftMs, cfg.Functions.CPUHardMs, cfg.Functions.MaxParallelism = -1, 3000, 2
	cfg.Functions.MaxPerProject, cfg.Functions.MaxWorkers, cfg.Functions.MaxWorkersPerProject = 200, 8, 3
	cfg.Functions.MemoryMax = "3G"
	specs, err := r.m.Specs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range specs {
		if s.Service != config.SvcEdgeRuntime {
			continue
		}
		found = true
		port := strconv.Itoa(cfg.Ports.EdgeRuntime)
		args := strings.Join(s.Exec, " ")
		for _, want := range []string{
			"bin/edge-runtime start", "--ip 127.0.0.1", "--port " + port, "--main-service " + realPath(MainServiceDir(cfg)),
			"--policy per_worker", "--user-worker-request-idle-timeout 9000", "--max-parallelism 2",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("args %q lack %q", args, want)
			}
		}
		if s.ArtifactDir != "/art/edge-runtime" || s.WorkDir != cfg.Paths().System(config.SvcEdgeRuntime) {
			t.Errorf("dirs %q %q", s.ArtifactDir, s.WorkDir)
		}
		env := s.Env
		for k, v := range map[string]string{
			"EDGE_RUNTIME_PORT": port, "SBCTL_FUNCTIONS_ROOT": realPath(cfg.Paths().FunctionsRoot()),
			"SBCTL_FUNCTIONS_MEMORY_MB": "128", "SBCTL_FUNCTIONS_WALL_CLOCK_SEC": "20", "SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC": "9",
			"SBCTL_FUNCTIONS_CPU_SOFT_MS": "0", "SBCTL_FUNCTIONS_CPU_HARD_MS": "3000", "SBCTL_FUNCTIONS_MAX_PER_PROJECT": "200", "SBCTL_FUNCTIONS_MAX_WORKERS": "8",
			"SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT": "3", "SBCTL_FUNCTIONS_WORKER_COST_MB": "320", // 2 per function x (128 + 32)
			"DENO_DIR": filepath.Join(cfg.Paths().System(config.SvcEdgeRuntime), "deno"),
			// 8 workers x 2 per function: one thread for each isolate the budget allows.
			"EDGE_RUNTIME_WORKER_POOL_SIZE": "16",
		} {
			if env[k] != v {
				t.Errorf("%s = %q, want %q", k, env[k], v)
			}
		}
		// The proxy's secret reaches the runtime through the environment, and is the node's.
		if want, err := config.LoadFunctionsProxyToken(cfg.Paths()); err != nil || len(want) != 64 || env["SBCTL_FUNCTIONS_PROXY_TOKEN"] != want {
			t.Errorf("proxy token %q (%v)", env["SBCTL_FUNCTIONS_PROXY_TOKEN"], err)
		}
		if sum, _ := MainServiceHash(); env[mainServiceMarkerEnv] != sum || sum == "" {
			t.Errorf("main service marker %q", env[mainServiceMarkerEnv])
		}
		if s.Limits.MemoryMax != "3G" || s.Limits.CPUQuota != cfg.Defaults.CPUQuota {
			t.Errorf("limits %+v", s.Limits)
		}
		// No secret of any project or of the fleet reaches the runtime's environment.
		for k := range env {
			if strings.Contains(k, "KEY") || strings.Contains(k, "SECRET") || strings.Contains(k, "PASSWORD") {
				t.Errorf("unexpected variable %s", k)
			}
		}
	}
	if !found {
		t.Fatal("no edge-runtime spec")
	}
}

func TestEdgeRuntimeSpecDefaults(t *testing.T) {
	r := edgeRig(t, true)
	specs, err := r.m.Specs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Service == config.SvcEdgeRuntime {
			if env := s.Env; env["SBCTL_FUNCTIONS_MEMORY_MB"] != "256" || env["SBCTL_FUNCTIONS_WALL_CLOCK_SEC"] != "400" ||
				env["SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC"] != "150" || env["SBCTL_FUNCTIONS_CPU_SOFT_MS"] != "1000" || env["SBCTL_FUNCTIONS_CPU_HARD_MS"] != "2000" || env["SBCTL_FUNCTIONS_MAX_PER_PROJECT"] != "128" || env["SBCTL_FUNCTIONS_MAX_WORKERS"] != "16" || env["SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT"] != "8" || env["EDGE_RUNTIME_WORKER_POOL_SIZE"] != "16" {
				t.Errorf("env %v", env)
			}
			if !strings.Contains(strings.Join(s.Exec, " "), "--max-parallelism 1") {
				t.Errorf("args %v", s.Exec)
			}
			// The unit's memory limit is what the 16 workers the main service allows can use
			// (256 MB heap + 32 MB overhead each), plus the runtime.
			want := r.n.cfg.Defaults
			want.MemoryMax = "4864M"
			if s.Limits != want {
				t.Errorf("limits %+v, want %+v", s.Limits, want)
			}
		}
	}
}

func TestManagerStartsTheEdgeRuntimeOnlyWhenEnabled(t *testing.T) {
	on := edgeRig(t, true)
	if err := on.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := on.sup.log()
	if !strings.Contains(log, "start sb-storage.service\nrender sb-edge-runtime.service\nstart sb-edge-runtime.service\nrender sb-studio.service") {
		t.Fatalf("start order:\n%s", log)
	}
	if !dirExists(on.n.cfg.Paths().System(config.SvcEdgeRuntime)) {
		t.Error("no work directory for the edge runtime")
	}
	hs := on.m.Status(context.Background())
	if len(hs) != 6 {
		t.Fatalf("status rows: %d", len(hs))
	}
	for _, h := range hs {
		if !h.Healthy {
			t.Errorf("%+v", h)
		}
	}

	off := edgeRig(t, false)
	if err := off.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off.sup.log(), "edge-runtime") {
		t.Fatalf("a node without [functions] enabled started the runtime:\n%s", off.sup.log())
	}
	if len(off.m.Status(context.Background())) != 5 {
		t.Fatal("status lists the runtime although it is off")
	}
	if err := on.m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on.sup.log(), "stop sb-studio.service\nstop sb-edge-runtime.service\nstop sb-storage.service") {
		t.Fatalf("stop order:\n%s", on.sup.log())
	}
}

func TestManagerReportsAnEdgeRuntimeThatDoesNotStart(t *testing.T) {
	r := edgeRig(t, true)
	r.sup.failOn["start sb-edge-runtime.service"] = os.ErrPermission
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "edge-runtime") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.sup.log(), "start sb-studio.service") {
		t.Fatal("a failing runtime stopped Studio from starting")
	}
}

func TestEnsureMainService(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	sum, err := EnsureMainService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := MainServiceHash(); sum != want {
		t.Fatalf("hash %s != %s", sum, want)
	}
	dir := MainServiceDir(cfg)
	for _, f := range []string{"index.ts", "src/handler.ts", "src/auth.ts", "src/projects.ts", "src/runtime.ts", "src/config.ts", "src/types.ts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	for _, f := range []string{"src/handler_test.ts", "src/testutil.ts", "deno.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s must not be part of the deployed service", f)
		}
	}
	// Idempotent: nothing is rewritten.
	idx := filepath.Join(dir, "index.ts")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(idx, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMainService(cfg); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(idx); !fi.ModTime().Equal(old.Truncate(time.Second)) && fi.ModTime().After(old.Add(time.Minute)) {
		t.Error("an unchanged file was rewritten")
	}
	// A tampered or stale file is repaired; a leftover of an older version is removed.
	if err := os.WriteFile(idx, []byte("tampered"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "old-module.ts"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMainService(cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(idx); string(b) == "tampered" {
		t.Error("tampered file kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "old-module.ts")); err == nil {
		t.Error("stale module kept")
	}
}

// [functions] enabled turned off after the runtime ran: fleet stop and status still see its
// unit, and fleet start removes it together with the tenants' secrets.
func TestTurningFunctionsOffRetiresTheRuntimeAndItsSecrets(t *testing.T) {
	r := edgeRig(t, true)
	cfg := r.n.cfg
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	run := units.FilesFor(cfg, units.Spec{Service: config.SvcEdgeRuntime}).Run
	if err := os.MkdirAll(filepath.Dir(run), 0o750); err != nil || os.WriteFile(run, []byte("#!/bin/sh\n"), 0o750) != nil {
		t.Fatal("could not stub the rendered launcher", err)
	}
	secret := filepath.Join(cfg.Paths().FunctionsRoot(), "abcdefghijklmnopqrst", "functions-env.json")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil || os.WriteFile(secret, []byte(`{"jwt_secret":"x"}`), 0o600) != nil {
		t.Fatal("could not stub a tenant's file", err)
	}

	cfg.Functions.Enabled = false
	hs := r.m.Status(ctx)
	if len(hs) != 6 {
		t.Fatalf("status rows: %d, want the runtime's unit among them", len(hs))
	}
	var seen bool
	for _, h := range hs {
		if h.Service == config.SvcEdgeRuntime {
			seen = true
			if !h.Optional || h.Healthy || !strings.Contains(h.Error, "still runs") {
				t.Errorf("a running runtime of an earlier configuration: %+v", h)
			}
		}
	}
	if !seen {
		t.Fatal("no row for the runtime")
	}
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.sup.log(), "stop sb-studio.service\nstop sb-edge-runtime.service\nstop sb-storage.service") {
		t.Fatalf("fleet stop left the runtime running:\n%s", r.sup.log())
	}

	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.sup.log(), "remove sb-edge-runtime.service") {
		t.Fatalf("fleet start did not remove the runtime's unit:\n%s", r.sup.log())
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("a tenant's secrets stayed on disk: %v", err)
	}
	if _, err := os.Stat(cfg.Paths().FunctionsRoot()); !os.IsNotExist(err) {
		t.Fatalf("the tenants tree stayed: %v", err)
	}
}
