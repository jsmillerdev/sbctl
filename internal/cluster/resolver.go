package cluster

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// InstanceDescriber is the part of the EC2 client the resolver uses (awsapi.EC2).
type InstanceDescriber interface {
	DescribeInstances(ctx context.Context, in awsapi.DescribeInstancesInput) ([]awsapi.Instance, error)
}

// AWSResolver finds the current address of a peer on AWS. The registry's peer_addr is a hint: a
// node that lost its Elastic IP to a takeover (design 2.10.6) is on an auto-assigned public address,
// or only on a private one, and the survivor reaches it through the addresses EC2 reports for its
// instance. A peer in the same region is tried on its private address first. The lookup goes to the
// region this node runs in, so a peer in another region gets no address from here (it is dialed on
// the peer_addr of its row), and so does a node without an AWS identity in its row.
type AWSResolver struct {
	EC2 InstanceDescriber
	// Self is this node, to tell a peer in its region from one in another.
	Self func() registry.Node
	// Port is the peer port when the peer's row has no peer_addr to take it from; zero is the
	// default peer port.
	Port int
	// TTL is how long an answer is kept; zero is 30 s. Dialing a peer retries, and EC2 throttles.
	TTL time.Duration
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedAddrs
}

type cachedAddrs struct {
	addrs []string
	until time.Time
}

var _ mesh.AddrResolver = (*AWSResolver)(nil)

// Addrs implements mesh.AddrResolver.
func (r *AWSResolver) Addrs(ctx context.Context, n registry.Node) ([]string, error) {
	if n.Provider.AWS == nil || n.Provider.AWS.InstanceID == "" || r.otherRegion(n) {
		return nil, nil
	}
	id := n.Provider.AWS.InstanceID
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	r.mu.Lock()
	if c, ok := r.cache[id]; ok && now().Before(c.until) {
		r.mu.Unlock()
		return c.addrs, nil
	}
	r.mu.Unlock()
	found, err := r.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{id}})
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, in := range found {
		if in.ID != id || in.State != "running" {
			continue
		}
		port := r.port(n)
		var public, private []string
		for _, ni := range in.NetworkInterfaces {
			for _, ip := range ni.PrivateIPs {
				if ip.PublicIP != "" {
					public = append(public, net.JoinHostPort(ip.PublicIP, port))
				}
			}
		}
		if in.PublicIP != "" {
			public = append([]string{net.JoinHostPort(in.PublicIP, port)}, public...)
		}
		if in.PrivateIP != "" {
			private = append(private, net.JoinHostPort(in.PrivateIP, port))
		}
		if r.sameRegion(n) {
			addrs = append(addrs, private...)
			addrs = append(addrs, public...)
		} else {
			addrs = append(addrs, public...)
			addrs = append(addrs, private...)
		}
	}
	addrs = dedupe(addrs)
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedAddrs{}
	}
	r.cache[id] = cachedAddrs{addrs: addrs, until: now().Add(ttl)}
	r.mu.Unlock()
	return addrs, nil
}

func (r *AWSResolver) port(n registry.Node) string {
	if _, p, err := net.SplitHostPort(n.PeerAddr); err == nil && p != "" {
		return p
	}
	if r.Port > 0 {
		return strconv.Itoa(r.Port)
	}
	return strconv.Itoa(config.PortPeer)
}

// otherRegion reports whether n is known to be in another region than this node.
func (r *AWSResolver) otherRegion(n registry.Node) bool {
	if r.Self == nil {
		return false
	}
	a, b := r.Self().Provider.AWS, n.Provider.AWS
	return a != nil && b != nil && a.Region != "" && b.Region != "" && a.Region != b.Region
}

func (r *AWSResolver) sameRegion(n registry.Node) bool {
	if r.Self == nil {
		return false
	}
	a, b := r.Self().Provider.AWS, n.Provider.AWS
	return a != nil && b != nil && a.Region != "" && a.Region == b.Region
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
