package server

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
)

// SessionConfig holds configurable session parameters.
type SessionConfig struct {
	Lifetime   time.Duration
	CookieName string
	Secure     bool // Set to true in production with HTTPS
}

// NewSessionManager creates a new session manager with SQLite store.
// It accepts a SessionConfig to allow the caller to control session settings
// from environment-based configuration.
func NewSessionManager(db *sql.DB, cfg SessionConfig) *scs.SessionManager {
	store := newSessionStore(db, 5*time.Minute)

	// Apply defaults for zero values
	if cfg.Lifetime == 0 {
		cfg.Lifetime = 24 * time.Hour
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "ark_session"
	}

	// Create session manager
	sessionManager := scs.New()
	sessionManager.Store = store
	sessionManager.Lifetime = cfg.Lifetime
	sessionManager.Cookie.Name = cfg.CookieName
	sessionManager.Cookie.HttpOnly = true
	sessionManager.Cookie.Secure = cfg.Secure
	sessionManager.Cookie.SameSite = http.SameSiteLaxMode

	return sessionManager
}
