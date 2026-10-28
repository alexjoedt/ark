package server

import (
	"context"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexjoedt/log"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/models"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/services"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/websocket"
)

// pinger is a minimal interface for checking database connectivity.
type pinger interface {
	Ping() error
}

// Config holds server configuration.
type Config struct {
	HTTPAddr       string
	SessionManager *scs.SessionManager
	UserService    *services.UserService
	JobService     *services.JobService
	PolicyService  *services.PolicyService
	Repo           *ark.Repository
	Docker         *arkdocker.Engine
	RepoDir        string
	HistoryService *services.JobHistoryService
	Scheduler      *services.Scheduler
	Hub            *websocket.Hub
	Logger         *log.Logger
	DevMode        bool

	// Security
	CSRFEnabled     bool
	RateLimitWindow time.Duration
	RateLimitTokens int

	// DB is used for health-check pings.
	DB pinger
}

// Server represents the arkUI HTTP server.
type Server struct {
	config     *Config
	router     *chi.Mux
	httpServer *http.Server

	// Convenience accessors for services
	sessionManager *scs.SessionManager
	userService    *services.UserService
	jobService     *services.JobService
	policyService  *services.PolicyService
	repo           *ark.Repository
	docker         *arkdocker.Engine
	repoDir        string
	historyService *services.JobHistoryService
	scheduler      *services.Scheduler
	hub            *websocket.Hub
	logger         *log.Logger

	// loginLimiter is a per-IP rate limiter for the login endpoint.
	loginLimiter *ipRateLimiter
	// db is used for health-check pings.
	db pinger
}

// New creates a new server instance.
func New(cfg *Config) *Server {
	// Verify static files are embedded when not in dev mode
	if !cfg.DevMode {
		if err := VerifyStaticFiles(); err != nil {
			cfg.Logger.Error("static files verification failed", "error", err)
			cfg.Logger.Error("to fix this, build the web UI first: task build")
			cfg.Logger.Error("or enable dev mode: ARK_DEV_MODE=true")
			panic(err)
		}
		cfg.Logger.Info("static files verified successfully")
	}

	// Build the login rate limiter from config. Fall back to safe defaults if
	// the caller left these fields at zero.
	rateLimitWindow := cfg.RateLimitWindow
	if rateLimitWindow <= 0 {
		rateLimitWindow = time.Minute
	}
	rateLimitTokens := cfg.RateLimitTokens
	if rateLimitTokens <= 0 {
		rateLimitTokens = 10
	}
	loginLimiter := newIPRateLimiter(
		float64(rateLimitTokens)/rateLimitWindow.Seconds(),
		rateLimitTokens,
	)

	s := &Server{
		config:         cfg,
		router:         chi.NewRouter(),
		sessionManager: cfg.SessionManager,
		userService:    cfg.UserService,
		jobService:     cfg.JobService,
		policyService:  cfg.PolicyService,
		repo:           cfg.Repo,
		docker:         cfg.Docker,
		repoDir:        cfg.RepoDir,
		historyService: cfg.HistoryService,
		scheduler:      cfg.Scheduler,
		hub:            cfg.Hub,
		logger:         cfg.Logger,
		loginLimiter:   loginLimiter,
		db:             cfg.DB,
	}

	s.setupMiddleware()
	s.setupRoutes(cfg.DevMode)

	// Configure CORS middleware.
	// In dev mode, allow the Vite dev server origin.
	// In production, same-origin requests don't need CORS headers.
	var origins []string
	if cfg.DevMode {
		origins = []string{"http://localhost:5173"}
	}

	crs := cors{origins: origins}

	s.httpServer = &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      crs.EnableCORS(s.router),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

// setupMiddleware configures global middleware. RemoteAddr stays the TCP
// peer: the login limiter keys on it, and a forwarded-for header is whatever
// the client wants it to be.
func (s *Server) setupMiddleware() {
	s.router.Use(middleware.RequestID)
	s.router.Use(s.structuredLogger)
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.Timeout(30 * time.Second))
	s.router.Use(s.sessionManager.LoadAndSave)
}

