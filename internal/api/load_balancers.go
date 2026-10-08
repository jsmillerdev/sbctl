package api

// routesLoadBalancers registers the listing of a project's API load balancer:
//
//	GET /platform/projects/{ref}/load-balancers
//
// The handler belongs to the read-replica work; until it exists the spec-derived stub answers.
func (s *Server) routesLoadBalancers(add func(string, handlerFunc)) {}
