package proxy

import "time"

// Cluster ties the proxy to the cluster its node belongs to (design 2.8). Options.Cluster is nil
// on a server that stands alone, and the proxy then behaves as it did before clusters existed.
type Cluster struct {
	// Self is the id of this node. The load balancer prefers the database that runs on it.
	Self func() string
	// RTT is the round-trip time to a peer node, from the peer heartbeats; ok is false while the
	// node has not answered one. The load balancer prefers the nearest database when none runs here.
	RTT func(node string) (d time.Duration, ok bool)
	// Lag is the latest replication lag of a replica; ok is false while it is unknown. Nil when
	// nothing measures it. With [replicas] lb_max_lag_seconds set, the load balancer sends no read to a
	// replica whose lag is above the limit or unknown.
	Lag func(identifier string) (d time.Duration, ok bool)
	// Connected reports whether the mesh has a session to a node now. The load balancer sends no read to a
	// replica on a node that has none, since the heartbeat times of a node that went away are the last ones
	// it answered. Nil means every node is connected. This node always is.
	Connected func(node string) bool
	// Leader reports whether this node leads the cluster. Only the leader serves api.<domain> itself: any
	// other node forwards it to the Management API's loopback listener ([listen] admin), where the mesh
	// has put a forwarder to the leader's (design 2.6). Nil means this node leads.
	Leader func() bool
	// Fenced reports whether the cluster role of this node is fenced. The node record of
	// internal/failover/fenced fences the node as well, whatever this says. Nil means not fenced.
	Fenced func() bool
	// Certs, when set, makes the node mirror the leader's certificates while it follows (see CertSync).
	Certs *CertSync
}

func (c *Cluster) self() string {
	if c == nil || c.Self == nil {
		return ""
	}
	return c.Self()
}

func (c *Cluster) rtt(node string) (time.Duration, bool) {
	if c == nil || c.RTT == nil {
		return 0, false
	}
	return c.RTT(node)
}

func (c *Cluster) lag(identifier string) (time.Duration, bool) {
	if c == nil || c.Lag == nil {
		return 0, false
	}
	return c.Lag(identifier)
}

func (c *Cluster) connected(node string) bool {
	if c == nil || c.Connected == nil || node == "" || node == c.self() {
		return true
	}
	return c.Connected(node)
}

func (c *Cluster) leads() bool {
	return c == nil || c.Leader == nil || c.Leader()
}

func (c *Cluster) fenced() bool {
	return c != nil && c.Fenced != nil && c.Fenced()
}
