package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// Artifacts is the part of the artifact store the fleet needs.
type Artifacts interface {
	// Dir returns the unpacked artifact root of svc, or an error if it is not fetched.
	Dir(svc string) (string, error)
}

// Services lists the shared services in start order. Stop runs the other way round.
// postgres-meta first (it needs nothing), the three multi-tenant services next (they need
// the system cluster), Studio last (it is only a client of the Management API).
var Services = []string{config.SvcPGMeta, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcStudio}

// Deps is everything the fleet needs from the node. lifecycle.Node carries all of it
// (Cfg, Registry, Secrets, Supervisor, Artifacts).
type Deps struct {
	Cfg *config.Config
	Log *slog.Logger
	// Registry and Secrets hold the sealed secrets of the services (system project) and
	// the credentials of projects. Start, Specs, Setup and the tenants need them; Stop
	// and Status do not.
	Registry registry.Registry
	Secrets  secrets.Secrets
	// Supervisor runs the units (both backends).
	Supervisor units.Supervisor
	// Artifacts locates the unpacked service artifacts.
	Artifacts Artifacts

	// Start makes Setup render and start the services before it returns the tenants.
	Start bool
	// Skip names services (config.Svc*) that Start leaves alone, for example config.SvcStudio
	// on a node without a Studio artifact.
	Skip []string
	// ReadyTimeout bounds the wait for one service to answer after its unit started
	// (default 3 minutes: Realtime and Supavisor run their migrations first).
	ReadyTimeout time.Duration
	// ProbeClient is used for the health checks of Manager (default: 4 s timeout).
	ProbeClient *http.Client
	// TenantClient is used for the tenant calls of Setup (default: 2 minute timeout).
	// Creating a Realtime or Storage tenant runs that service's migrations in the
	// project's database before the call answers, so a client with a short timeout makes
	// tenant creation fail.
	TenantClient *http.Client
	// Retry bounds the retries of tenant calls (default: 5 attempts, 500 ms to 8 s backoff).
	Retry Retry
}

func (d *Deps) log() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return d.Log
}

func (d *Deps) skipped(svc string) bool {
	for _, s := range d.Skip {
		if s == svc {
			return true
		}
	}
	return false
}

// Health is the state of one shared service.
type Health struct {
	Service string // config.Svc*
	Unit    string
	Healthy bool
	// Status is ACTIVE_HEALTHY, COMING_UP, UNHEALTHY or STOPPED.
	Status string
	Error  string
	// Optional marks a service that is allowed to be absent: Studio, which needs our own
	// artifact, on a node that never rendered its unit. Callers should not call the fleet
	// unhealthy because of it.
	Optional bool
}

// AllHealthy reports whether every service that is supposed to run is healthy; an
// Optional service that is absent does not count.
func AllHealthy(hs []Health) bool {
	for _, h := range hs {
		if !h.Healthy && !h.Optional {
			return false
		}
	}
	return len(hs) > 0
}

// Manager renders, starts, stops and checks the units of the shared services.
type Manager struct {
	d    Deps
	http *http.Client
	log  *slog.Logger
}

// NewManager builds a Manager. It needs Cfg and Supervisor; Start also needs Registry,
// Secrets and Artifacts.
func NewManager(d Deps) (*Manager, error) {
	if d.Cfg == nil || d.Supervisor == nil {
		return nil, errors.New("fleet: Deps needs Cfg and Supervisor")
	}
	if d.ReadyTimeout == 0 {
		d.ReadyTimeout = 3 * time.Minute
	}
	hc := d.ProbeClient
	if hc == nil {
		hc = &http.Client{Timeout: 4 * time.Second}
	}
	return &Manager{d: d, http: hc, log: d.log()}, nil
}

func (m *Manager) cfg() *config.Config { return m.d.Cfg }

func unitOf(svc string) string { return config.UnitName(svc, "") }

// Addr is the loopback address of svc's main listener.
func Addr(cfg *config.Config, svc string) string {
	return "127.0.0.1:" + strconv.Itoa(Port(cfg, svc))
}

