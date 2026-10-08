package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/lifecycle"
)

var (
	rzSize string
	rzJSON bool
)

func init() {
	resize := projectCmd("resize <ref>", "Change a project's compute size and wait until it is healthy on it", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			if rzSize == "" {
				return fmt.Errorf("--size is required (%v)", lifecycle.ClassNames())
			}
			before, err := n.Registry.GetProject(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			p, err := n.Engine.Resize(cmd.Context(), a[0], rzSize)
			if err != nil {
				return err
			}
			v := viewOf(n.Cfg, p)
			if rzJSON {
				return printJSON(cmd.OutOrStdout(), v)
			}
			from, _ := lifecycle.ClassFor(before.Class)
			to, _ := lifecycle.ClassFor(p.Class)
			if from.Name == to.Name {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is already %s\n", p.Ref, to.Title)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s resized from %s to %s (%s memory, %d connections), status %s\n",
				p.Ref, from.Title, to.Title, memoryText(to), to.MaxConnections, p.Status)
			return nil
		})
	resize.Flags().StringVar(&rzSize, "size", "", fmt.Sprintf("the compute size %v", lifecycle.ClassNames()))
	resize.Flags().BoolVar(&rzJSON, "json", false, "print JSON")

	sizes := projectCmd("sizes [ref]", "List the compute sizes and which of them this node can give now", cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			ref := ""
			if len(a) == 1 {
				ref = a[0]
			}
			offers, err := n.Engine.Offers(cmd.Context(), ref)
			if err != nil {
				return err
			}
			cp, known, err := n.Engine.Capacity(cmd.Context(), "")
			if err != nil {
				return err
			}
			if rzJSON {
				return printJSON(cmd.OutOrStdout(), sizesJSON(offers))
			}
			printSizes(cmd.OutOrStdout(), offers, cp, known, ref)
			return nil
		})
	sizes.Flags().BoolVar(&rzJSON, "json", false, "print JSON")

	projectsCmd.AddCommand(resize, sizes)
}

func memoryText(c lifecycle.Class) string {
	if c.MemoryBytes < 1<<30 {
		return strconv.FormatInt(c.MemoryBytes>>20, 10) + " MB"
	}
	return strconv.FormatInt(c.MemoryBytes>>30, 10) + " GB"
}

func cpuText(c lifecycle.Class) string {
	kind := "dedicated"
	if c.Shared {
		kind = "shared"
	}
	return fmt.Sprintf("%d vCPU %s", c.VCPUs(), kind)
}

type sizeView struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	VCPUs          int    `json:"vcpus"`
	SharedCPU      bool   `json:"shared_cpu"`
	MemoryBytes    int64  `json:"memory_bytes"`
	MaxConnections int    `json:"max_connections"`
	PoolerClients  int    `json:"pooler_max_clients"`
	PoolSize       int    `json:"pooler_pool_size"`
	Current        bool   `json:"current"`
	Available      bool   `json:"available"`
	Reason         string `json:"reason,omitempty"`
}

func sizesJSON(offers []lifecycle.Offer) []sizeView {
	out := make([]sizeView, 0, len(offers))
	for _, o := range offers {
		c := o.Class
		out = append(out, sizeView{c.Name, c.Title, c.VCPUs(), c.Shared, c.MemoryBytes, c.MaxConnections, c.PoolerMaxClients, c.PoolSize, o.Current, o.Fits, o.Reason})
	}
	return out
}

// printSizes prints the size table, then the node's room and the reason for every size that does
// not fit. ref is the project the answer is for ("": a new one).
func printSizes(w io.Writer, offers []lifecycle.Offer, cp lifecycle.Capacity, known bool, ref string) {
	t := newTable(w)
	fmt.Fprintln(t, "SIZE\tCPU\tMEMORY\tCONNECTIONS\tPOOLER CLIENTS\tAVAILABLE")
	for _, o := range offers {
		c := o.Class
		avail := "yes"
		switch {
		case o.Current:
			avail = "current"
		case !o.Fits:
			avail = "no"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%d\t%s\n", c.Name, cpuText(c), memoryText(c), c.MaxConnections, c.PoolerMaxClients, avail)
	}
	_ = t.Flush()
	if !known {
		fmt.Fprintln(w, "\nThe node's resources are unknown here, so every size is shown as available.")
		return
	}
	free := cp.FreeBytes()
	if cp.BudgetBytes == 0 {
		fmt.Fprintf(w, "\nnode: %d cores, memory unknown; %d projects\n", cp.Node.CPUs, cp.Projects)
	} else {
		fmt.Fprintf(w, "\nnode: %s memory x %g overcommit = %s of project memory caps, %s promised to %d projects, %s free; %d cores\n",
			bytesText(cp.Node.MemoryBytes), cp.Overcommit, bytesText(cp.BudgetBytes), bytesText(cp.CommittedBytes), cp.Projects, bytesText(free), cp.Node.CPUs)
	}
	for _, o := range offers {
		if !o.Fits {
			fmt.Fprintf(w, "  %s: %s\n", o.Class.Name, o.Reason)
		}
	}
	if ref != "" {
		fmt.Fprintf(w, "(for project %s: its own cap is left out of the sum)\n", ref)
	}
}

func bytesText(b int64) string {
	if b >= 1<<30 {
		return strconv.FormatFloat(float64(b)/float64(1<<30), 'f', 1, 64) + " GB"
	}
	return strconv.FormatInt(b>>20, 10) + " MB"
}
