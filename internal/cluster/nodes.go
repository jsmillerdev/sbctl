package cluster

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// EnsureFounder gives the founding node its identity when it has none: a key, and a certificate
// signed by the CA derived from the master key, valid for node founderID. It also fills in the node
// row from the configuration (name, region, peer address, public host), which the migration left as
// "primary" with nothing else. It is what `supavise node token` does first; a node that has the
// files is left alone. The daemon notices the files and restarts into cluster mode.
func EnsureFounder(ctx context.Context, reg registry.Registry, ca *CA, cfg *config.Config, configPath, version string, now time.Time) (created bool, err error) {
	dir := config.ClusterDir(configPath)
	if Joined(dir) {
		return false, nil
	}
	n, err := reg.GetNode(ctx, registry.FounderNodeID)
	if err != nil {
		return false, err
	}
	key, err := NewKey()
	if err != nil {
		return false, err
	}
	issued, err := ca.Issue(key.Public().(ed25519.PublicKey), n.ID, now, NodeCertTTL)
	if err != nil {
		return false, err
	}
	n.Name = cfg.NodeName()
	n.Region = cfg.NodeRegion()
	n.PeerAddr = cfg.PeerAddr()
	n.PublicHost = firstNonEmpty(n.PublicHost, cfg.PublicIP)
	n.Version = firstNonEmpty(version, n.Version)
	if err := reg.UpdateNode(ctx, n); err != nil {
		return false, fmt.Errorf("cluster: recording this node in the registry: %w", err)
	}
	if err := reg.SetNodeCert(ctx, n.ID, issued.Serial); err != nil {
		return false, err
	}
	if err := SaveIdentity(dir, key, issued.DER, ca.DER()); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveOptions are the flags of `supavise node rm`.
type RemoveOptions struct {
	// Force deletes the node's replica rows at once instead of waiting for the replica controller to
	// remove the instances: for a node that is unreachable.
	Force bool
	// Wait bounds the wait for the replicas to go; zero is five minutes. Poll is how often the registry
	// is looked at; zero is two seconds.
	Wait, Poll time.Duration
	// Log, when set, is told what the removal is waiting for.
	Log func(format string, a ...any)
}

// RemoveNode takes a node out of the cluster, on the leader, through the registry: it refuses the
// leader and a node that is the home of a project (fail the projects over first), sets the node's
// replicas GOING_DOWN for the replica controller to remove, deletes the system replica row, waits for
// the others to be gone, and sets the node left. A left node's certificate is refused at the next
// handshake, and the node, which sees its own row in its copy of the registry, retires itself.
func RemoveNode(ctx context.Context, reg registry.Registry, id string, o RemoveOptions) error {
	if o.Wait <= 0 {
		o.Wait = 5 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	n, err := reg.GetNode(ctx, id)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("there is no node %s", id)
		}
		return err
	}
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return err
	}
	if n.State == registry.NodeLeft {
		return fmt.Errorf("node %s (%s) has been removed already", n.ID, n.Name)
	}
	if cl.Leader == n.ID {
		return fmt.Errorf("node %s is the leader; fail over to another node first (`supavise failover`)", n.ID)
	}
	ps, err := reg.ListProjects(ctx)
	if err != nil {
		return err
	}
	var homed []string
	for _, p := range ps {
		if p.NodeID == n.ID {
			homed = append(homed, p.Ref)
		}
	}
	if len(homed) > 0 {
		return fmt.Errorf("node %s is the home of %d project(s) (%s); move them with `supavise projects failover` first", n.ID, len(homed), strings.Join(homed, ", "))
	}
	rs, err := reg.ListReplicasOn(ctx, n.ID)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Origin == registry.ReplicaSystem {
			if err := reg.DeleteReplica(ctx, r.Identifier); err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
			continue
		}
		if o.Force {
			if err := reg.DeleteReplica(ctx, r.Identifier); err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
			continue
		}
		if err := reg.SetReplicaStatus(ctx, r.Identifier, string(registry.StatusGoingDown), r.InitStep, r.InitError); err != nil && !errors.Is(err, registry.ErrNotFound) {
			return err
		}
	}
	deadline := time.Now().Add(o.Wait)
	for {
		left, err := reg.ListReplicasOn(ctx, n.ID)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			ids := make([]string, len(left))
			for i, r := range left {
				ids[i] = r.Identifier
			}
			return fmt.Errorf("the replicas %s are still being removed from node %s; try again later, or use --force if the node cannot be reached", strings.Join(ids, ", "), n.ID)
		}
		logf("waiting for %d replica(s) on %s to be removed", len(left), n.ID)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
	return reg.SetNodeState(ctx, n.ID, registry.NodeLeft)
}

// DNSRecord is one record `supavise node ls --dns` says the cluster still needs.
type DNSRecord struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// DNSRecords lists the records the cluster needs: the service names point at the service address
// (the leader's, or the one the cluster row records), and each node whose public host is a name
// needs it to resolve to the node. Any server answers any project host and forwards what it does
// not host, so these are all that is required; locality records are optional.
func DNSRecords(cfg *config.Config, cl *registry.Cluster, nodes []registry.Node) []DNSRecord {
	base := cfg.BaseDomain()
	ip := cl.ServiceAddress.IP
	if ip == "" {
		for _, n := range nodes {
			if n.ID == cl.Leader {
				ip = hostOf(n.PeerAddr)
			}
		}
	}
	if ip == "" {
		ip = "<the service address>"
	}
	var out []DNSRecord
	if base != "" {
		for _, h := range []string{"api." + base, "studio." + base, "pooler." + base, "*.api." + base} {
			out = append(out, DNSRecord{Name: h, Type: recordType(ip), Value: ip, Reason: "the service address: the leader's; after a failover it moves to the survivor"})
		}
	}
	sorted := append([]registry.Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, n := range sorted {
		if n.State == registry.NodeLeft || n.PublicHost == "" || net.ParseIP(n.PublicHost) != nil {
			continue
		}
		v := hostOf(n.PeerAddr)
		if v == "" {
			v = "<the address of " + n.ID + ">"
		}
		out = append(out, DNSRecord{Name: n.PublicHost, Type: recordType(v), Value: v, Reason: "the pooler endpoints of replicas on node " + n.ID})
	}
	return out
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return h
}

func recordType(v string) string {
	if ip := net.ParseIP(v); ip != nil && ip.To4() == nil {
		return "AAAA"
	}
	return "A"
}
