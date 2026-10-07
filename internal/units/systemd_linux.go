//go:build linux

package units

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	if err := s.applyLimits(ctx, spec.Unit(), spec.Limits); err != nil {
		return changed, err
	}
	return changed, s.applyEgress(ctx, spec.Unit(), spec.DenyEgress)
}

// EnforcesEgress implements EgressEnforcer.
func (s *Systemd) EnforcesEgress() bool { return true }

var _ EgressEnforcer = (*Systemd)(nil)

// applyEgress makes the unit's IPAddressDeny and IPAddressAllow what spec.DenyEgress says.
// Like the limits it is a persistent drop-in written by systemd itself, so it survives
// restarts and reboots, and a running unit has it at once (the cgroup's BPF filter changes).
// A unit whose lists already are what the policy says (including one that never had a restriction and
// wants none) is not touched; one confined by an earlier release with a wider allow list is narrowed (a
// unit that systemd does not have loaded counts as having none; Remove clears the drop-in explicitly).
func (s *Systemd) applyEgress(ctx context.Context, unit string, deny bool) error {
	if !deny && !egressManaged(unit) {
		return nil
	}
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	cur, err := c.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		cur = nil // not loaded: it has no restriction
	}
	if egressMatches(cur, deny) {
		return nil
	}
	return s.setEgress(ctx, c, unit, deny)
}

func (s *Systemd) setEgress(ctx context.Context, c *sddbus.Conn, unit string, deny bool) error {
	toDBus := func(rs []IPRange) []dbusIPAddr {
		out := make([]dbusIPAddr, 0, len(rs))
		for _, r := range rs {
			out = append(out, dbusIPAddr(r))
		}
		return out
	}
	set := func(denied, allowed []dbusIPAddr) error {
		return c.SetUnitPropertiesContext(ctx, unit, false,
			sddbus.Property{Name: "IPAddressAllow", Value: godbus.MakeVariant(allowed)},
			sddbus.Property{Name: "IPAddressDeny", Value: godbus.MakeVariant(denied)},
		)
	}
	if deny {
		return set(toDBus(EgressDeny()), toDBus(EgressAllow()))
	}
	// Lifting the restriction must not lift the template's own metadata-service deny: an empty list
	// resets the property, the unit file's value included, so reset both lists and then set the
	// metadata deny again (two calls, because whether a non-empty list replaces or extends the
	// current one is not something the lift may depend on).
	if err := set([]dbusIPAddr{}, []dbusIPAddr{}); err != nil {
		return err
	}
	return c.SetUnitPropertiesContext(ctx, unit, false,
		sddbus.Property{Name: "IPAddressDeny", Value: godbus.MakeVariant(toDBus(IMDSDeny()))},
	)
}

// dbusIPAddr is the a(iayu) element of IPAddressAllow and IPAddressDeny; godbus encodes a Go
// struct as a D-Bus struct of its fields in order.
type dbusIPAddr struct {
	Family int32
	Addr   []byte
	Prefix uint32
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
	files, svc, _, err := runFilesFor(s.cfg, unit)
	if err != nil {
		return err
	}
	if svc == config.SvcEdgeBundle {
		// The bundler's instance of a project that never had sources bundled has nothing to
		// remove, and loading its unit just to find that out would leave a drop-in behind.
		if _, err := os.Stat(files.Env); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err := s.Stop(ctx, unit); err != nil {
		return err
	}
	if svc == config.SvcEdgeBundle {
		// Done before the rendered files go, so that a failure leaves the unit to try again.
		if err := s.cleanCache(ctx, unit); err != nil {
			return fmt.Errorf("units: remove the module cache of %s: %w", unit, err)
		}
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

// cleanCache removes the module cache of the bundler instance unit, which is private to the
// instance's dynamic uid (CacheDirectory=), so the daemon cannot delete it. A user other than
// root cannot ask systemd to do it either (CleanUnit is refused by the D-Bus policy of
// systemd, before polkit is asked), so a one-shot unit that the polkit rule lets the sbctl user
// start does the removal as root (deploy/systemd/sb-edge-bundle-clean@.service).
func (s *Systemd) cleanCache(ctx context.Context, unit string) error {
	_, ref, err := ParseUnit(unit)
	if err != nil {
		return err
	}
	return s.Start(ctx, config.EdgeBundleCleanUnit(ref))
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
	if err := c.SetUnitPropertiesContext(ctx, unit, false,
		sddbus.Property{Name: "MemoryMax", Value: godbus.MakeVariant(inf)},
		sddbus.Property{Name: "CPUQuotaPerSecUSec", Value: godbus.MakeVariant(inf)},
	); err != nil {
		return err
	}
	// A branch's Postgres unit may carry an egress restriction, and the drop-in stays on disk
	// after the unit stops; a later project that reuses the ref (a reset keeps it) must not
	// inherit it. The unit may be unloaded by now, so its properties cannot tell: clear it.
	if !egressManaged(unit) {
		return nil
	}
	return s.setEgress(ctx, c, unit, false)
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
