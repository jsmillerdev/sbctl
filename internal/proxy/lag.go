package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// lagTTL is how long a reading of a project's replicas is reused: the controller measures every ten
// seconds, and a burst of balanced reads should ask it once.
const lagTTL = 5 * time.Second

// ReplicaLag returns a Cluster.Lag that reads the lag from the replica controller. Only the leader has
// the readings, so a node that is not the leader knows no lag, and with [replicas] lb_max_lag_seconds
// set its load balancer sends no read to a replica.
func ReplicaLag(svc replicas.Service, leader func() bool) func(identifier string) (time.Duration, bool) {
	c := &lagCache{svc: svc, leader: leader, now: time.Now, refs: map[string]lagReading{}}
	return c.lag
}

type lagCache struct {
	svc    replicas.Service
	leader func() bool
	now    func() time.Time

	mu   sync.Mutex
	refs map[string]lagReading
}

// lagReading is what the controller said about a project's replicas at a time; a replica with no
// entry has unknown lag.
type lagReading struct {
	at  time.Time
	lag map[string]time.Duration
}

func (c *lagCache) lag(identifier string) (time.Duration, bool) {
	if !c.leader() {
		return 0, false
	}
	ref, _, _, ok := registry.ParseReplicaIdentifier(identifier)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	r, have := c.refs[ref]
	c.mu.Unlock()
	if !have || c.now().Sub(r.at) >= lagTTL {
		r = c.read(ref)
	}
	d, ok := r.lag[identifier]
	return d, ok
}

// read asks the controller about ref's replicas. A failed question is remembered as "unknown" for the
// same time as an answer, so that an unreachable controller is not asked once per request.
func (c *lagCache) read(ref string) lagReading {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := lagReading{at: c.now(), lag: map[string]time.Duration{}}
	if sts, err := c.svc.Statuses(ctx, ref); err == nil {
		for _, st := range sts {
			if st.LagSeconds >= 0 {
				r.lag[st.Identifier] = time.Duration(st.LagSeconds * float64(time.Second))
			}
		}
	}
	c.mu.Lock()
	c.refs[ref] = r
	c.mu.Unlock()
	return r
}
