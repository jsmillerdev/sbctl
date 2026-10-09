package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

// failoverClient is the CLI's side of the daemon's failover control socket.
// failover.Client implements it; the tests replace it.
type failoverClient interface {
	Readiness(ctx context.Context) (failover.Readiness, error)
	PlanProject(ctx context.Context, o failover.ProjectOptions) (*failover.Plan, error)
	PlanServer(ctx context.Context, o failover.ServerOptions) (*failover.Plan, error)
	RunProject(ctx context.Context, o failover.ProjectOptions, onStep func(registry.MoveStep)) (*registry.Move, error)
	RunServer(ctx context.Context, o failover.ServerOptions, onStep func(registry.MoveStep)) (*registry.Move, error)
	Follow(ctx context.Context, epoch int64, onStep func(registry.MoveStep)) (*registry.Move, error)
}

// newFailoverClient reaches the daemon of this node. A variable so that tests can use a fake.
var newFailoverClient = func(cfg *config.Config) failoverClient {
	return failover.Client{Path: failover.ControlSocket(cfg)}
}

func init() {
	var o failover.ServerOptions
	cmd := &cobra.Command{
		Use:   "failover",
		Short: "Move the whole server's work to another node",
		Long: `Makes another node the leader. With the old leader alive this is a planned switchover and
nothing is lost: the old leader stops cleanly, the survivor takes over, and the old leader follows. With
the old leader dead it is a failover: the survivor fences the old leader first, takes the service address,
promotes the system cluster and then every project's replica, and loses at most the replication lag.

Run it on the node that should lead; a switchover can also be started on the leader with --to,
which has that node run it. --dry-run prints each precondition. State is kept so that --resume
continues a run that stopped, and --abort discards one that stopped before the leader marker was
written (it starts the old leader again if the switchover had stopped it); a run that went further
only continues. A project with no replica is refused unless --restore-missing, which builds
its standby from the WAL archive (data loss up to archive_timeout).

The daemon of the node that takes over restarts once when its system cluster is promoted, because that
makes it the leader. The move is not over then: the daemon that starts continues it, and this command
waits for it and goes on printing the steps. A node that stops leading restarts its daemon the same way
when its own system cluster becomes a standby.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runServerFailover(cmd, o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.To, "to", "", "the node to promote (default: this one)")
	f.BoolVar(&o.Force, "force", false, "go on although a precondition that is not marked hard fails: a replica that lags or is not healthy, nodes on different releases, an unreachable epoch-marker store")
	f.BoolVar(&o.DryRun, "dry-run", false, "print the preconditions and what would happen, and change nothing")
	f.BoolVar(&o.Resume, "resume", false, "continue the run that stopped")
	f.BoolVar(&o.Abort, "abort", false, "discard the run that stopped before the leader marker was written, and start the old leader again if it was stopped for a switchover")
	f.BoolVar(&o.RestoreMissing, "restore-missing", false, "seed a project that has no replica from the archive")
	f.BoolVar(&o.OldPrimaryIsDown, "old-primary-is-down", false, "assert that the old leader is down, when no fence is configured")
	f.BoolVarP(&o.Yes, "yes", "y", false, "do not ask for confirmation")
	rootCmd.AddCommand(cmd)
}

// runServerFailover plans the move, prints the plan, asks, and follows the run.
func runServerFailover(cmd *cobra.Command, o failover.ServerOptions) error {
	ctx, c, out, err := openFailover(cmd)
	if err != nil {
		return err
	}
	if o.Abort {
		return abortServerFailover(ctx, out, c, o)
	}
	plan, err := c.PlanServer(ctx, o)
	if err != nil {
		return planError(err)
	}
	if run, err := reviewMovePlan(out, plan, o.Force, o.Resume, o.DryRun); !run {
		return err
	}
	if !o.Yes {
		if err := confirmTyped(cmd, serverQuestion(plan), "the name of the new leader", plan.To, orID(plan.ToName, plan.To)); err != nil {
			return err
		}
	}
	fmt.Fprintln(out)
	if !o.Resume { // the daemon refuses to run a plan other than the one that was shown
		o.ExpectKind, o.ExpectEpoch = plan.Kind, plan.Epoch
	}
	show := printOnce(stepPrinter(out))
	mv, err := c.RunServer(ctx, o, show)
	if errors.Is(err, failover.ErrRestarting) || errors.Is(err, failover.ErrStreamClosed) {
		mv, err = followRestart(ctx, out, c, plan.Epoch, mv, show)
	}
	return finishMove(out, mv, err)
}

// followRestart goes on with a server move whose connection the daemon's restart cut: the daemon that
// starts continues the move (the node that leads now) or has the log from the leader (a node that
// follows now), and the steps it records are printed from where they stopped: the daemon sends the log
// from its start, and what the lost run printed is not printed again (show).
func followRestart(ctx context.Context, out io.Writer, c failoverClient, epoch int64, mv *registry.Move, show func(registry.MoveStep)) (*registry.Move, error) {
	fmt.Fprintln(out, "\nThe daemon of this node restarts in the role the move gives it. Waiting for it, and following the move from there.")
	next, err := c.Follow(ctx, epoch, show)
	switch {
	case errors.Is(err, failover.ErrNothingRunning):
		return mv, errors.New("the daemon restarted and has no record of this move. The move goes on where the cluster leads: supavise status shows the leader and the cluster, and supavise failover --resume on the node it goes to continues a move that stopped")
	case next == nil && err != nil:
		return mv, err
	}
	return next, err
}

// abortServerFailover discards the run that stopped early. It plans nothing: there is no move to
// plan, only the unfinished one to take back.
func abortServerFailover(ctx context.Context, out io.Writer, c failoverClient, o failover.ServerOptions) error {
	if o.Resume || o.DryRun || o.Force || o.RestoreMissing || o.OldPrimaryIsDown || o.To != "" {
		return errors.New("--abort discards the unfinished run and takes no other option")
	}
	mv, err := c.RunServer(ctx, o, stepPrinter(out))
	if err != nil {
		return err
	}
	if mv != nil {
		fmt.Fprintf(out, "\nAborted: the %s of the server from %s to %s was discarded after %s; nothing was promoted.\n", mv.Kind, mv.FromNode, mv.ToNode, lastStepName(mv))
	}
	return nil
}

// openFailover loads the config and reaches the daemon of this node for a failover command. It
// returns the command's context and its output.
func openFailover(cmd *cobra.Command) (context.Context, failoverClient, io.Writer, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, newFailoverClient(cfg), cmd.OutOrStdout(), nil
}

// reviewMovePlan prints the plan, then ends the command where a plan ends before any step runs: a
// dry run is done (an error when the plan would be refused), and a refused plan is an error. run
// reports whether the move should go on.
func reviewMovePlan(out io.Writer, plan *failover.Plan, force, resume, dryRun bool) (run bool, err error) {
	printMovePlan(out, plan, force || resume)
	refused := plan.Refused(force || resume)
	if dryRun {
		if len(refused) > 0 {
			return false, fmt.Errorf("the move would be refused: %d precondition(s) fail", len(refused))
		}
		fmt.Fprintln(out, "\nDry run: nothing was changed.")
		return false, nil
	}
	if len(refused) > 0 {
		return false, &failover.RefusedError{Checks: refused, Force: force}
	}
	return true, nil
}

// planError words an error of the plan request.
func planError(err error) error {
	if errors.Is(err, failover.ErrNoCluster) {
		return errors.New("this server is not part of a cluster: there is nothing to fail over to")
	}
	return err
}

func serverQuestion(p *failover.Plan) string {
	from, to := orID(p.FromName, p.From), orID(p.ToName, p.To)
	if p.Kind == string(registry.MoveSwitchover) {
		return fmt.Sprintf("This stops %s cleanly and makes %s the leader (epoch %d).", from, to, p.Epoch)
	}
	return fmt.Sprintf("This fences %s and makes %s the leader (epoch %d); what %s wrote and %s did not receive is lost.", from, to, p.Epoch, from, to)
}

// confirmTyped asks the operator to type what names the thing the move acts on (what: "the name of
// the new leader"), and refuses to guess when stdin is not a terminal.
func confirmTyped(cmd *cobra.Command, question, what, id, name string) error {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return errors.New("nothing was changed; run it again with --yes to go on without being asked")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\n%s\nType %s (%s) to go on: ", question, what, name)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.TrimSpace(line); a != name && a != id {
		return errors.New("nothing was changed")
	}
	return nil
}

// printMovePlan writes the plan the way --dry-run shows it: the header, every precondition with its
// verdict, the projects, and the notes.
func printMovePlan(w io.Writer, p *failover.Plan, forced bool) {
	what := "Server"
	if p.Ref != "" {
		what = "Project " + p.Ref
	}
	kind := p.Kind
	if kind == "" {
		kind = "move"
	}
	head := fmt.Sprintf("%s %s", what, kind)
	if p.From != "" || p.To != "" {
		head += fmt.Sprintf(": %s -> %s", orDash(orID(p.FromName, p.From)), orDash(orID(p.ToName, p.To)))
	}
	if p.Epoch > 0 {
		head += fmt.Sprintf(" (epoch %d)", p.Epoch)
	}
	fmt.Fprintln(w, head)
	fmt.Fprintln(w, "\nPreconditions")
	t := newTable(w)
	for _, c := range p.Checks {
		verdict := "ok"
		switch {
		case !c.OK && c.Blocking && (c.Hard || !forced):
			verdict = "FAIL"
		case !c.OK && c.Blocking:
			verdict = "FAIL (forced)"
		case !c.OK:
			verdict = "note"
		}
		fmt.Fprintf(t, "  %s\t%s\t%s\n", verdict, c.Name, c.Detail)
	}
	t.Flush()
	if len(p.Projects) > 0 {
		fmt.Fprintln(w, "\nProjects")
		t := newTable(w)
		for _, pr := range p.Projects {
			switch {
			case pr.RestoreFromArchive:
				fmt.Fprintf(t, "  %s\t%s\tno replica: restored from the archive\n", pr.Ref, orDash(pr.Node))
			default:
				lag := "lag unknown"
				if pr.LagSeconds != nil {
					lag = fmt.Sprintf("lag %.1fs", *pr.LagSeconds)
				}
				fmt.Fprintf(t, "  %s\t%s\t%s, %s\n", pr.Ref, orDash(pr.Node), pr.Replica, lag)
			}
		}
		t.Flush()
	}
	for _, n := range p.Notes {
		fmt.Fprintf(w, "\n%s\n", n)
	}
}

// orID is a node's name, or its id when the plan has no name.
func orID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// stepPrinter prints each step of a run as the daemon records it.
func stepPrinter(w io.Writer) func(registry.MoveStep) {
	return func(s registry.MoveStep) {
		detail := s.Detail
		if s.Name == "begin" || len(detail) > 300 {
			detail = ""
		}
		line := fmt.Sprintf("%s  %s", s.At.Local().Format(time.TimeOnly), s.Name)
		if detail != "" {
			line += ": " + detail
		}
		fmt.Fprintln(w, line)
	}
}

// printOnce passes on the first step of each name. The steps of one move have names of their own, and
// a step the stream printed comes again from the log when the CLI follows the move.
func printOnce(show func(registry.MoveStep)) func(registry.MoveStep) {
	seen := map[string]bool{}
	return func(s registry.MoveStep) {
		if !seen[s.Name] {
			seen[s.Name] = true
			show(s)
		}
	}
}

// finishMove prints the end of a run and returns its error.
func finishMove(w io.Writer, mv *registry.Move, err error) error {
	if err != nil {
		var re *failover.RemoteError
		if errors.As(err, &re) && len(re.Checks) > 0 {
			err = &failover.RefusedError{Checks: re.Checks}
		}
		if mv != nil && mv.ID != 0 {
			fmt.Fprintf(w, "\nThe move (id %d) is %s after %s. ", mv.ID, mv.State, lastStepName(mv))
		}
		return err
	}
	if mv != nil {
		fmt.Fprintf(w, "\nDone: the %s of %s from %s to %s is finished (move %d).\n", mv.Kind, scopeText(mv), mv.FromNode, mv.ToNode, mv.ID)
	}
	return nil
}

func scopeText(mv *registry.Move) string {
	if mv.Scope == registry.MoveProject {
		return "project " + mv.Ref
	}
	return "the server"
}

func lastStepName(mv *registry.Move) string {
	if len(mv.Steps) == 0 {
		return "the start"
	}
	return mv.Steps[len(mv.Steps)-1].Name
}

// failoverStatus is the failover-readiness block of `supavise status`; nil means there is none to
// show (a node that is not part of a cluster, or whose daemon does not run).
func failoverStatus(ctx context.Context, cfg *config.Config) (*failover.Readiness, error) {
	if _, err := os.Stat(failover.ControlSocket(cfg)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := newFailoverClient(cfg).Readiness(ctx)
	switch {
	case errors.Is(err, failover.ErrNoCluster):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &r, nil
}
