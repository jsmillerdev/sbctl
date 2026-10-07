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
	// DefaultFunctionsMaxParallelism is the workers one function may have at once. The
	// runtime's per_worker policy sends every request of a function to its one worker, so a
	// second one exists only for a moment (a cold burst); 1 keeps the memory budget exact.
	DefaultFunctionsMaxParallelism = 1
	// DefaultFunctionsMaxWorkers is the functions (distinct live workers) the whole runtime
	// may keep warm at once, across all projects, when neither max_workers nor memory_max
	// says otherwise.
	DefaultFunctionsMaxWorkers = 16
	// DefaultFunctionsMaxPerProject is the requests one project may have in flight at once.
	// It is generous because a worker serves many requests at a time; the memory budget is
	// the worker cap, not this one.
	DefaultFunctionsMaxPerProject = 128
	// FunctionsRuntimeOverheadMB is what the runtime process needs besides its workers'
	// heaps (the Deno main service, the V8 and Tokio runtimes).
	FunctionsRuntimeOverheadMB = 256
	// FunctionsWorkerOverheadMB is added to memory_mb for every worker: the heap limit does
	// not cover an isolate's own structures, buffers and compiled code.
	FunctionsWorkerOverheadMB    = 32
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
	// MaxParallelism is how many workers ONE function (one deployment of one slug of one
	// project) may have at once: edge-runtime's --max-parallelism, which in v1.77.4 is a
	// semaphore per pool key, not a limit on the runtime. Zero means
	// DefaultFunctionsMaxParallelism. It is not the cap on the runtime's workers; see
	// MaxWorkers.
	MaxParallelism int `toml:"max_parallelism"`
	// MaxWorkers caps the live workers of the whole runtime, all projects together; the
	// main service enforces it (functions-main/src/limiter.ts) by refusing a request that
	// would need a worker over the cap with 503 PROJECT_AT_CAPACITY. Zero derives it from
	// memory_max (see Workers), or DefaultFunctionsMaxWorkers when that is unset; negative
	// means no cap.
	MaxWorkers int `toml:"max_workers"`
	// MaxWorkersPerProject caps the live workers of one project, so one project cannot
	// take the whole budget. Zero means half of MaxWorkers (at least 1); negative means
	// the whole budget.
	MaxWorkersPerProject int `toml:"max_workers_per_project"`
	// MaxPerProject caps the requests of one project in flight at once; a request over it
	// is answered 503 PROJECT_AT_CAPACITY. Zero means DefaultFunctionsMaxPerProject;
	// negative means no cap.
	MaxPerProject int `toml:"max_per_project"`
	// MemoryMax is the memory limit of sb-edge-runtime (systemd syntax, "2G"). Empty means
	// Workers x worker cost plus FunctionsRuntimeOverheadMB, the most the workers can use
	// together. A value below what max_workers needs fails validation, because a runtime
	// that hits its cgroup limit is killed whole, for every project. When max_workers is
	// zero, memory_max sets it.
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

// Parallelism returns the workers one function may have at once (at least 1).
func (f Functions) Parallelism() int {
	if f.MaxParallelism < 1 {
		return DefaultFunctionsMaxParallelism
	}
	return f.MaxParallelism
}

// WorkerCostMB is what one live function counts against the memory budget: its heap limit
// and isolate overhead, for every worker it may have at once.
func (f Functions) WorkerCostMB() int {
	return f.Parallelism() * (f.Memory() + FunctionsWorkerOverheadMB)
}

// PerProject returns the cap on one project's requests in flight; 0 means no cap.
func (f Functions) PerProject() int {
	switch n := f.MaxPerProject; {
	case n < 0:
		return 0
	case n > 0:
		return n
	}
	return DefaultFunctionsMaxPerProject
}

// Workers returns the cap on live workers (functions kept warm) of the whole runtime;
// 0 means no cap. An explicit max_workers wins; otherwise a finite memory_max decides how
// many workers fit into it, and without either the default applies.
func (f Functions) Workers() int {
	switch n := f.MaxWorkers; {
	case n < 0:
		return 0
	case n > 0:
		return n
	}
	if f.MemoryMax != "" {
		if limit, finite, err := parseSystemdSize(f.MemoryMax); err == nil && finite {
			room := int64(limit>>20) - FunctionsRuntimeOverheadMB
			return int(max(room, 0) / int64(f.WorkerCostMB()))
		}
	}
	return DefaultFunctionsMaxWorkers
}

// WorkersPerProject returns how many live workers (functions) one project may have: its
// share of Workers, so that one project cannot hold the whole budget. 0 means no cap
// (the runtime-wide cap is off).
func (f Functions) WorkersPerProject() int {
	total := f.Workers()
	switch n := f.MaxWorkersPerProject; {
	case total == 0:
		return max(n, 0)
	case n < 0:
		return total
	case n > 0:
		return min(n, total)
	}
	return max(1, total/2)
}

// RuntimeMemoryMax returns the MemoryMax of sb-edge-runtime: the configured value, or when
// workers are capped the most they can use together plus the runtime's own needs. Empty
// means the [defaults] limit applies (workers are uncapped and no value was set).
func (f Functions) RuntimeMemoryMax() string {
	if f.MemoryMax != "" {
		return f.MemoryMax
	}
	if w := f.Workers(); w > 0 {
		return strconv.Itoa(w*f.WorkerCostMB()+FunctionsRuntimeOverheadMB) + "M"
	}
	return ""
}

// Validate checks that the limits agree: the workers the runtime may hold at once must
// fit into its memory limit.
func (f Functions) Validate() error {
	if !f.Enabled {
		return nil
	}
	if f.MaxParallelism < 0 {
		return fmt.Errorf("config: functions.max_parallelism %d must be positive: it is the workers one function may have at once, not a cap on the runtime (use max_workers for that)", f.MaxParallelism)
	}
	if f.MemoryMax == "" {
		return nil
	}
	limit, finite, err := parseSystemdSize(f.MemoryMax)
	if err != nil {
		return fmt.Errorf("config: functions.memory_max: %w", err)
	}
	if !finite || f.MaxWorkers < 0 {
		return nil
	}
	workers := f.Workers()
	need := uint64(max(workers, 1)*f.WorkerCostMB()+FunctionsRuntimeOverheadMB) << 20
	if workers < 1 || limit < need {
		return fmt.Errorf("config: functions.memory_max %s cannot hold %d worker(s) of %d MB (memory_mb %d + %d MB overhead, x %d per function) plus %d MB for the runtime (%d MB needed): the runtime would be killed for every project when its workers use their heaps; raise memory_max or lower max_workers or memory_mb",
			f.MemoryMax, max(workers, 1), f.WorkerCostMB(), f.Memory(), FunctionsWorkerOverheadMB, f.Parallelism(), FunctionsRuntimeOverheadMB, need>>20)
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
