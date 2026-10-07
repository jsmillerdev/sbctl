package config

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
	DefaultFunctionsReconcileSec   = 30
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
