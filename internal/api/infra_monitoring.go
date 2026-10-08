package api

// routesInfraMonitoring registers the infrastructure metrics of a project's databases, which
// carry the replication lag of a replica:
//
//	GET /platform/projects/{ref}/infra-monitoring
//
// The handler belongs to the read-replica work; until it exists the spec-derived stub answers.
func (s *Server) routesInfraMonitoring(add func(string, handlerFunc)) {}
