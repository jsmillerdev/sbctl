package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
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
is fenced first, and what the replica has not received is lost, so the command asks for the project's
ref before it goes on (--yes skips that). A project whose home node does not
answer at all is a node failure: if that node is the leader, run supavise failover on a surviving node.
No move covers the projects of a follower that is down.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Ref = args[0]
			return runProjectFailover(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.To, "to", "", "the node to move the project to (default: the healthiest replica)")
	f.BoolVar(&o.Force, "force", false, "go on although a precondition that is not marked hard fails: a replica that lags or is not healthy, nodes on different releases")
	f.BoolVar(&o.DryRun, "dry-run", false, "print the preconditions and what would happen, and change nothing")
	f.BoolVar(&o.Resume, "resume", false, "continue the move that stopped")
	f.BoolVarP(&o.Yes, "yes", "y", false, "do not ask for confirmation of a failover (a switchover is never asked)")
	projectsCmd.AddCommand(cmd)
}

// runProjectFailover plans the move, prints the plan and follows the run. A switchover stops one
// project for a few seconds and needs no typed confirmation. A failover fences the old primary,
// sets its data aside and loses what the replica has not received, so it asks for the project's ref
// (--yes skips it), like the server failover does for the new leader's name.
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
	if plan.Kind == string(registry.MoveFailover) && !o.Resume && !o.Yes {
		if err := confirmTyped(cmd, projectQuestion(plan), "the project ref", plan.Ref, plan.Ref); err != nil {
			return err
		}
	}
	fmt.Fprintln(out)
	if !o.Resume { // the daemon refuses to run a plan other than the one that was shown
		o.ExpectKind = plan.Kind
	}
	mv, err := c.RunProject(ctx, o, stepPrinter(out))
	return finishMove(out, mv, err)
}

// projectQuestion is the warning before a project failover: the old primary is fenced and its
// data set aside, so what it wrote and the replica did not receive is lost.
func projectQuestion(p *failover.Plan) string {
	from, to := orID(p.FromName, p.From), orID(p.ToName, p.To)
	return fmt.Sprintf("The primary of %s on %s does not answer. This fences it, sets its data aside and promotes the replica on %s; what the primary wrote and the replica did not receive is lost.", p.Ref, from, to)
}
