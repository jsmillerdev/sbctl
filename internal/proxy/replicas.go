package proxy

import (
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// routeHeader names the response header that says which database answered a request on a
// project's load balancer host: the replica's identifier, or the project's ref for the primary.
const routeHeader = "X-Supavise-Route"

// replicaServable reports whether a replica in status st may receive traffic on its own
// endpoint. A replica that is restarting or lagging is still asked: PostgREST answers or the proxy
// reports the upstream error. One that does not exist yet, failed to set up or is going away is not.
func replicaServable(st string) bool {
	switch st {
	case registry.ReplicaInit, registry.ReplicaInitError, string(registry.StatusGoingDown):
		return false
	}
	return true
}

// replicaAddr is the loopback address of the PostgREST of replica r of p: the replica itself on
// its node, a forwarder to it on every other (invariant I3). ok is false for a project whose
// sequence leaves no room for replica ports.
func (s *Server) replicaAddr(p project, r replica) (addr string, ok bool) {
	if s.replicaUpstreamFn != nil {
		return s.replicaUpstreamFn(p, r), true
	}
	if p.seq > s.cfg.MaxReplicaSeq() {
		return "", false
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.ReplicaPorts(p.ref, p.seq).PostgREST)), true
}

// database is one choice of the load balancer: the primary or a replica.
type database struct {
	// identifier is what X-Supavise-Route and the access log name: the ref for the primary.
	identifier string
	node       string
	// replica is nil for the primary.
	replica *replica
}

// candidates are the databases a balanced read may go to: the primary, and every replica that is
// ACTIVE_HEALTHY, runs on an active node this node has a mesh session to and, when [replicas]
// lb_max_lag_seconds is set, lags no more than that (a replica whose lag is unknown is skipped then).
func (s *Server) candidates(p project) []database {
	cs := []database{{identifier: p.ref, node: p.home}}
	maxLag := s.cfg.Replicas.LBMaxLag()
	for i := range p.replicas {
		r := &p.replicas[i]
		if r.status != string(registry.StatusActiveHealthy) || !s.table.nodeActive(r.node) || !s.cluster.connected(r.node) {
			continue
		}
		if _, ok := s.replicaAddr(p, *r); !ok {
			continue
		}
		if maxLag > 0 {
			if lag, ok := s.cluster.lag(r.identifier); !ok || lag > maxLag {
				continue
			}
		}
		cs = append(cs, database{identifier: r.identifier, node: r.node, replica: r})
	}
	return cs
}

// pickDatabase chooses where a balanced read goes: the database on the node that received the
// request; else the one on the node with the lowest round-trip time (from the peer heartbeats; a
// node with no reading yet is passed over); else the candidates in turn.
func (s *Server) pickDatabase(p project) database {
	cs := s.candidates(p)
	if len(cs) == 1 {
		return cs[0]
	}
	if self := s.cluster.self(); self != "" {
		for _, c := range cs {
			if c.node == self {
				return c
			}
		}
	}
	best, bestRTT := -1, time.Duration(0)
	for i, c := range cs {
		if d, ok := s.cluster.rtt(c.node); ok && (best < 0 || d < bestRTT) {
			best, bestRTT = i, d
		}
	}
	if best >= 0 {
		return cs[best]
	}
	n, ok := s.turns.Load(p.ref)
	if !ok {
		n, _ = s.turns.LoadOrStore(p.ref, new(atomic.Uint64))
	}
	return cs[(n.(*atomic.Uint64).Add(1)-1)%uint64(len(cs))]
}

// balanced reports whether a request may be sent to a replica on a load balancer host.
func balanced(r *http.Request, rt *route) bool {
	return rt.balance && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}
