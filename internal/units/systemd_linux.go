//go:build linux

package units

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"

	"github.com/OWNER/sbctl/internal/config"
)

// Systemd is the production Supervisor. It drives systemd over D-Bus (pure Go, no
// systemctl) and relies on the static templates installed from deploy/systemd:
// each template runs <state_dir>/projects/<ref>/<svc>.run and reads
// <state_dir>/projects/<ref>/<svc>.env, which Render writes as the sbctl user. Per-unit
// MemoryMax and CPUQuota are persistent drop-ins that systemd writes itself
// (SetUnitProperties with runtime=false, under /etc/systemd/system.control), so
// Render never needs a daemon-reload. Reload is only needed after the templates
// change, which only root does (sbctl system install-units).
//
// Running as a non-root user needs the polkit rule in deploy/systemd/50-sbctl.rules,
// which grants manage-units on sb-* units and nothing else.
type Systemd struct {
	cfg *config.Config
	log *slog.Logger

	mu   sync.Mutex
	conn *sddbus.Conn
}

var _ Supervisor = (*Systemd)(nil)

// NewSystemd connects to systemd. The connection is re-established on demand.
func NewSystemd(cfg *config.Config, log *slog.Logger) (*Systemd, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Systemd{cfg: cfg, log: log}
	if _, err := s.dial(context.Background()); err != nil {
		return nil, fmt.Errorf("units: connect to systemd: %w", err)
	}
	return s, nil
}

func (s *Systemd) dial(ctx context.Context) (*sddbus.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	c, err := sddbus.NewWithContext(ctx)
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

// Close releases the D-Bus connection.
func (s *Systemd) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// Reload runs daemon-reload. Call it after installing or changing unit templates.
func (s *Systemd) Reload(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	return c.ReloadContext(ctx)
}

// Render implements Supervisor: it writes the env file and run script and applies the
// unit's resource limits if they differ from what systemd has.
func (s *Systemd) Render(ctx context.Context, spec Spec) error {
	_, err := s.RenderChanged(ctx, spec)
	return err
}

// RenderChanged implements ChangeRenderer. The limits are applied to a running unit
// immediately, so only the env file and run script count as a change.
func (s *Systemd) RenderChanged(ctx context.Context, spec Spec) (bool, error) {
	changed, err := renderFiles(s.cfg, spec)
	if err != nil {
		return false, err
	}
	return changed, s.applyLimits(ctx, spec.Unit(), spec.Limits)
}

func (s *Systemd) applyLimits(ctx context.Context, unit string, l config.Limits) error {
	mem, err := ParseBytes(l.MemoryMax)
	if err != nil {
		return err
	}
	cpu, err := ParseCPUQuota(l.CPUQuota)
	if err != nil {
		return err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	cur, err := c.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err == nil {
		m, _ := cur["MemoryMax"].(uint64)
		q, _ := cur["CPUQuotaPerSecUSec"].(uint64)
		if m == mem && q == cpu {
			return nil
		}
	}
	return c.SetUnitPropertiesContext(ctx, unit, false,
		sddbus.Property{Name: "MemoryMax", Value: godbus.MakeVariant(mem)},
		sddbus.Property{Name: "CPUQuotaPerSecUSec", Value: godbus.MakeVariant(cpu)},
	)
}

// Start implements Supervisor. It waits for the start job and returns an error if the
// job did not complete or the unit is not active afterwards.
func (s *Systemd) Start(ctx context.Context, unit string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	if st, err := s.Status(ctx, unit); err == nil && st.State == StateFailed {
		_ = c.ResetFailedUnitContext(ctx, unit)
	}
	ch := make(chan string, 1)
	if _, err := c.StartUnitContext(ctx, unit, "replace", ch); err != nil {
		return fmt.Errorf("units: start %s: %w", unit, err)
	}
	return waitJob(ctx, unit, "start", ch)
}

// Stop implements Supervisor. systemd applies the template's KillSignal (SIGINT for
// Postgres) and TimeoutStopSec.
func (s *Systemd) Stop(ctx context.Context, unit string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	ch := make(chan string, 1)
	if _, err := c.StopUnitContext(ctx, unit, "replace", ch); err != nil {
		var de godbus.Error
		if errors.As(err, &de) && de.Name == "org.freedesktop.systemd1.NoSuchUnit" {
			return nil
		}
		return fmt.Errorf("units: stop %s: %w", unit, err)
	}
	return waitJob(ctx, unit, "stop", ch)
}

func waitJob(ctx context.Context, unit, verb string, ch <-chan string) error {
	select {
	case res := <-ch:
		if res != "done" {
			return fmt.Errorf("units: %s %s job finished with %q (see `journalctl -u %s`)", verb, unit, res, unit)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status implements Supervisor.
func (s *Systemd) Status(ctx context.Context, unit string) (Status, error) {
	if _, _, err := ParseUnit(unit); err != nil {
		return Status{}, err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return Status{}, err
	}
	u, err := c.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return Status{}, fmt.Errorf("units: status %s: %w", unit, err)
	}
	st := Status{Unit: unit, State: StateUnknown}
	if v, ok := u["ActiveState"].(string); ok {
		st.State = State(v)
	}
	st.SubState, _ = u["SubState"].(string)
	if ts, ok := u["ActiveEnterTimestamp"].(uint64); ok && ts > 0 {
		st.Since = time.UnixMicro(int64(ts))
	}
	if sv, err := c.GetUnitTypePropertiesContext(ctx, unit, "Service"); err == nil {
		if pid, ok := sv["MainPID"].(uint32); ok {
			st.MainPID = int(pid)
		}
		if mem, ok := sv["MemoryCurrent"].(uint64); ok && mem != ^uint64(0) {
			st.MemoryBytes = mem
		}
	}
	return st, nil
}

// Remove implements Supervisor: stop the unit, lift its resource limits and delete
// the files Render wrote.
func (s *Systemd) Remove(ctx context.Context, unit string) error {
	files, _, _, err := runFilesFor(s.cfg, unit)
	if err != nil {
		return err
	}
	if err := s.Stop(ctx, unit); err != nil {
		return err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	_ = c.ResetFailedUnitContext(ctx, unit)
	if err := s.revert(ctx, unit); err != nil {
		s.log.Warn("could not remove unit drop-ins", "unit", unit, "error", err)
	}
	return removeFiles(files)
}

// revert lifts the limits SetLimits wrote by setting them back to infinity. Removing the
// drop-in files (RevertUnitFiles) would need the manage-unit-files polkit action, which
// the sbctl user must not hold; the inert drop-in that remains is harmless and a later
// project with the same ref overwrites it.
func (s *Systemd) revert(ctx context.Context, unit string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	inf := ^uint64(0)
	return c.SetUnitPropertiesContext(ctx, unit, false,
		sddbus.Property{Name: "MemoryMax", Value: godbus.MakeVariant(inf)},
		sddbus.Property{Name: "CPUQuotaPerSecUSec", Value: godbus.MakeVariant(inf)},
	)
}

var _ Enabler = (*Systemd)(nil)

// Sandboxed implements Sandboxer: the unit files confine their services.
func (*Systemd) Sandboxed() bool { return true }

// Enable makes units start at boot (systemctl enable). Template instances such as
// "sb-postgres@system.service" are accepted.
func (s *Systemd) Enable(ctx context.Context, units ...string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	if _, _, err := c.EnableUnitFilesContext(ctx, units, false, true); err != nil {
		return fmt.Errorf("units: enable %v: %w", units, err)
	}
	return s.Reload(ctx)
}
