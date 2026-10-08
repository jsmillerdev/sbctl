package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// replicasEnv is what the `supavise replicas` commands work with: the replica service over the
// registry, the registry for the names of nodes, and where the daemon leaves its lag numbers.
// The commands write the rows; the daemon on the leader runs the setups (level triggered, from
// the registry), so they work whether or not it is running.
type replicasEnv struct {
	svc      replicas.Service
	reg      registry.Registry
	snapshot string
	close    func()
}

// openReplicas is a variable so that the tests can open a registry in memory.
var openReplicas = func(ctx context.Context) (*replicasEnv, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &replicasEnv{
		svc:      replicas.New(replicas.Options{Registry: reg, Config: cfg, Log: newLogger(cfg)}),
		reg:      reg,
		snapshot: filepath.Join(cfg.StateDir, replicas.SnapshotName),
		close:    reg.Close,
	}, nil
}

// replicaRow is one line of `replicas ls`, and its JSON.
type replicaRow struct {
	Identifier string   `json:"identifier"`
	Ref        string   `json:"ref"`
	Node       string   `json:"node"`
	NodeName   string   `json:"node_name"`
	Region     string   `json:"region"`
	Origin     string   `json:"origin"`
	Status     string   `json:"status"`
	Step       string   `json:"step"`
	Error      string   `json:"error,omitempty"`
	Receiver   string   `json:"receiver,omitempty"`
	LagSeconds *float64 `json:"lag_seconds"`
}

