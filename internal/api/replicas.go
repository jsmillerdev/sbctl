package api

// routesReplicas registers the operations that add and remove a project's read replicas:
//
//	POST /v1/projects/{ref}/read-replicas/setup
//	POST /v1/projects/{ref}/read-replicas/remove
//
// The handlers belong to the read-replica work; until they exist the spec-derived stubs answer.
func (s *Server) routesReplicas(add func(string, handlerFunc)) {}
