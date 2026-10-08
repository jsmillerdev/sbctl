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
