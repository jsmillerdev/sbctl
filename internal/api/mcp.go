package api

import "net/http"

// MCPGate is proxy.Options.MCPGate: the gate in front of the remote MCP endpoint (api.<domain>/mcp)
// that the proxy asks before it forwards the request to Studio. It authenticates the bearer, answers
// OPTIONS and every refusal itself, and returns the query to forward. ok false means the answer has
// been written and nothing is forwarded.
//
// Until the gate is written it refuses every request with 404, so that nothing reaches Studio.
func (s *Server) MCPGate(w http.ResponseWriter, r *http.Request) (rawQuery string, ok bool) {
	writeError(w, errf(http.StatusNotFound, "Not Found"))
	return "", false
}
