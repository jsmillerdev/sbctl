package api

// routesDatabases registers the listings of a project's databases, the primary and its read
// replicas:
//
//	GET /platform/projects/{ref}/databases
//	GET /platform/projects/{ref}/databases-statuses
//
// The handlers belong to the read-replica work; until they exist the spec-derived stubs answer.
func (s *Server) routesDatabases(add func(string, handlerFunc)) {}
