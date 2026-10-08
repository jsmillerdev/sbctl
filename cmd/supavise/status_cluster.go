package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// clusterBlock is the cluster section of `supavise status`: the nodes, the epoch, the leader and
// each replica's lag.
type clusterBlock struct {
	Name   string `json:"name,omitempty"`
	Epoch  int64  `json:"epoch"`
	Leader string `json:"leader,omitempty"`
	// Node is this node's id and Role what it does now.
	Node string `json:"node,omitempty"`
	Role string `json:"role,omitempty"`
	// Fenced is set on a node that was replaced as leader and starts no primary.
	Fenced      *cluster.FencedRecord `json:"fenced,omitempty"`
	Maintenance *registry.Maintenance `json:"maintenance,omitempty"`
	Nodes       []nodeRow             `json:"nodes,omitempty"`
	Replicas    []replicaRow          `json:"replicas,omitempty"`
	// Live says whether the daemon's view (sessions, lag) is current; false when its status file is
	// missing or stale, in which case sessions and lag are not shown.
	Live bool `json:"live"`
}

// replicaRow is a replica in the cluster block.
type replicaRow struct {
	Identifier string   `json:"identifier"`
	Ref        string   `json:"ref"`
	Node       string   `json:"node"`
	Status     string   `json:"status"`
	Step       string   `json:"step,omitempty"`
	Receiver   string   `json:"receiver,omitempty"`
	LagSeconds *float64 `json:"lag_seconds,omitempty"`
}

func (b *clusterBlock) render(w io.Writer) {
	const label, indent = "cluster   ", "          "
	switch {
	case b.Fenced != nil:
		fmt.Fprintf(w, "%sFENCED: %s\n", label, b.Fenced.Reason)
		fmt.Fprintf(w, "%sThis node starts no primary and answers 503. Fix: sudo supavise node rejoin\n", indent)
	default:
		head := fmt.Sprintf("%sepoch %d, leader %s", label, b.Epoch, orDash(b.Leader))
		if b.Name != "" {
			head = fmt.Sprintf("%s%s: epoch %d, leader %s", label, b.Name, b.Epoch, orDash(b.Leader))
		}
		if b.Node != "" {
			head += fmt.Sprintf(" (this node: %s, %s)", b.Node, b.Role)
		}
		fmt.Fprintln(w, head)
	}
	if m := b.Maintenance; m != nil {
		fmt.Fprintf(w, "%smaintenance on %s until %s: %s\n", indent, m.Node, m.Until.Format(time.RFC3339), m.Reason)
	}
	for _, n := range b.Nodes {
		line := fmt.Sprintf("%s%-4s %-14s %-9s %-8s %-12s %s", indent, n.ID, n.Name, n.Role, n.State, orDash(n.Region), orDash(n.Version))
		switch {
		case n.Self:
		case n.Connected != nil && *n.Connected:
			line += fmt.Sprintf("  session up, %.0f ms", n.RTTMillis)
		case n.Connected != nil:
			line += "  session DOWN"
		}
		fmt.Fprintln(w, line)
	}
	for _, r := range b.Replicas {
		line := fmt.Sprintf("%sreplica %s on %s: %s", indent, r.Identifier, r.Node, r.Status)
		if r.Step != "" && r.Status != string(registry.StatusActiveHealthy) {
			line += " (" + r.Step + ")"
		}
		if r.LagSeconds != nil {
			line += fmt.Sprintf(", lag %.1f s", *r.LagSeconds)
		}
		fmt.Fprintln(w, line)
	}
	if !b.Live && b.Fenced == nil {
		fmt.Fprintf(w, "%s(the daemon's live view is not current: sessions and lag are not shown)\n", indent)
	}
}

// clusterStatus reads the cluster block; nil means there is none to show (a node that is not
// part of a cluster).
func clusterStatus(ctx context.Context, cfg *config.Config) (*clusterBlock, error) {
	rec, err := cluster.ReadFenced(cfg)
	if err != nil {
		return nil, err
	}
	if rec == nil && !cluster.Joined(config.ClusterDir(config.ResolvePath(configPath))) {
		return nil, nil
	}
	st, _ := cluster.ReadStatus(cfg)
	live := st != nil && time.Since(st.At) < cluster.StatusStale
	if rec != nil {
		// A fenced node's database is stopped: the record and the last status are what there is.
		b := &clusterBlock{Fenced: rec, Epoch: rec.Epoch, Leader: rec.Leader}
		if st != nil {
			b.Node, b.Role = st.Node, string(st.Role)
		}
		return b, nil
	}
	reg, err := openRegistryReadOnly(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	env := &nodeEnv{cfg: cfg, reg: reg, now: time.Now}
	if live {
		env.status = st
	}
	rows, cl, err := nodeRows(ctx, env)
	if err != nil {
		return nil, err
	}
	b := &clusterBlock{Name: cl.Name, Epoch: cl.Epoch, Leader: cl.Leader, Nodes: rows, Live: live}
	if cl.Maintenance.Active(time.Now()) {
		m := cl.Maintenance
		b.Maintenance = &m
	}
	if live {
		b.Node, b.Role = st.Node, string(st.Role)
	}
	reps, err := reg.ListReplicas(ctx, "")
	if err != nil {
		return nil, err
	}
	lag := map[string]cluster.ReplicaStatus{}
	if live {
		for _, r := range st.Replicas {
			lag[r.Identifier] = r
		}
	}
	for _, r := range reps {
		if r.Ref == config.SystemRef { // the system cluster's standby is part of the node, not a replica of a project
			continue
		}
		row := replicaRow{Identifier: r.Identifier, Ref: r.Ref, Node: r.NodeID, Status: r.Status, Step: r.InitStep}
		if o, ok := lag[r.Identifier]; ok {
			row.Receiver, row.LagSeconds = o.ReceiverStatus, o.LagSeconds
		}
		b.Replicas = append(b.Replicas, row)
	}
	return b, nil
}
