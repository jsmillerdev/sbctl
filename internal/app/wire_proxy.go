package app

import (
	"context"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/replicas"
)

// wireProxy gives the edge proxy the replica and load-balancer hosts and, on a follower, the
// certificate mirror and the synced or managing switch (design 2.8). It sets the fields it owns
// on w.Proxy, serves the leader's certificate store to the followers (GET /peer/v1/certs), and
// provides the *proxy.CertRole the proxy follows. On a node with no cluster it does nothing.
func wireProxy(ctx context.Context, w *Wire) error {
	m, ok := Get[mesh.Mesh](w)
	if !ok {
		return nil
	}
	ms, ok := Get[cluster.Membership](w)
	if !ok {
		return nil
	}

	leader := func() (string, bool) {
		n, ok := ms.Leader()
		return n.ID, ok
	}
	// The role follows the membership: the leader issues, every other node (a follower, a fenced
	// leader) mirrors. A promotion or demotion in between reaches it through Watch too.
	role := proxy.NewCertRole(ms.IsLeader())
	Provide[*proxy.CertRole](w, role)
	w.Go("proxy certificate role", func(ctx context.Context) error {
		for snap := range ms.Watch(ctx) {
			if snap.Role == cluster.RoleLeader {
				role.Promote()
			} else {
				role.Demote()
			}
		}
		return nil
	})
	mesh.Handle("GET "+peerapi.PathCerts, proxy.CertsHandler(w.Cfg.Paths().Certs(), ms.IsLeader, w.Log))

	c := &proxy.Cluster{
		Self:  func() string { return ms.Self().ID },
		RTT:   m.RTT,
		Certs: &proxy.CertSync{Role: role, Source: proxy.MeshCerts{RPC: m, Leader: leader}},
	}
	// The replica controller (wire_replicas.go) knows the lag; without it the load balancer's lag
	// limit has nothing to read.
	if svc, ok := Get[replicas.Service](w); ok {
		c.Lag = proxy.ReplicaLag(svc, ms.IsLeader)
	}
	w.Proxy.Cluster = c
	// The proxy serves <ref>-lb.api.<domain> once a project has a replica; Studio lists the endpoint
	// only if the Management API says so.
	w.API.LoadBalancers = true
	// The proxy can tell whether the certificates it mirrored are the leader's; the preflight of a
	// server move does not ask yet, because the proxy offers no such question (proxy.CertSync has no
	// state to read).
	w.Off("failover.CertificateCheck", "the proxy does not report whether its mirror of the leader's certificates is current; a server move does not check it")
	return nil
}
