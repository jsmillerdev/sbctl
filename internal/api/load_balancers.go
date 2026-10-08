package api

import "net/http"

// A project's API load balancer: once it has a replica, https://<ref>-lb.api.<domain> sends the
// Data API's reads to the nearest healthy database and everything else to the primary. The proxy
// serves it (internal/proxy); this route is how Studio learns that it exists and which databases
// stand behind it.

func (s *Server) routesLoadBalancers(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/load-balancers", s.listLoadBalancers)
}

// lbEndpoint is the balancer's public origin.
func (s *Server) lbEndpoint(ref string) string { return s.projectURL(ref + "-lb") }

// listLoadBalancers answers one balancer for a project with at least one replica and none
// otherwise. While the proxy does not serve balancers (Deps.LoadBalancers false) it answers none
// whatever the replicas, so Studio does not show an endpoint that nothing answers.
func (s *Server) listLoadBalancers(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	rs, err := s.replicasOf(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	if !s.lbOn || len(rs) == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return nil
	}
	dbs := []any{map[string]any{"identifier": p.Ref, "type": "PRIMARY", "status": databaseStatus(p.Status)}}
	for _, rep := range rs {
		dbs = append(dbs, map[string]any{"identifier": rep.Identifier, "type": "READ_REPLICA", "status": rep.Status})
	}
	writeJSON(w, http.StatusOK, []any{map[string]any{"endpoint": s.lbEndpoint(p.Ref), "databases": dbs}})
	return nil
}
