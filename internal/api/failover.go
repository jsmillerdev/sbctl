package api

// routesFailover registers the Supavise-specific failover routes, outside the namespaces of the
// hosted API:
//
//	GET /supavise/v1/failover/readiness
//
// The handler belongs to the failover work.
func (s *Server) routesFailover(add func(string, handlerFunc)) {}
