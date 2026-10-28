package server

import (
	"net/http"
)

// handleHealthCheck is a public endpoint for liveness/readiness probing.
// Checks database connectivity and returns a JSON status object.
// Always responds with 200 when the process is alive; the database status
// is reflected in the JSON body so callers can distinguish partial failures.
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Status   string `json:"status"`
		Database string `json:"database"`
	}

	resp := response{
		Status:   "ok",
		Database: "ok",
	}

	// Check database connectivity.
	if s.db != nil {
		if err := s.db.Ping(); err != nil {
			s.logger.Warn("health check: database ping failed", "error", err)
			resp.Database = "error"
			resp.Status = "degraded"
		}
	}

	s.respondJSON(w, http.StatusOK, resp)
}
