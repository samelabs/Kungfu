package server

import (
	"net/http"
)

// handleHealth is the liveness probe: the HTTP process/router can
// respond. It does not touch PostgreSQL, Credits, repository,
// service, domain, Creem, PostAPI, or the rate limiter. There is no
// mutable healthy flag — liveness is "this handler ran".
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	SuccessResponse(w, map[string]interface{}{"status": "ok"}, "")
}

// handleReady is the readiness probe. The only process-level runtime
// dependency is PostgreSQL; it is judged by a direct Ping on the
// existing pool using the request context (the R2.1 25s budget is
// the single timeout owner). Failures map to the existing
// ErrorResponse contract — never a raw DB error, DSN, or host.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		ErrorResponse(w, 503, "NOT_READY", "Service not ready", nil)
		return
	}
	SuccessResponse(w, map[string]interface{}{"status": "ready"}, "")
}
