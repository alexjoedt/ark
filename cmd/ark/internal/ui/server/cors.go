package server

import (
	"net/http"
	"slices"
	"strings"
)

type cors struct {
	origins []string
}

func (c *cors) EnableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		w.Header().Add("Vary", "Access-Control-Request-Method")
		w.Header().Add("Vary", "Access-Control-Request-Headers")

		origin := r.Header.Get("Origin")

		if origin != "" && c.isAllowedOrigin(origin) {
			// Set the origin that made the request (required for credentials)
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Access-Control-Allow-Origin
			// Enable credentials (cookies, authorization headers)
			w.Header().Set("Access-Control-Allow-Credentials", "true")

			if isPreflight(r) {
				// Allow common HTTP methods
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")

				// Allow common headers
				allowedHeaders := []string{
					"Authorization",
					"Content-Type",
					"Accept",
					"Origin",
					"X-Requested-With",
				}
				w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))

				// Cache preflight response for 24 hours
				w.Header().Set("Access-Control-Max-Age", "86400")

				w.WriteHeader(http.StatusNoContent)
				return
			}

			// For actual requests, allow specific headers to be exposed to the client
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")
		}

		next.ServeHTTP(w, r)
	})
}

// isAllowedOrigin checks if the origin is in the allowed list
func (c *cors) isAllowedOrigin(origin string) bool {
	return slices.Contains(c.origins, origin)
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}
