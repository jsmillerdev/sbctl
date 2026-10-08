package main

import (
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/notimpl"
)

func init() {
	replicasCmd := &cobra.Command{
		Use:   "replicas",
		Short: "List, add and remove read replicas",
		Long: `A read replica is a standby copy of one project's database, with its own PostgREST, on
another server of the cluster. These commands do what Studio's "Add read replica" does, and a script can
run them. [replicas] default = "all" makes every project get a replica on every other node.`,
	}

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [ref]",
		Short: "List replicas, with their setup step and lag",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(*cobra.Command, []string) error { return notimpl.For("supavise replicas ls") },
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var addRegion, addNode string
	add := &cobra.Command{
		Use:   "add <ref> --region <region>",
		Short: "Add a read replica of a project",
		Long: `Adds a replica of the project in the region, on the least loaded node there, and returns
when the request is accepted; the setup then runs in the background (see "supavise replicas ls").
--node names the node instead. Refused when no node is joined in the region, when the region is the
project's own server, and when the project has the most replicas its size allows.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise replicas add") },
	}
	add.Flags().StringVar(&addRegion, "region", "", "the region of the replica")
	add.Flags().StringVar(&addNode, "node", "", "the node to put the replica on (default: the least loaded in the region)")
	_ = add.MarkFlagRequired("region")

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <identifier>",
		Short: "Remove a read replica",
		Args:  cobra.ExactArgs(1),
		RunE:  func(*cobra.Command, []string) error { return notimpl.For("supavise replicas rm") },
	}
	rm.Flags().BoolVarP(&rmYes, "yes", "y", false, "do not ask for confirmation")

	replicasCmd.AddCommand(ls, add, rm)
	rootCmd.AddCommand(replicasCmd)
}