func init() {
	replicasCmd := &cobra.Command{
		Use:   "replicas",
		Short: "List, add and remove read replicas",
		Long: `A read replica is a standby copy of one project's database, with its own PostgREST, on
another server of the cluster. These commands do what Studio's "Add read replica" does, and a script can
run them. [replicas] default = "all" makes every project get a replica on every other node.

The commands read and write the registry; the daemon on the leader does the work, so run them
on the leader.`,
	}

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [ref]",
		Short: "List replicas, with their setup step and lag",
		Long: `Lists the replicas of the project, or of every project, with the node each one runs on, its
status and the setup step it has reached. Lag comes from the daemon, which refreshes it every 10
seconds; it shows "-" when the daemon is not running or the replica has not reported yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			re, err := openReplicas(cmd.Context())
			if err != nil {
				return err
			}
			defer re.close()
			rows, err := replicaRows(cmd.Context(), re, ref)
			if err != nil {
				return err
			}
			if lsJSON {
				return printJSON(cmd.OutOrStdout(), rows)
			}
			printReplicas(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var addRegion, addNode string
	add := &cobra.Command{
		Use:   "add <ref> --region <region>",
		Short: "Add a read replica of a project",
		Long: `Adds a replica of the project in the region, on the least loaded node there, and returns
when the request is accepted; the setup then runs in the background (see "supavise replicas ls").
--node names the node instead (its id or its name). Refused when no node is joined in the region, when the
region is the project's own server, and when the project has the most replicas its size allows.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			re, err := openReplicas(cmd.Context())
			if err != nil {
				return err
			}
			defer re.close()
			return addReplica(cmd.Context(), cmd.OutOrStdout(), re, args[0], addRegion, addNode)
		},
	}
	add.Flags().StringVar(&addRegion, "region", "", "the region of the replica")
	add.Flags().StringVar(&addNode, "node", "", "the node to put the replica on (default: the least loaded in the region)")
	_ = add.MarkFlagRequired("region")

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <identifier>",
		Short: "Remove a read replica",
		Long: `Removes the replica: its units stop, its pooler tenant and its data go, and its row is deleted.
The daemon does it in the background; the replica shows GOING_DOWN until it is done. Removing a replica
that [replicas] default = "all" made keeps the default from making it again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, _, _, ok := registry.ParseReplicaIdentifier(args[0])
			if !ok {
				return fmt.Errorf("%q is not a replica identifier (see `supavise replicas ls`)", args[0])
			}
			if !rmYes {
				if err := confirmRemoval(cmd, fmt.Sprintf("Remove read replica %s?", args[0])); err != nil {
					return err
				}
			}
			re, err := openReplicas(cmd.Context())
			if err != nil {
				return err
			}
			defer re.close()
			if err := re.svc.Remove(cmd.Context(), ref, args[0]); err != nil {
				if errors.Is(err, replicas.ErrNotFound) {
					return fmt.Errorf("no read replica %s (see `supavise replicas ls`)", args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removing %s; the daemon finishes it in the background\n", args[0])
			return nil
		},
	}
	rm.Flags().BoolVarP(&rmYes, "yes", "y", false, "do not ask for confirmation")

	replicasCmd.AddCommand(ls, add, rm)
	rootCmd.AddCommand(replicasCmd)
}

// confirmRemoval asks on a terminal, and refuses to guess when stdin is not one.
func confirmRemoval(cmd *cobra.Command, question string) error {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return errors.New("nothing was changed; run it again with --yes to remove without being asked")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("nothing was changed")
	}
	return nil
}

// addReplica asks for a replica of ref in region, on node when one is named, and prints the
// identifier of the row it made.
func addReplica(ctx context.Context, w io.Writer, re *replicasEnv, ref, region, node string) error {
	before, err := re.svc.List(ctx, ref)
	if err != nil {
		return err
	}
	if node == "" {
		err = re.svc.Setup(ctx, ref, region)
	} else {
		nodes, lerr := re.reg.ListNodes(ctx)
		if lerr != nil {
			return lerr
		}
		i := slices.IndexFunc(nodes, func(n registry.Node) bool { return n.ID == node || n.Name == node })
		if i >= 0 && nodes[i].Region != region {
			return fmt.Errorf("node %s is in %s, not in %s", node, nodes[i].Region, region)
		}
		err = re.svc.SetupOn(ctx, ref, node)
	}
	if err != nil {
		return err
	}
	after, err := re.svc.List(ctx, ref)
	if err != nil {
		return err
	}
	for _, r := range after {
		if !slices.ContainsFunc(before, func(b replicas.Replica) bool { return b.Identifier == r.Identifier }) {
			fmt.Fprintf(w, "requested %s on node %s; the setup runs in the background (supavise replicas ls %s)\n", r.Identifier, r.NodeID, ref)
			return nil
		}
	}
	fmt.Fprintf(w, "requested a read replica of %s; the setup runs in the background (supavise replicas ls %s)\n", ref, ref)
	return nil
}

// replicaRows lists the replicas of ref (every project when empty) with their node names and the lag
// the daemon last wrote.
func replicaRows(ctx context.Context, re *replicasEnv, ref string) ([]replicaRow, error) {
	rs, err := re.svc.List(ctx, ref)
	if err != nil {
		return nil, err
	}
	nodes, err := re.reg.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(nodes))
	for _, n := range nodes {
		names[n.ID] = n.Name
	}
	snap := replicas.ReadSnapshot(re.snapshot, time.Now())
	out := make([]replicaRow, 0, len(rs))
	for _, r := range rs {
		row := replicaRow{Identifier: r.Identifier, Ref: r.Ref, Node: r.NodeID, NodeName: names[r.NodeID], Region: r.Region,
			Origin: r.Origin, Status: r.Status, Step: r.InitStep, Error: r.InitError}
		if e, ok := snap.Entry(r.Identifier); ok {
			row.Receiver, row.LagSeconds = e.Receiver, e.LagSeconds
		}
		out = append(out, row)
	}
	return out, nil
}

func printReplicas(w io.Writer, rows []replicaRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no read replicas")
		return
	}
	t := newTable(w)
	fmt.Fprintln(t, "IDENTIFIER\tPROJECT\tNODE\tREGION\tORIGIN\tSTATUS\tSTEP\tLAG")
	for _, r := range rows {
		lag := "-"
		if r.LagSeconds != nil {
			lag = fmt.Sprintf("%.0fs", *r.LagSeconds)
		}
		node := r.Node
		if r.NodeName != "" {
			node += " (" + r.NodeName + ")"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Identifier, r.Ref, node, r.Region, r.Origin, r.Status, stepText(r), lag)
	}
	_ = t.Flush()
}

// stepText is the step column: the step reached, "done" once complete, and the failure after it.
func stepText(r replicaRow) string {
	switch {
	case r.Error != "":
		return r.Step + " (" + r.Error + ")"
	case r.Step == replicas.StepDone:
		return "done"
	}
	return r.Step
}
