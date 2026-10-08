package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

// handoffWait is how long BecomeLeader waits for the daemon to restart after it handed a move over.
// The membership closes Changed after two polls of two seconds, so the wait ends in seconds; the rest
// is for a poll that fails.
const handoffWait = 2 * time.Minute

// handoffTakeover is failover.Takeover for a daemon that restarts when its role changes (ErrRoleChanged).
//
// The orchestrator promotes the survivor's system cluster and then asks the node to become the leader.
// In the process that started as a follower the registry handle is read-only and the shared services
// run in follower mode, and no part of the daemon switches role in place, so that process cannot
// continue the move: it returns an error and the daemon ends, which systemd answers with a restart. The
// process that boots as the leader has recorded the leadership (cluster.AssumeLeadership) and asks the
// same question; the answer is yes, and resumeServerMove runs the rest of the move there.
type handoffTakeover struct {
	m   cluster.Membership
	log *slog.Logger
	// handing is set once a process gave its move over. The move ends with a failure in that process,
	// which the orchestrator announces; the move is not failed, so the alert is held back (notify).
	handing atomic.Bool
	// deliver sends an alert; nil is alerts.Notify.
	deliver func(context.Context, alerts.Event) error
	// wait is how long BecomeLeader waits for the restart; zero is handoffWait.
	wait time.Duration
}

var _ failover.Takeover = (*handoffTakeover)(nil)

func (t *handoffTakeover) BecomeLeader(ctx context.Context, epoch int64) error {
	if t.m.IsLeader() && t.m.Epoch() >= epoch {
		return nil // this process leads at the move's epoch already: it started after the promotion
	}
	t.handing.Store(true)
	t.log.Info("the system cluster is promoted; the daemon restarts as the leader and finishes the move there", "epoch", epoch)
	wait := t.wait
	if wait <= 0 {
		wait = handoffWait
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
	return fmt.Errorf("%w: the daemon restarts as the leader and finishes the move", ErrRoleChanged)
}

// notify delivers the orchestrator's alerts, except the failure of a move that ended because this
// process handed it over. A failure of the same move for another reason is announced.
func (t *handoffTakeover) notify(ctx context.Context, ev alerts.Event) {
	if t.handing.Load() && ev.Kind == alerts.KindFailoverFailed && strings.Contains(ev.Detail, ErrRoleChanged.Error()) {
		t.log.Info("the move continues in the daemon that restarts as the leader; no failure is announced", "detail", ev.Detail)
		return
	}
	deliver := t.deliver
	if deliver == nil {
		deliver = alerts.Notify
	}
	_ = deliver(ctx, ev)
}

// projectLocker is failover.Locker: the lock that serializes a move with the lifecycle operations on
// the same project (pause, resume, delete, upgrade). lifecycle.Engine takes a mutex in the process and,
// on a Postgres registry, a session advisory lock on a connection of its own, released when the
// connection closes even if the process dies (Engine.lock). The engine exports no way to take its
// lock, so this takes the same advisory lock, `pg_advisory_lock(hashtext('supavise:' || ref))`, which
// excludes the engine's operations in this process and in the CLI's. Where the registry has no
// advisory locks to give (a standby, an in-memory registry) the lock is a mutex per project in this
// process, which is what the engine has there too.
type projectLocker struct {
	reg func() registry.Registry
	mus sync.Map // ref -> chan struct{} of capacity 1
}

var _ failover.Locker = (*projectLocker)(nil)

func (l *projectLocker) Lock(ctx context.Context, ref string) (func(), error) {
	v, _ := l.mus.LoadOrStore(ref, make(chan struct{}, 1))
	mu := v.(chan struct{})
	select {
	case mu <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-mu }
	pool, ok := l.reg().(interface{ Pool() *pgxpool.Pool })
	if !ok || pool.Pool() == nil {
		return release, nil
	}
	conn, err := pgx.ConnectConfig(ctx, pool.Pool().Config().ConnConfig)
	if err != nil {
		release()
		return nil, fmt.Errorf("failover: lock %s: %w", ref, err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext('supavise:' || $1::text))`, ref); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "25006" { // a standby takes no advisory locks
			return release, nil
		}
		release()
		return nil, fmt.Errorf("failover: lock %s: %w", ref, err)
	}
	return func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
		release()
	}, nil
}
