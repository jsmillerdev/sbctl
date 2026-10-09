package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// A node whose system cluster is a hot standby follows the leader of its cluster. It runs the
// shared services in a profile of its own (design 2.6):
//
//   - Supavisor runs, against the replicated _supavisor: its tenant rows, users and secrets are
//     the leader's, and the same VAULT_ENC_KEY (a sealed registry secret every node reads) opens
//     them. Its unit has no bin/prepare in front of it, which migrates a database and exits 1 on
//     a standby. A tenant row that changes on the leader reaches the follower's Supavisor only
//     after RefreshTenant (GET /api/tenants/<id>/terminate), once the standby has replayed the change.
//   - pg-meta, Realtime, Storage, Edge Runtime and Studio are parked: their units are rendered,
//     so the artifacts and files are in place, and stopped. Their ports belong to the mesh's
//     forwarders, which carry a connection to the leader's service (invariant I3).
//   - Nothing is written. The credentials are read, not generated, and the tenants skip their
//     Ensure and Remove calls: those are the leader's, and the fingerprint they keep in the
//     registry could not be saved on a standby.
//
// Which profile applies is read from the system cluster itself (pg_is_in_recovery(), invariant
// I1), so a boot, a CLI call and a promotion agree without being told. Apply sets it by hand.

// Mode is what a node runs of the shared services.
type Mode int

const (
	// ModeLeader runs every service with its tenants. It is also a node that is part of no cluster.
	ModeLeader Mode = iota
	// ModeFollower runs Supavisor on the replicated _supavisor and parks the rest.
	ModeFollower
	// ModeStopped runs nothing: a fenced node.
	ModeStopped
)

func (m Mode) String() string {
	switch m {
	case ModeFollower:
		return "follower"
	case ModeStopped:
		return "stopped"
	}
	return "leader"
}

// Apply puts the node's shared services in mode, whatever the system cluster says: the daemon
// calls it when the cluster's role changes (a promotion starts the services the follower parked,
// a demotion stops them). It is Start with the profile fixed, or Stop.
func (m *Manager) Apply(ctx context.Context, mode Mode) error {
	if mode == ModeStopped {
		if err := m.Stop(ctx); err != nil {
			return err
		}
		m.recordMode(ModeStopped)
		return nil
	}
	follower := mode == ModeFollower
	return m.start(ctx, &follower)
}

// follower says which profile applies: forced when the caller fixed it, else Deps.Follower, else
// what the registry's cluster says.
func (m *Manager) follower(ctx context.Context, forced *bool) (bool, error) {
	if forced != nil {
		return *forced, nil
	}
	return m.d.isFollower(ctx)
}

func (d Deps) isFollower(ctx context.Context) (bool, error) {
	if d.Follower != nil {
		return d.Follower(ctx)
	}
	if p, ok := d.Registry.(interface{ Pool() *pgxpool.Pool }); ok && p.Pool() != nil {
		var inRecovery bool
		err := p.Pool().QueryRow(ctx, `select pg_is_in_recovery()`).Scan(&inRecovery)
		return inRecovery, err
	}
	return false, nil
}

// park renders a service's unit and makes sure it is not running. A follower does this for every
// service but Supavisor, and a demoted leader needs the stop.
func (m *Manager) park(ctx context.Context, spec units.Spec) error {
	if cr, ok := m.d.Supervisor.(units.ChangeRenderer); ok {
		if _, err := cr.RenderChanged(ctx, spec); err != nil {
			return err
		}
	} else if err := m.d.Supervisor.Render(ctx, spec); err != nil {
		return err
	}
	unit := spec.Unit()
	st, err := m.d.Supervisor.Status(ctx, unit)
	if err == nil && (st.State == units.StateActive || st.State == units.StateActivating) {
		m.log.Info("this node follows the leader; stopping a shared service that belongs to it", "unit", unit)
		return m.d.Supervisor.Stop(ctx, unit)
	}
	return nil
}

// isCold reports whether svc is parked on a follower.
func isCold(follower bool, svc string) bool { return follower && svc != config.SvcSupavisor }

// Parked reports whether the daemon of a follower parks the shared service svc: it renders the
// unit for the release it runs and keeps it stopped, every service but Supavisor (Start).
func Parked(follower bool, svc string) bool { return isCold(follower, svc) }

func modeOf(follower bool) Mode {
	if follower {
		return ModeFollower
	}
	return ModeLeader
}

// modeFile records the profile Start last applied, for a caller that has no registry to ask
// (`supavise fleet status`).
func modeFile(cfg *config.Config) string {
	return filepath.Join(cfg.Paths().System(""), "fleet-mode")
}

func (m *Manager) recordMode(mode Mode) {
	path := modeFile(m.cfg())
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err == nil {
		err = os.WriteFile(path, []byte(mode.String()+"\n"), 0o640)
		if err != nil {
			m.log.Debug("could not record the fleet mode", "error", err)
		}
	}
}

// followerForStatus answers Status, which cannot fail: the registry's cluster when there is one
// to ask, else the profile Start last recorded.
func (m *Manager) followerForStatus(ctx context.Context) bool {
	if m.d.Follower != nil || m.d.Registry != nil {
		if f, err := m.d.isFollower(ctx); err == nil {
			return f
		}
	}
	b, _ := os.ReadFile(modeFile(m.cfg()))
	return strings.TrimSpace(string(b)) == ModeFollower.String()
}

// followerGate lets a tenant skip the calls that belong to the leader. Its function asks the
// system cluster on every call: a Lazy keeps its tenants for the life of the daemon, and a
// promotion must not leave them skipping. A nil function is a leader.
type followerGate struct {
	following func(ctx context.Context) (bool, error)
}

// skip reports whether a tenant call is the leader's. The error is the question failing: the
// call does not go ahead, because a write to a standby is refused and one to a leader that was
// taken for a follower would be lost.
func (g followerGate) skip(ctx context.Context) (bool, error) {
	if g.following == nil {
		return false, nil
	}
	return g.following(ctx)
}

// gate returns the follower gate the tenants of d share.
func (d Deps) gate() followerGate {
	return followerGate{following: d.isFollower}
}
