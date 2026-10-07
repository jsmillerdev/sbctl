package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jsmillerdev/supavise/internal/health"
	"github.com/jsmillerdev/supavise/internal/members"
)

// HealthSource is the node's health report. *health.Monitor implements it, so every request
// shares one probe of the node and a burst of requests costs one check, not one each.
type HealthSource interface {
	Report(ctx context.Context) (*health.Report, error)
}

// healthTimeout bounds how long a health request waits for the report.
const healthTimeout = 10 * time.Second

// healthzRoutes serves GET /healthz on the API host, for external uptime monitors and load
// balancers. It needs no credentials and gives away nothing: the answer is {"status": ...} with
// the verdict (healthy, degraded or down), no project names, no versions, no component names.
// The status code is 200 unless the node is down (503). A degraded node answers 200 on purpose:
// a load balancer or a restarter that acts on a non-200 answer must not pull a node out of
// service because one project's PostgREST stopped; "degraded" in the body is there for the monitor
// that reads it, and the alerts carry the rest. Without a health source (tests, the dev mock) it
// answers as a liveness check: the process serves, so it is healthy.
func (s *Server) healthzRoutes(mux *muxSet) {
	mux.handle("GET /healthz", s.wrap("", authNone, func(w http.ResponseWriter, r *http.Request) error {
		verdict := health.Healthy
		if s.healthSrc != nil {
			ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
			defer cancel()
			if rep, err := s.healthSrc.Report(ctx); err != nil {
				verdict = health.Down
			} else {
				verdict = rep.Verdict
			}
		}
		code := http.StatusOK
		if verdict == health.Down {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, code, health.Public{Status: verdict})
		return nil
	}))
}

// routesHealth adds GET /healthz/detail: the whole report for operators (Owner and
// Administrator, by session or personal access token; authz.go). The components describe the
// node and are shown in full; the projects are only those of organizations the caller is an
// Owner or Administrator of, and the summary names no other.
func (s *Server) routesHealth(add func(string, handlerFunc)) {
	add("GET /healthz/detail", func(w http.ResponseWriter, r *http.Request) error {
		if s.healthSrc == nil {
			return errf(http.StatusServiceUnavailable, "Node health checks are not running on this node")
		}
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()
		rep, err := s.healthSrc.Report(ctx)
		if err != nil {
			return errf(http.StatusServiceUnavailable, "The node's health could not be checked: %v", err)
		}
		access, err := s.callerAccess(r)
		if err != nil {
			return err
		}
		rep = rep.FilterProjects(func(p health.ProjectResult) bool {
			role := access.OrgRole(p.OrgID)
			return role == members.RoleOwner || role == members.RoleAdministrator
		})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, rep)
		return nil
	})
}
