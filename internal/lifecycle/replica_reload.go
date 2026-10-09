package lifecycle

import (
	"context"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/ctxutil"
	"github.com/supavise/supavise/internal/units"
)

// reloadGrace is how long a PostgREST that has just started is left alone: SIGUSR1 ends a process that
// has not installed its handler yet, and a tick that lands in the first moments after a (re)start
// would kill the unit.
const reloadGrace = 5 * time.Second

// ReloadSchema asks the PostgREST of ref's replica to rebuild its schema cache: it sends SIGUSR1 to the
// unit's main process (the run script execs PostgREST). It does nothing when the unit does not run or
// started within reloadGrace.
//
// PostgREST learns of a DDL change by a NOTIFY on the primary, and on a replica that notification can
// arrive before the WAL of the change was replayed, so that the reload reads the old schema and
// nothing triggers another one (spike S3: up to a third of the changes under 20 MB/s of WAL).
// Reloading on a timer makes every change visible within one interval and the replay lag.
func (pl *PostgresPlane) ReloadSchema(ctx context.Context, ref string) error {
	st, err := pl.sup.Status(ctx, config.UnitName(config.SvcPostgREST, ref))
	if err != nil {
		return err
	}
	if st.State != units.StateActive || st.MainPID <= 0 || (!st.Since.IsZero() && time.Since(st.Since) < reloadGrace) {
		return nil
	}
	return syscall.Kill(st.MainPID, syscall.SIGUSR1)
}

// RunSchemaReload calls ReloadSchema for every ref refs returns once per every, until ctx ends. The
// signals of one round are spread across the interval, every/len(refs) apart, so that the
// PostgREST processes of a node do not rebuild their schema caches at the same moment. The daemon
// runs it with [replicas] schema_reload_seconds and the refs of the replicas on this node.
func (pl *PostgresPlane) RunSchemaReload(ctx context.Context, every time.Duration, refs func(context.Context) ([]string, error)) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rs, err := refs(ctx)
		if err != nil {
			pl.log.Warn("schema reload: listing the replicas", "error", err)
			continue
		}
		gap := every / time.Duration(max(len(rs), 1))
		for i, ref := range rs {
			if i > 0 && ctxutil.Sleep(ctx, gap) != nil {
				return
			}
			if err := pl.ReloadSchema(ctx, ref); err != nil {
				pl.log.Warn("schema reload", "ref", ref, "error", err)
			}
		}
	}
}
