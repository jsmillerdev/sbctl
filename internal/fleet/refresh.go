package fleet

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A follower's Supavisor reads its tenants from the follower's own standby of the system
// cluster, and keeps the target of a tenant until Refresher.RefreshTenant. The refresh must come
// after the standby has replayed the change, or Supavisor reads the old row again (spike S1:
// the harness waited for replay first). The leader therefore sends the WAL position of its write
// with the request (peerapi.RefreshRequest), and the follower drops its copy once replay has
// passed it. PeerRefresh is the follower's half; PeerRefresher is what the leader's callers use.

// ErrInvalid wraps the refusal of a tenant or a WAL position that is not one.
var ErrInvalid = errors.New("fleet: invalid request")

// ErrReplayBehind is returned when the standby did not reach the position in time. The tenant is
// not refreshed; the caller tries again.
var ErrReplayBehind = errors.New("fleet: the system standby has not replayed the change yet")

// PeerRefresher asks the other nodes that run Supavisor to drop their cached copy of a tenant,
// once their standby has replayed the leader's write. A caller on the leader changes the tenant
// row (EnsureTenant, EnsureReplicaTenant, a role password), refreshes its own Supavisor with
// Fleet.RefreshTenant, then calls this. It is nil on a node that is part of no cluster.
//
// The error joins those of every node that failed, and a node that is active in the registry but
// down costs its whole timeout (20 seconds in the daemon's implementation). A caller treats it as a
// warning about those nodes and not as a failure of the change it made: a refresh is idempotent, it
// can be sent again, and a node that missed it reads the replicated row when its Supavisor next
// needs it. ReplicaPooler logs it and goes on.
type PeerRefresher interface {
	RefreshPeers(ctx context.Context, tenant string) error
}

// Replay says how far the node's system cluster has replayed.
type Replay interface {
	// ReplayedPast reports whether replay has reached lsn. A cluster that is not in recovery has
	// replayed everything it has.
	ReplayedPast(ctx context.Context, lsn string) (bool, error)
}

// lsnRe is the text form of a pg_lsn.
var lsnRe = regexp.MustCompile(`^[0-9A-Fa-f]{1,8}/[0-9A-Fa-f]{1,8}$`)

// ValidLSN reports whether s is the text form of a pg_lsn ("0/3000100").
func ValidLSN(s string) bool { return lsnRe.MatchString(s) }

// PGReplay is the Replay of a pool on the system cluster, and the leader's way to read its
// position.
type PGReplay struct{ Pool *pgxpool.Pool }

// ReplayedPast implements Replay.
func (r PGReplay) ReplayedPast(ctx context.Context, lsn string) (bool, error) {
	if !ValidLSN(lsn) {
		return false, fmt.Errorf("%w: %q is not a WAL position", ErrInvalid, lsn)
	}
	var past bool
	err := r.Pool.QueryRow(ctx,
		`select case when pg_is_in_recovery() then pg_last_wal_replay_lsn() >= $1::pg_lsn else true end`, lsn).Scan(&past)
	return past, err
}

// CurrentLSN is the leader's pg_current_wal_lsn(): every write it committed so far lies before it.
func (r PGReplay) CurrentLSN(ctx context.Context) (string, error) {
	var lsn string
	err := r.Pool.QueryRow(ctx, `select pg_current_wal_lsn()::text`).Scan(&lsn)
	return lsn, err
}

// PeerRefresh serves the follower's side of the refresh: wait for replay, then drop the
// Supavisor's copy of the tenant.
type PeerRefresh struct {
	Replay Replay
	// Refresh drops the tenant's cached copy: Fleet.RefreshTenant of the node's lazy fleet.
	Refresh Refresher
	// Runs reports whether this node runs Supavisor; nil means it does.
	Runs func() bool
	// Wait bounds the wait for replay (default 10 s); Poll is how often replay is looked at
	// (default 50 ms).
	Wait, Poll time.Duration
}

// Do waits until replay has reached lsn (not at all when lsn is empty) and refreshes tenant. It
// returns false, and does nothing, on a node that does not run Supavisor.
func (p PeerRefresh) Do(ctx context.Context, tenant, lsn string) (bool, error) {
	if err := validTenantID(tenant); err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if lsn != "" && !ValidLSN(lsn) {
		return false, fmt.Errorf("%w: %q is not a WAL position", ErrInvalid, lsn)
	}
	if p.Runs != nil && !p.Runs() {
		return false, nil
	}
	if lsn != "" {
		if err := p.waitReplay(ctx, lsn); err != nil {
			return false, err
		}
	}
	if err := p.Refresh.RefreshTenant(ctx, tenant); err != nil {
		return false, err
	}
	return true, nil
}

func (p PeerRefresh) waitReplay(ctx context.Context, lsn string) error {
	wait, poll := p.Wait, p.Poll
	if wait <= 0 {
		wait = 10 * time.Second
	}
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		past, err := p.Replay.ReplayedPast(ctx, lsn)
		if err != nil {
			return err
		}
		if past {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w (position %s)", ErrReplayBehind, lsn)
		case <-tick.C:
		}
	}
}
