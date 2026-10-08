package main

import (
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/notimpl"
)

func init() {
	var to string
	var force, dryRun, resume bool
	cmd := &cobra.Command{
		Use:   "failover <ref>",
		Short: "Move one project's primary to the node that holds its replica",
		Long: `Promotes the project's replica and makes the old primary a replica in its place, so the
project's endpoints, pooler and Realtime reconnect with no change of address. With the old primary alive
nothing is lost. --dry-run prints each precondition; --resume continues a move that stopped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise projects failover") },
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "the node to move the project to (default: the healthiest replica)")
	f.BoolVar(&force, "force", false, "go on although the replica lags more than [failover] max_lag_seconds")
	f.BoolVar(&dryRun, "dry-run", false, "print the preconditions and what would happen, and change nothing")
	f.BoolVar(&resume, "resume", false, "continue the move that stopped")
	projectsCmd.AddCommand(cmd)
}
