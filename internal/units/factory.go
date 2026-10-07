package units

import (
	"context"
	"log/slog"

	"github.com/OWNER/sbctl/internal/config"
)

// New returns the backend cfg.Supervisor selects: "systemd" (D-Bus) or "exec".
func New(cfg *config.Config, log *slog.Logger) (Supervisor, error) {
	if cfg.Supervisor == config.SupervisorExec {
		return NewExec(cfg, log), nil
	}
	return NewSystemd(cfg, log)
}

// Enabler is implemented by backends that can make units start at boot. Only the
// system project and the fleet are enabled; ordinary projects are started by the
// daemon from the registry, so that a paused project stays paused across reboots.
type Enabler interface {
	Enable(ctx context.Context, units ...string) error
}
