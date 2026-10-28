package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexjoedt/log"
	"github.com/urfave/cli/v3"

	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/cmd/ark/internal/config"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/database"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/server"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/services"
	"github.com/alexjoedt/ark/cmd/ark/internal/ui/websocket"
	"github.com/alexjoedt/ark/cmd/ark/internal/version"
)

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "start the web UI (not covered by the v1 promise)",
		UsageText: `ark serve [options]

   Serves the web UI on --addr. The repository comes from --repo, else from
   /etc/ark/daemon.json. Docker is reached through --docker-host or DOCKER_HOST.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "docker-host",
				Usage:   "Docker daemon `ADDR`",
				Sources: cli.EnvVars("DOCKER_HOST"),
			},
			&cli.StringFlag{
				Name:    "addr",
				Aliases: []string{"a"},
				Usage:   "HTTP server bind address (overrides config and ARK_HTTP_ADDR)",
			},
			&cli.StringFlag{
				Name:  "db",
				Usage: "Path to SQLite database file (overrides config and ARK_DB_PATH)",
			},
			&cli.StringFlag{
				Name:    "admin-password",
				Usage:   "Admin user password — env var only, never written to disk (ARK_ADMIN_PASSWORD)",
				Sources: cli.EnvVars("ARK_ADMIN_PASSWORD"),
			},
			&cli.StringFlag{
				Name:  "admin-username",
				Usage: "Admin username (overrides config and ARK_ADMIN_USERNAME)",
			},
			&cli.BoolFlag{
				Name:  "dev",
				Usage: "Development mode: serve API only, no static files (use Vite dev server on :5173)",
			},
			&cli.BoolFlag{
				Name:  "no-csrf",
				Usage: "Disable CSRF protection (not recommended in production)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			logger := log.New(
				log.WithFormat(log.FormatConsole),
				log.WithLevel(log.INFO),
			)

			logger.Info("starting ark serve",
				"version", version.GetVersion(),
			)

			// Load fully resolved serve config (daemon.json → user config → env vars).
			serveCfg, err := config.LoadServe(func(msg string) {
				logger.Info(msg)
			})
			if err != nil {
				return err
			}
			// CLI flags are the final override layer.
			applyServeFlags(serveCfg, cmd)

			// Open repository and create Docker engine.
			repo, err := openRepoAt(ctx, cmd, serveCfg.RepoPath)
			if err != nil {
				return err
			}
			defer repo.Close()

			dockerEngine, err := arkdocker.New(repo,
				arkdocker.WithDockerHost(serveCfg.DockerHost),
			)
			if err != nil {
				return err
			}

			dbPath := serveCfg.DBPath

			logger.Info("initializing database", "path", dbPath)
			db, err := database.New(dbPath)
			if err != nil {
				logger.Error("failed to initialize database", "error", err)
				os.Exit(1)
			}
			defer db.Close()

			// Initialize services.
			userService := services.NewUserService(db.DB())
			jobService := services.NewJobService(db.DB())
			policyService := services.NewPolicyService(db.DB())
			historyService := services.NewJobHistoryService(db.DB())

			// Seed admin user if password is provided.
			adminPassword := cmd.String("admin-password")
			adminUsername := serveCfg.AdminUsername
			if adminPassword != "" {
				logger.Info("ensuring admin user exists", "username", adminUsername)
				if err := userService.EnsureAdminExists(adminUsername, adminPassword); err != nil {
					logger.Error("failed to create admin user", "error", err)
					os.Exit(1)
				}
			} else {
				logger.Warn(
					"no admin password provided (--admin-password / ARK_ADMIN_PASSWORD), skipping admin creation",
				)
			}

			// Initialize WebSocket hub.
			hub := websocket.NewHub(logger)
			go hub.Run()

			// Initialize scheduler with direct engine calls.
			scheduler := services.NewScheduler(
				jobService,
				dockerEngine,
				repo,
				policyService,
				historyService,
				hub,
				logger,
			)

			logger.Info("starting scheduler")
			if err := scheduler.Start(); err != nil {
				logger.Error("failed to start scheduler", "error", err)
				os.Exit(1)
			}
			defer scheduler.Stop()

			// Initialize session manager.
			sessionManager := server.NewSessionManager(db.DB(), server.SessionConfig{
				Lifetime:   24 * time.Hour,
				CookieName: "ark_session",
				Secure:     false, // Set true for HTTPS in production
			})

			devMode := serveCfg.DevMode
			csrfEnabled := serveCfg.CSRFEnabled

			// Create and start HTTP server.
			srv := server.New(&server.Config{
				HTTPAddr:        serveCfg.HTTPAddr,
				SessionManager:  sessionManager,
				UserService:     userService,
				JobService:      jobService,
				PolicyService:   policyService,
				Repo:            repo,
				Docker:          dockerEngine,
				RepoDir:         serveCfg.RepoPath,
				HistoryService:  historyService,
				Scheduler:       scheduler,
				Hub:             hub,
				Logger:          logger,
				DevMode:         devMode,
				CSRFEnabled:     csrfEnabled,
				RateLimitWindow: time.Minute,
				RateLimitTokens: 10,
				DB:              db,
			})

			// Start server in goroutine.
			go func() {
				logger.Info("HTTP server listening", "addr", serveCfg.HTTPAddr)
				if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTP server failed", "error", err)
					os.Exit(1)
				}
			}()

			// Wait for interrupt signal.
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			<-sigChan

			logger.Info("shutting down gracefully")
			hub.Stop()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error("server shutdown failed", "error", err)
			}

			logger.Info("ark serve stopped")
			return nil
		},
	}
}

// applyServeFlags overlays explicitly provided CLI flags onto cfg.
// This is the final override layer on top of config files and env vars.
func applyServeFlags(cfg *config.ServeConfig, cmd *cli.Command) {
	// The repository comes from the global flag, else from daemon.json.
	if v := cmd.Root().String("repo"); v != "" {
		cfg.RepoPath = v
	}
	if cmd.IsSet("docker-host") {
		cfg.DockerHost = cmd.String("docker-host")
	}
	// Serve-specific flags.
	if cmd.IsSet("addr") {
		cfg.HTTPAddr = cmd.String("addr")
	}
	if cmd.IsSet("db") {
		cfg.DBPath = cmd.String("db")
	}
	if cmd.IsSet("admin-username") {
		cfg.AdminUsername = cmd.String("admin-username")
	}
	if cmd.IsSet("dev") {
		cfg.DevMode = cmd.Bool("dev")
	}
	if cmd.IsSet("no-csrf") {
		cfg.CSRFEnabled = !cmd.Bool("no-csrf")
	}
}
