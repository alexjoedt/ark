package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

// structuredLogger is a request-logging middleware that emits one structured
// log line per request using the server's slog-backed logger, matching the
// field names used by arkd: method, path, status, duration, request_id.
func (s *Server) structuredLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.logger.Info("request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.Status()),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", middleware.GetReqID(r.Context())),
			slog.String("component", "arkui"),
		)
	})
}

// requireAuth middleware ensures the user is authenticated.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get user ID from session
		userID := s.sessionManager.GetInt64(r.Context(), "user_id")
		if userID == 0 {
			s.respondError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		// Get user from database
		user, err := s.userService.GetByID(userID)
		if err != nil {
			s.logger.Error("failed to get user from session", "user_id", userID, "error", err)
			s.respondError(w, http.StatusUnauthorized, "invalid session")
			return
		}

		// Add user to context
		ctx := contextWithUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAdmin middleware ensures the user is an admin.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFromContext(r.Context())
		if user == nil {
			s.respondError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		if !user.IsAdmin() {
			s.respondError(w, http.StatusForbidden, "admin access required")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ipRateLimiter holds per-IP token-bucket rate limiters.
type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	r        rate.Limit
	b        int
}

// newIPRateLimiter creates a new per-IP rate limiter.
// tokensPerSecond is the sustained request rate; burst is the maximum burst.
func newIPRateLimiter(tokensPerSecond float64, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		r:        rate.Limit(tokensPerSecond),
		b:        burst,
	}
}

// getLimiter returns the rate limiter for the given IP, creating one if needed.
func (l *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	limiter, exists := l.limiters[ip]
	if !exists {
		limiter = rate.NewLimiter(l.r, l.b)
		l.limiters[ip] = limiter
	}

	return limiter
}

// rateLimitLogin is a per-IP rate-limiting middleware for the login endpoint.
// It uses a token-bucket algorithm; the rate and burst are configured via
// RateLimitTokens and RateLimitWindow in the server Config.
func (s *Server) rateLimitLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}

		if !s.loginLimiter.getLimiter(ip).Allow() {
			w.Header().Set("Retry-After", "60")
			s.respondError(w, http.StatusTooManyRequests, "too many login attempts, try again later")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// csrfProtect validates CSRF tokens on state-changing requests (POST, PUT, DELETE, PATCH).
// It is a no-op when CSRFEnabled is false in the server Config.
// Clients must first call GET /api/auth/csrf to obtain a token, then include it
// in the X-CSRF-Token request header on every mutating request.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.config.CSRFEnabled {
			next.ServeHTTP(w, r)
			return
		}

		// Safe methods do not require CSRF validation.
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			next.ServeHTTP(w, r)
			return
		}

		sessionToken := s.sessionManager.GetString(r.Context(), "csrf_token")
		if sessionToken == "" {
			s.respondError(w, http.StatusForbidden, "CSRF token not found in session; call GET /api/auth/csrf first")
			return
		}

		headerToken := r.Header.Get("X-CSRF-Token")
		if headerToken == "" || headerToken != sessionToken {
			s.respondError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// generateCSRFToken returns a cryptographically random 32-byte hex-encoded token.
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
