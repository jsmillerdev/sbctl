package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/failover"
)

func init() {
	var o failover.ProjectOptions
	cmd := &cobra.Command{
		Use:   "failover <ref>",
		Short: "Move one project's primary to the node that holds its replica",
		Long: `Promotes the project's replica and makes the old primary a replica in its place, so the
project's endpoints, pooler and Realtime reconnect with no change of address. With the old primary alive
nothing is lost. --dry-run prints each precondition; --resume continues a move that stopped.

Run it on the leader. A project whose primary does not answer is failed over instead: the old primary
is fenced first, and what the replica has not received is lost. A project whose home node does not
answer at all is a node failure: run supavise failover on a surviving node.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Ref = args[0]
			return runProjectFailover(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.To, "to", "", "the node to move the project to (default: the healthiest replica)")
	f.BoolVar(&o.Force, "force", false, "go on although the replica lags more than [failover] max_lag_seconds")
	f.BoolVar(&o.DryRun, "dry-run", false, "print the preconditions and what would happen, and change nothing")
	f.BoolVar(&o.Resume, "resume", false, "continue the move that stopped")
	projectsCmd.AddCommand(cmd)
}

// runProjectFailover plans the move, prints the plan and follows the run. A project move stops
// one project for a few seconds and needs no typed confirmation.
func runProjectFailover(cmd *cobra.Command, o failover.ProjectOptions) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	c := newFailoverClient(cfg)
	out := cmd.OutOrStdout()
	plan, err := c.PlanProject(ctx, o)
	if err != nil {
		return planError(err)
	}
	printMovePlan(out, plan, o.Force || o.Resume)
	refused := plan.Refused(o.Force || o.Resume)
	if o.DryRun {
		if len(refused) > 0 {
			return fmt.Errorf("the move would be refused: %d precondition(s) fail", len(refused))
		}
		fmt.Fprintln(out, "\nDry run: nothing was changed.")
		return nil
	}
	if len(refused) > 0 {
		return &failover.RefusedError{Checks: refused, Force: o.Force}
	}
	fmt.Fprintln(out)
	mv, err := c.RunProject(ctx, o, stepPrinter(out))
	return finishMove(out, mv, err)
}
