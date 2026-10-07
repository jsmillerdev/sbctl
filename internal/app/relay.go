package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
)

// StartWALRelay starts the WAL relay over the configured backend (nil and a no-op stop
// when the node does not archive through the daemon, config.Backup.WALRelay). The relay
// outlives ctx: stop ends it, so a caller can keep archiving available while it drains.
//
// The daemon calls it with cli false. A command-line process that has to wait for WAL to
// be archived (`sbctl backups create`, a delete's final base backup) calls it with cli
// true while the daemon may be down: it serves only the sockets that nobody answers, and
// looks for them more often.
//
// The backend is opened on the first request that needs it and again after a failure, so
// a bucket that is unreachable at boot keeps neither the daemon from starting nor the
// relay from recovering.
func StartWALRelay(ctx context.Context, cfg *config.Config, log *slog.Logger, cli bool) (*backup.Relay, func()) {
	if !cfg.WALRelayEnabled() {
		return nil, func() {}
	}
	o := backup.RelayOptions{
		Config: cfg,
		Log:    log.With("component", "wal-relay"),
		Service: func(ctx context.Context) (*backup.Service, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			store, err := backup.OpenStore(ctx, cfg.Backup)
			if err != nil {
				return nil, err
			}
			return backup.New(backup.Options{Config: cfg, Store: store, Log: log})
		},
	}
	if cli {
		o.SkipServed, o.Interval = true, time.Second
	}
	relay := backup.NewRelay(o)
	rctx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() { defer close(done); _ = relay.Run(rctx) }()
	return relay, func() { stopRun(); <-done }
}
