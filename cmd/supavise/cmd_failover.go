package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/notimpl"
)

func init() {
	var to string
	var force, dryRun, resume, restoreMissing, oldDown, yes bool
	cmd := &cobra.Command{
		Use:   "failover",
		Short: "Move the whole server's work to another node",
		Long: `Makes another node the leader. With the old leader alive this is a planned switchover and
nothing is lost: the old leader stops cleanly, the survivor takes over, and the old leader follows. With
the old leader dead it is a failover: the survivor fences the old leader first, takes the service address,
promotes the system cluster and then every project's replica, and loses at most the replication lag.

Run it on the node that should lead. --dry-run prints each precondition. State is kept so that --resume
continues a run that stopped. A project with no replica is refused unless --restore-missing, which builds
its standby from the WAL archive (data loss up to archive_timeout).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise failover") },
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "the node to promote (default: this one)")
	f.BoolVar(&force, "force", false, "go on although a replica lags more than [failover] max_lag_seconds")
	f.BoolVar(&dryRun, "dry-run", false, "print the preconditions and what would happen, and change nothing")
	f.BoolVar(&resume, "resume", false, "continue the run that stopped")
	f.BoolVar(&restoreMissing, "restore-missing", false, "seed a project that has no replica from the archive")
	f.BoolVar(&oldDown, "old-primary-is-down", false, "assert that the old leader is down, when no fence is configured")
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	rootCmd.AddCommand(cmd)
}

// failoverStatus is the failover-readiness block of `supavise status`; nil means there is none to
// show (a node that is not part of a cluster).
func failoverStatus(ctx context.Context, cfg *config.Config) (*failover.Readiness, error) {
	return nil, nil
}
