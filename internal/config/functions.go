package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults of the [functions] section. They follow the limits of hosted Edge Functions
// (256 MB per worker, 2 s of CPU time per request, a 400 s wall clock, 150 s without
// a response) so a function that works on one works on the other.
const (
	DefaultFunctionsMemoryMB       = 256
	DefaultFunctionsWallClockSec   = 400
	DefaultFunctionsIdleTimeoutSec = 150
	DefaultFunctionsCPUSoftMs      = 1000
	DefaultFunctionsCPUHardMs      = 2000
	DefaultFunctionsMaxParallelism = 16
	// DefaultFunctionsMaxPerProject applies when the runtime-wide cap is off.
	DefaultFunctionsMaxPerProject = 4
	// FunctionsRuntimeOverheadMB is what the runtime process needs besides its workers'
	// heaps (the Deno main service, the V8 and Tokio runtimes).
	FunctionsRuntimeOverheadMB   = 256
	DefaultFunctionsReconcileSec = 30
)

// Functions is the [functions] config section: Edge Functions, served by the
// edge-runtime artifact (unit sb-edge-runtime) with sbctl's tenant-aware main service
// from functions-main/. Off by default: a node without it does not fetch or start
// the runtime, and the proxy answers 503 on /functions/v1. The environment overrides
// are SBCTL_FUNCTIONS_*.
type Functions struct {
	// Enabled turns the feature on: the fleet starts sb-edge-runtime, the proxy routes
	// /functions/v1 to it and the API materializes deployments on disk.
	Enabled bool `toml:"enabled"`
	// MemoryMB is the heap limit of one worker. Zero means DefaultFunctionsMemoryMB.
	MemoryMB int `toml:"memory_mb"`
	// WallClockSeconds ends a worker that has lived this long, whatever it is doing.
	// Zero means DefaultFunctionsWallClockSec.
	WallClockSeconds int `toml:"wall_clock_seconds"`
	// IdleTimeoutSeconds answers 504 when a request got no response for this long
	// (the worker does not need to be busy). Zero means DefaultFunctionsIdleTimeoutSec.
	IdleTimeoutSeconds int `toml:"idle_timeout_seconds"`
	// CPUSoftMs and CPUHardMs bound the CPU time of one request: the worker is asked
	// to stop at the soft limit and killed at the hard one. Zero means the defaults;
	// a negative value switches the limit off.
	CPUSoftMs int `toml:"cpu_soft_ms"`
	CPUHardMs int `toml:"cpu_hard_ms"`
	// MaxParallelism caps the workers of the whole runtime alive at once. Zero means
	// DefaultFunctionsMaxParallelism; negative means no cap.
	MaxParallelism int `toml:"max_parallelism"`
	// MaxPerProject caps the requests of one project in flight at once in the shared
	// runtime; a request over it is answered 503 PROJECT_AT_CAPACITY. Zero means half of
	// MaxParallelism (at least 1); negative means no cap. Independently of it, a project
	// may have at most WorkersPerProject distinct live workers (MaxParallelism - 1), so it
	// can never hold every slot of the pool.
	MaxPerProject int `toml:"max_per_project"`
	// MemoryMax is the memory limit of sb-edge-runtime (systemd syntax, "2G"). Empty means
	// MaxParallelism x MemoryMB plus FunctionsRuntimeOverheadMB, the most the workers can
	// use together; a value below that fails validation, because a runtime that hits its
	// cgroup limit is killed whole, for every project.
	MemoryMax string `toml:"memory_max"`
	// ProjectURLTemplate is SUPABASE_URL as functions see it, with {ref} for the project
	// ref. Empty derives it from the domain, the TLS mode and the public listen ports
	// (https://<ref>.api.<domain>). Set it when functions must reach their own project
	// through another address, for example in a test with a non-default port.
	ProjectURLTemplate string `toml:"project_url_template"`
	// ReconcileSeconds is how often the API server compares the stored deployments,
	// keys and secrets with the files on disk. Zero means DefaultFunctionsReconcileSec.
	ReconcileSeconds int `toml:"reconcile_seconds"`
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// Memory returns the per-worker heap limit in MB with the default applied.
func (f Functions) Memory() int { return orDefault(f.MemoryMB, DefaultFunctionsMemoryMB) }

// WallClock returns the worker wall clock in seconds with the default applied.
func (f Functions) WallClock() int {
	return orDefault(f.WallClockSeconds, DefaultFunctionsWallClockSec)
}

// IdleTimeout returns the request idle timeout in seconds with the default applied.
func (f Functions) IdleTimeout() int {
	return orDefault(f.IdleTimeoutSeconds, DefaultFunctionsIdleTimeoutSec)
}

// CPUSoft returns the soft CPU limit in ms; 0 means off.
func (f Functions) CPUSoft() int { return limitOrOff(f.CPUSoftMs, DefaultFunctionsCPUSoftMs) }

// CPUHard returns the hard CPU limit in ms; 0 means off.
func (f Functions) CPUHard() int { return limitOrOff(f.CPUHardMs, DefaultFunctionsCPUHardMs) }

// Parallelism returns the worker cap; 0 means no cap.
func (f Functions) Parallelism() int {
	return limitOrOff(f.MaxParallelism, DefaultFunctionsMaxParallelism)
}

// PerProject returns the cap on one project's requests and workers; 0 means no cap.
func (f Functions) PerProject() int {
	switch n := f.MaxPerProject; {
	case n < 0:
		return 0
	case n > 0:
		return n
	}
	if p := f.Parallelism(); p > 0 {
		return max(1, p/2)
	}
	return DefaultFunctionsMaxPerProject
}

// WorkersPerProject returns how many distinct live workers (functions) one project may
// have: one less than the runtime-wide cap, so a project never holds every slot. 0 means
// no cap (the runtime-wide cap is off).
func (f Functions) WorkersPerProject() int {
	if p := f.Parallelism(); p > 0 {
		return max(1, p-1)
	}
	return 0
}

// RuntimeMemoryMax returns the MemoryMax of sb-edge-runtime: the configured value, or when
// workers are capped the most they can use together plus the runtime's own needs. Empty
// means the [defaults] limit applies (workers are uncapped and no value was set).
func (f Functions) RuntimeMemoryMax() string {
	if f.MemoryMax != "" {
		return f.MemoryMax
	}
	if p := f.Parallelism(); p > 0 {
		return strconv.Itoa(p*f.Memory()+FunctionsRuntimeOverheadMB) + "M"
	}
	return ""
}

// Validate checks that the limits agree: the workers the runtime may hold at once must
// fit into its memory limit.
func (f Functions) Validate() error {
	if !f.Enabled || f.MemoryMax == "" || f.Parallelism() == 0 {
		return nil
	}
	limit, finite, err := parseSystemdSize(f.MemoryMax)
	if err != nil {
		return fmt.Errorf("config: functions.memory_max: %w", err)
	}
	need := uint64(f.Parallelism()*f.Memory()+FunctionsRuntimeOverheadMB) << 20
	if finite && limit < need {
		return fmt.Errorf("config: functions.memory_max %s is below max_parallelism (%d) x memory_mb (%d) + %d MB = %d MB: the runtime would be killed for every project when its workers use their heaps; raise memory_max or lower max_parallelism or memory_mb",
			f.MemoryMax, f.Parallelism(), f.Memory(), FunctionsRuntimeOverheadMB, need>>20)
	}
	return nil
}

// parseSystemdSize reads a size in systemd's syntax: a number with an optional K, M, G or
// T suffix (powers of 1024), or "infinity" (finite false).
func parseSystemdSize(s string) (n uint64, finite bool, err error) {
	s = strings.TrimSpace(s)
	if s == "infinity" {
		return 0, false, nil
	}
	mult := uint64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K', 'k':
			mult, s = 1<<10, s[:len(s)-1]
		case 'M', 'm':
			mult, s = 1<<20, s[:len(s)-1]
		case 'G', 'g':
			mult, s = 1<<30, s[:len(s)-1]
		case 'T', 't':
			mult, s = 1<<40, s[:len(s)-1]
		}
	}
	v, perr := strconv.ParseUint(s, 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("invalid size %q (use a number with K, M, G or T, or infinity)", s)
	}
	return v * mult, true, nil
}

// Reconcile returns the reconcile interval in seconds with the default applied.
func (f Functions) Reconcile() int {
	return orDefault(f.ReconcileSeconds, DefaultFunctionsReconcileSec)
}

func limitOrOff(v, d int) int {
	switch {
	case v == 0:
		return d
	case v < 0:
		return 0
	}
	return v
}

// FunctionsRoot is where Edge Functions live on disk: one directory per project ref, each
// with functions-env.json and functions/. It sits inside the state directory of
// sb-edge-runtime (system/edge-runtime/tenants), the one directory the unit's mount
// namespace shows besides the artifacts, so the runtime sees the functions and nothing
// of the projects' clusters, sockets and unit files. internal/functions writes it,
// functions-main/ reads it.
func (p Paths) FunctionsRoot() string {
	return filepath.Join(p.System(SvcEdgeRuntime), "tenants")
}
