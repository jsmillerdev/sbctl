// Package units renders and drives the systemd units (or, in dev and tests, child
// processes) that run Supabase's native artifacts.
package units

import (
	"context"
	"time"

	"github.com/OWNER/sbctl/internal/config"
)

// Spec is everything needed to render one unit instance: the env file at
// config.Paths.EnvFile(ref, svc), resource limits as a drop-in, and the artifact
// directory the template's ExecStart points into.
type Spec struct {
	Service     string // config.Svc*
	Ref         string // project ref; "" for fleet singletons
	ArtifactDir string // unpacked artifact root (contains bin/)
	WorkDir     string // service state dir, e.g. config.Paths.ProjectService(ref, svc)
	Env         map[string]string
	Limits      config.Limits
	// Exec overrides the artifact launcher (path relative to ArtifactDir, plus args).
	// Empty means the service's standard launcher (StandardExec) without arguments.
	Exec []string
	// PreStart lists one-shot commands (path relative to ArtifactDir, plus args) run
	// in order before Exec, with the same environment; a failing command stops the
	// unit. GoTrue uses it for "bin/auth migrate".
	PreStart [][]string
}

// Unit returns the systemd unit name for the spec.
func (s Spec) Unit() string { return config.UnitName(s.Service, s.Ref) }

type State string

const (
	StateActive       State = "active"
	StateActivating   State = "activating"
	StateDeactivating State = "deactivating"
	StateInactive     State = "inactive"
	StateFailed       State = "failed"
	StateUnknown      State = "unknown"
)

type Status struct {
	Unit        string
	State       State
	SubState    string
	MainPID     int
	MemoryBytes uint64
	Since       time.Time
}

// Supervisor is implemented by the systemd (D-Bus) backend and by an exec backend
// that runs launchers as child processes for development and tests.
type Supervisor interface {
	// Render writes the env file and limits drop-in for spec and reloads the manager.
	// Rendering an unchanged spec is a no-op.
	Render(ctx context.Context, spec Spec) error
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Status(ctx context.Context, unit string) (Status, error)
	// Remove stops the unit and deletes everything Render wrote for it.
	Remove(ctx context.Context, unit string) error
}

// ChangeRenderer is implemented by both backends. RenderChanged is Render that also
// reports whether the env file or run script differed from what was on disk, which tells
// a caller that a running unit still runs on the old files and needs a restart.
type ChangeRenderer interface {
	RenderChanged(ctx context.Context, spec Spec) (changed bool, err error)
}

// StandardExec returns the artifact launcher of svc, relative to the artifact root.
func StandardExec(svc string) []string {
	switch svc {
	case config.SvcPostgres:
		return []string{"bin/supabase-postgres-start"}
	case config.SvcGoTrue:
		return []string{"bin/auth"}
	case config.SvcPostgREST:
		return []string{"bin/postgrest"}
	case config.SvcSupavisor, config.SvcRealtime:
		return []string{"bin/server"}
	case config.SvcStorage:
		return []string{"bin/storage"}
	case config.SvcPGMeta:
		return []string{"bin/pgmeta"}
	case config.SvcStudio:
		return []string{"bin/studio"}
	case config.SvcImgproxy:
		return []string{"bin/imgproxy"}
	case config.SvcEdgeRuntime:
		return []string{"bin/edge-runtime"}
	}
	return nil
}

// LogTailer is implemented by backends that keep a unit's output in a file the
// process can read (the exec backend). The systemd backend logs to journald only.
type LogTailer interface {
	Tail(unit string, lines int) string
}
