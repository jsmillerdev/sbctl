package app

import (
	"context"
	"time"

	"github.com/supavise/supavise/internal/storagemigrate"
	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

// wireStorageMigrate looks at the record `supavise storage migrate` keeps (design 2.11) when the
// daemon starts. The command does the work in its own process; what the daemon owes it is to tidy
// up after one that died: a write hold that expired is removed, and a run that stopped before it
// finished is named in the log with the command that continues it. On a node that never migrated
// there is no record and it does nothing.
func wireStorageMigrate(ctx context.Context, w *Wire) error {
	p := w.Cfg.Paths()
	if hold.ClearStale(p, time.Now()) {
		w.Log.Warn("removed the write hold of a Storage migration that is no longer running")
	}
	st, err := storagemigrate.ReadState(p)
	if err != nil {
		w.Log.Warn("cannot read the record of the Storage migration", "err", err)
		return nil
	}
	if st != nil && !st.Phase.Terminal() {
		w.Log.Warn("a Storage migration stopped before it finished; continue it with `supavise storage migrate --resume`, or look at it with --status",
			"phase", st.Phase, "step", st.Step, "bucket", st.Dest.Bucket, "error", st.Error)
	}
	return nil
}