// setupRoutes configures all HTTP routes.
func (s *Server) setupRoutes(devMode bool) {
	// Public health check (no auth required — used by Docker healthcheck).
	s.router.Get("/health", s.handleHealthCheck)

	// WebSocket endpoint — session cookies are sent automatically, so
	// requireAuth works here without any URL change.
	s.router.With(s.requireAuth).Get("/ws", websocket.Handler(s.hub, s.logger))

	// API routes
	s.router.Route("/api", func(r chi.Router) {
		// Public routes
		r.With(s.rateLimitLogin).Post("/auth/login", s.handleLogin)
		r.Post("/auth/logout", s.handleLogout)

		// Protected routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Use(s.csrfProtect)

			r.Get("/auth/csrf", s.handleCSRF)

			r.Get("/auth/me", s.handleGetMe)

			// Engine operations (backup, restore, snapshots, volumes)
			r.Get("/status", s.handleStatus)
			r.Get("/docker/health", s.handleDockerHealth)
			r.Get("/volumes", s.handleListVolumes)
			r.Get("/volumes/{name}", s.handleGetVolume)
			r.Get("/volumes/{name}/snapshots", s.handleListSnapshots)
			r.Get("/snapshots", s.handleListAllSnapshots)
			r.Get("/snapshots/{id}", s.handleGetSnapshot)
			r.Delete("/snapshots/{id}", s.handleDeleteSnapshot)
			r.Post("/backup", s.handleBackup)
			r.Post("/restore", s.handleRestore)
			r.Post("/restore/dry-run", s.handleRestoreDryRun)

			// Export / Import
			r.Get("/snapshots/{id}/export", s.handleExportSnapshot)
			r.Post("/import", s.handleImportSnapshot)

			// Jobs
			r.Get("/jobs", s.handleListJobs)
			r.Post("/jobs", s.handleCreateJob)
			r.Get("/jobs/{id}", s.handleGetJob)
			r.Put("/jobs/{id}", s.handleUpdateJob)
			r.Delete("/jobs/{id}", s.handleDeleteJob)
			r.Post("/jobs/{id}/run", s.handleRunJobNow)
			r.Get("/jobs/{id}/history", s.handleGetJobHistory)

			// History
			r.Get("/history", s.handleListRecentHistory)

			// Policies
			r.Get("/policies", s.handleListPolicies)
			r.Post("/policies", s.handleCreatePolicy)
			r.Get("/policies/{id}", s.handleGetPolicy)
			r.Put("/policies/{id}", s.handleUpdatePolicy)
			r.Delete("/policies/{id}", s.handleDeletePolicy)

			// Admin-only routes
			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin)

				// User management
				r.Get("/users", s.handleListUsers)
				r.Post("/users", s.handleCreateUser)
				r.Get("/users/{id}", s.handleGetUser)
				r.Delete("/users/{id}", s.handleDeleteUser)
				r.Put("/users/{id}/password", s.handleUpdatePassword)
			})
		})
	})

	// Serve static files (React build) only in production mode
	if !devMode {
		spa, err := newSPAHandler()
		if err != nil {
			s.logger.Error("failed to create SPA handler", "error", err)
			panic(err)
		}
		s.router.Handle("/*", spa)
		s.logger.Info("serving static files from embedded build")
	} else {
		s.logger.Info("dev mode enabled - static files not served (use Vite dev server)")
	}
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	s.logger.Info("starting HTTP server", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down HTTP server")
	return s.httpServer.Shutdown(ctx)
}

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

const (
	ctxKeyUser contextKey = "user"
)

// userFromContext retrieves the user from the request context.
func userFromContext(ctx context.Context) *models.User {
	user, _ := ctx.Value(ctxKeyUser).(*models.User)
	return user
}

// contextWithUser adds a user to the request context.
func contextWithUser(ctx context.Context, user *models.User) context.Context {
	return context.WithValue(ctx, ctxKeyUser, user)
}
