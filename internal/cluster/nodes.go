package cluster

import (
	"cmp"
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
	n.PublicHost = cmp.Or(n.PublicHost, cfg.PublicIP)
	n.Version = cmp.Or(version, n.Version)
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

// ReplicaRemover removes the replicas on a node: *replicas.Controller implements it (Remover.RemoveOn).
// The code that runs inside the daemon passes it; `supavise node rm`, a separate process, has none and
// leaves the replicas to the daemon's controller through the registry.
type ReplicaRemover interface {
	RemoveOn(ctx context.Context, node string) error
}

// RemoveOptions are the flags of `supavise node rm`.
type RemoveOptions struct {
	// Force deletes the node's replica rows at once instead of waiting for the replica controller to
	// remove the instances: for a node that is unreachable.
	Force bool
	// Remover, when set, removes the node's replicas (RemoveOn) after the node is marked left. Without it
	// the rows are marked GOING_DOWN, which the replica controller of the daemon removes.
	Remover ReplicaRemover
	// Wait bounds the wait for the replicas to go; zero is five minutes. Poll is how often the registry
	// is looked at; zero is two seconds.
	Wait, Poll time.Duration
	// Log, when set, is told what the removal is waiting for.
	Log func(format string, a ...any)
}

// RemoveNode takes a node out of the cluster, on the leader, through the registry. It refuses the
// leader and a node that is the home of a project (fail the projects over first). Then it sets the node
// left, and only then deals with the replicas on it: while the node is active the replica controller's
// default (`[replicas] default = "all"`) makes a replica again for every one that goes, and the removal
// would never see the node empty. A left node's certificate is refused at the next handshake, and the
// node, which sees its own row in its copy of the registry, retires itself, so its instances need no
// removal: the controller drops what belongs to the pooler and deletes the rows of a node that has
// left. The system replica row is deleted here, the others are marked GOING_DOWN (or handed to Remover,
// or deleted with Force), and the call waits for them to go. A wait that ends first returns an error that
// says the node is removed already and what is left; the same command, run again, waits for the rest.
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
	rs, err := reg.ListReplicasOn(ctx, n.ID)
	if err != nil {
		return err
	}
	if n.State == registry.NodeLeft && len(rs) == 0 {
		return fmt.Errorf("node %s (%s) has been removed already", n.ID, n.Name)
	}
	if cl.Leader == n.ID {
		return fmt.Errorf("node %s is the leader; fail over to another node first (`supavise failover`)", n.ID)
	}
	if err := refuseHomed(ctx, reg, n.ID); err != nil {
		return err
	}
	prev := n.State
	if err := reg.SetNodeState(ctx, n.ID, registry.NodeLeft); err != nil {
		return err
	}
	// A move of a project onto the node that committed between the check above and the state change is
	// seen now; one that comes later is refused by the registry, which takes the node row for share and
	// finds it left. The node is put back as it was, because a project homed on a left node would be
	// stranded there, and the node, which reads its own row, would retire itself under it.
	if err := refuseHomed(ctx, reg, n.ID); err != nil {
		if rerr := reg.SetNodeState(ctx, n.ID, prev); rerr != nil {
			return fmt.Errorf("%w; and node %s could not be put back to %s: %v", err, n.ID, prev, rerr)
		}
		return err
	}
	for _, r := range rs {
		if r.IsSystemStandby() || o.Force {
			if err := reg.DeleteReplica(ctx, r.Identifier); err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
			continue
		}
		if o.Remover != nil {
			continue
		}
		if err := reg.SetReplicaStatus(ctx, r.Identifier, string(registry.StatusGoingDown), r.InitStep, r.InitError); err != nil && !errors.Is(err, registry.ErrNotFound) {
			return err
		}
	}
	if o.Remover != nil && !o.Force {
		// A removal the remover could not finish stays GOING_DOWN for its controller to retry; the wait
		// below sees it through.
		if err := o.Remover.RemoveOn(ctx, n.ID); err != nil {
			logf("removing the replicas on %s: %v", n.ID, err)
		}
	}
	deadline := time.Now().Add(o.Wait)
	for {
		left, err := reg.ListReplicasOn(ctx, n.ID)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			ids := make([]string, len(left))
			for i, r := range left {
				ids[i] = r.Identifier
			}
			return fmt.Errorf("node %s is removed, but the replicas %s are still being cleaned up: the replica controller of the leader's daemon does that and tries again, "+
				"or run this command with --force to delete their rows now", n.ID, strings.Join(ids, ", "))
		}
		logf("waiting for %d replica(s) on %s to be removed", len(left), n.ID)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// refuseHomed is the error of a node that is the home of a project: the projects are moved first.
func refuseHomed(ctx context.Context, reg registry.Registry, id string) error {
	ps, err := reg.ListProjects(ctx)
	if err != nil {
		return err
	}
	var homed []string
	for _, p := range ps {
		if p.NodeID == id {
			homed = append(homed, p.Ref)
		}
	}
	if len(homed) > 0 {
		return fmt.Errorf("node %s is the home of %d project(s) (%s); move them with `supavise projects failover` first", id, len(homed), strings.Join(homed, ", "))
	}
	return nil
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
