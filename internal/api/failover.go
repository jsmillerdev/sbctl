package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/supavise/supavise/internal/failover"
)

// FailoverSource answers the failover-readiness question. *failover.Orchestrator implements it.
type FailoverSource interface {
	Readiness(ctx context.Context) (failover.Readiness, error)
}

// failoverSource is the daemon's orchestrator. It is set once by the daemon's wiring
// (SetFailoverSource) because the Management API is built from Deps, and the readiness route is
// the only route that needs the orchestrator. A server with none answers 503.
var failoverSource atomic.Pointer[FailoverSource]

// SetFailoverSource tells the Management API where to ask whether a failover would be accepted.
// nil removes it.
func SetFailoverSource(src FailoverSource) {
	if src == nil {
		failoverSource.Store(nil)
		return
	}
	failoverSource.Store(&src)
}

// readinessTimeout bounds the question: it probes the fencer and the backup store.
const readinessTimeout = 20 * time.Second

func init() {
	// The answer names nodes and says what is wrong with them: for the people who run the node.
	routeRules = append(routeRules, rule("R", "/supavise/v1/failover/readiness", nOperator))
}

// routesFailover registers the Supavise-specific failover routes, outside the namespaces of the
// hosted API:
//
//	GET /supavise/v1/failover/readiness
//
// The answer is the block `supavise status` shows, as JSON (failover.Readiness): whether a
// server failover would be accepted now, why not, what the fencer says, and whether the backup
// store holding the leader marker is reachable. Owners and Administrators may ask. A node that is not
// part of a cluster answers 404: there is nothing to fail over to.
func (s *Server) routesFailover(add func(string, handlerFunc)) {
	add("GET /supavise/v1/failover/readiness", func(w http.ResponseWriter, r *http.Request) error {
		p := failoverSource.Load()
		if p == nil {
			return errf(http.StatusServiceUnavailable, "Failover is not available on this node")
		}
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		rd, err := (*p).Readiness(ctx)
		switch {
		case errors.Is(err, failover.ErrNoCluster):
			return errf(http.StatusNotFound, "This server is not part of a cluster")
		case err != nil:
			return errf(http.StatusServiceUnavailable, "Failover readiness could not be checked: %v", err)
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, rd)
		return nil
	})
}
