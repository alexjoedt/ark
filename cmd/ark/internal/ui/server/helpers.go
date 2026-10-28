package server

import (
	"encoding/json"
	"net/http"
)

// Helper functions for handling HTTP responses

// respondJSON sends a JSON response.
func (s *Server) respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			s.logger.Error("failed to encode JSON response", "error", err)
		}
	}
}

// respondError sends an error response.
func (s *Server) respondError(w http.ResponseWriter, status int, message string) {
	s.respondJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
		},
	})
}

// decodeJSON decodes JSON from request body with size limit.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}

	return nil
}