// Port is the port of svc's main listener: Supavisor's is its HTTP API (the public
// pooler ports are Ports.SupavisorSession and Ports.SupavisorTransaction).
func Port(cfg *config.Config, svc string) int {
	switch svc {
	case config.SvcSupavisor:
		return cfg.Fleet.SupavisorAPI()
	case config.SvcRealtime:
		return cfg.Ports.Realtime
	case config.SvcStorage:
		return cfg.Ports.Storage
	case config.SvcPGMeta:
		return cfg.Ports.PGMeta
	case config.SvcStudio:
		return cfg.Ports.Studio
	}
	return 0
}

// healthPath is the unauthenticated URL path that answers 2xx when svc works (Supavisor's
// answers 204).
func healthPath(svc string) string {
	switch svc {
	case config.SvcSupavisor:
		return "/api/health"
	case config.SvcRealtime:
		return "/healthcheck"
	case config.SvcStorage:
		return "/status"
	case config.SvcPGMeta:
		return "/health"
	case config.SvcStudio:
		return "/api/get-utc-time"
	}
	return "/"
}

// probe asks svc's health endpoint for a 2xx answer.
func (m *Manager) probe(ctx context.Context, svc string) error {
	url := "http://" + Addr(m.cfg(), svc) + healthPath(svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// Specs returns the unit specs of the services in start order, generating the services'
// secrets on first use. A service whose artifact is not fetched is an error naming it.
func (m *Manager) Specs(ctx context.Context) ([]units.Spec, error) {
	if m.d.Registry == nil || m.d.Secrets == nil || m.d.Artifacts == nil {
		return nil, errors.New("fleet: rendering units needs Deps.Registry, Secrets and Artifacts")
	}
	c, err := loadCreds(ctx, m.d, true)
	if err != nil {
		return nil, err
	}
	var specs []units.Spec
	var errs []error
	for _, svc := range Services {
		if m.d.skipped(svc) {
			continue
		}
		s, err := m.spec(svc, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", svc, err))
			continue
		}
		specs = append(specs, s)
	}
	return specs, errors.Join(errs...)
}

// studioReadyTimeout caps the wait for Studio, which nothing else depends on: a Studio
// that does not come up must not hold the boot of the node for the full ReadyTimeout.
const studioReadyTimeout = time.Minute

// Start renders and starts every service (except Deps.Skip) in order and waits until each
// answers its health endpoint. It is idempotent: a running service whose rendered files
// are unchanged is left alone, one whose files changed is restarted. A service that
// cannot start does not stop the others from starting; the errors are joined. Studio is
// optional: it needs our own artifact, and nothing depends on it, so when it cannot start
// (no artifact, a unit that fails) Start logs a warning and does not return an error; the
// failure shows in Status.
func (m *Manager) Start(ctx context.Context) error {
	if m.d.Registry == nil || m.d.Secrets == nil || m.d.Artifacts == nil {
		return errors.New("fleet: starting services needs Deps.Registry, Secrets and Artifacts")
	}
	c, err := loadCreds(ctx, m.d, true)
	if err != nil {
		return err
	}
	var errs []error
	for _, svc := range Services {
		if m.d.skipped(svc) {
			continue
		}
		spec, err := m.spec(svc, c)
		if err == nil {
			err = m.startOne(ctx, spec)
		}
		if err != nil && svc == config.SvcStudio {
			m.log.Warn("studio did not start; the other services are not affected", "error", err)
			continue
		}
		if err != nil {
			m.log.Error("fleet service did not start", "service", svc, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", svc, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) startOne(ctx context.Context, spec units.Spec) error {
	if err := os.MkdirAll(spec.WorkDir, 0o750); err != nil {
		return err
	}
	changed := false
	if cr, ok := m.d.Supervisor.(units.ChangeRenderer); ok {
		var err error
		if changed, err = cr.RenderChanged(ctx, spec); err != nil {
			return err
		}
	} else if err := m.d.Supervisor.Render(ctx, spec); err != nil {
		return err
	}
	unit := spec.Unit()
	if changed {
		// A running service keeps the environment it started with, so changed secrets,
		// ports or launcher paths need a restart.
		if st, err := m.d.Supervisor.Status(ctx, unit); err == nil && (st.State == units.StateActive || st.State == units.StateActivating) {
			m.log.Info("fleet service settings changed; restarting", "unit", unit)
			if err := m.d.Supervisor.Stop(ctx, unit); err != nil {
				return err
			}
		}
	}
	if err := m.d.Supervisor.Start(ctx, unit); err != nil {
		return err
	}
	timeout := m.d.ReadyTimeout
	if spec.Service == config.SvcStudio && timeout > studioReadyTimeout {
		timeout = studioReadyTimeout
	}
	return m.waitReady(ctx, spec.Service, unit, timeout)
}

// waitReady polls svc's health endpoint until it answers, the timeout passes, or the unit
// dies (failed, stopped or restarting), in which case it returns at once with the end of
// the unit's log when the backend keeps one.
func (m *Manager) waitReady(ctx context.Context, svc, unit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for delay := 100 * time.Millisecond; ; {
		if last = m.probe(ctx, svc); last == nil {
			return nil
		}
		if st, err := m.d.Supervisor.Status(ctx, unit); err == nil {
			if st.State == units.StateFailed || st.State == units.StateInactive || st.SubState == "auto-restart" {
				return fmt.Errorf("%s is %s/%s: %v%s", unit, st.State, st.SubState, last, m.logTail(unit))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %v%s", unit, timeout, last, m.logTail(unit))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay += 100 * time.Millisecond
		}
	}
}

func (m *Manager) logTail(unit string) string {
	if t, ok := m.d.Supervisor.(units.LogTailer); ok {
		if s := t.Tail(unit, 15); s != "" {
			return "\n" + strings.TrimRight(s, "\n")
		}
	}
	return " (see `journalctl -u " + unit + "`)"
}

// Stop stops every service (except Deps.Skip) in reverse start order. It does not touch
// the data. A unit that does not exist or does not run is not an error.
func (m *Manager) Stop(ctx context.Context) error {
	var errs []error
	for i := len(Services) - 1; i >= 0; i-- {
		svc := Services[i]
		if m.d.skipped(svc) {
			continue
		}
		if err := m.d.Supervisor.Stop(ctx, unitOf(svc)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", svc, err))
		}
	}
	return errors.Join(errs...)
}

// neverRendered reports whether svc is Studio and no unit was ever rendered for it.
func (m *Manager) neverRendered(svc string) bool {
	if svc != config.SvcStudio {
		return false
	}
	_, err := os.Stat(units.FilesFor(m.cfg(), units.Spec{Service: svc}).Run)
	return err != nil
}

// Status checks every service (except Deps.Skip): unit state plus a request to its
// health endpoint.
func (m *Manager) Status(ctx context.Context) []Health {
	var out []Health
	for _, svc := range Services {
		if m.d.skipped(svc) {
			continue
		}
		unit := unitOf(svc)
		h := Health{Service: svc, Unit: unit}
		st, err := m.d.Supervisor.Status(ctx, unit)
		switch {
		case err != nil:
			h.Status, h.Error = "STOPPED", err.Error()
			h.Optional = m.neverRendered(svc)
		case st.State == units.StateActivating:
			h.Status, h.Error = "COMING_UP", fmt.Sprintf("unit is %s/%s", st.State, st.SubState)
		case st.State != units.StateActive:
			h.Status, h.Error = "STOPPED", fmt.Sprintf("unit is %s/%s", st.State, st.SubState)
			if h.Optional = m.neverRendered(svc); h.Optional {
				h.Error = "never started on this node"
			}
		default:
			if err := m.probe(ctx, svc); err != nil {
				h.Status, h.Error = "UNHEALTHY", err.Error()
				if time.Since(st.Since) < 30*time.Second {
					h.Status = "COMING_UP"
				}
			} else {
				h.Healthy, h.Status = true, "ACTIVE_HEALTHY"
			}
		}
		out = append(out, h)
	}
	return out
}
