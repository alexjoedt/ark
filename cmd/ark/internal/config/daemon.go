package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// EnvHTTPAddr is the environment variable for overriding the HTTP bind address.
	EnvHTTPAddr = "ARK_HTTP_ADDR"

	// EnvDBPath is the environment variable for overriding the SQLite database path.
	EnvDBPath = "ARK_DB_PATH"

	// EnvAdminUsername is the environment variable for the admin username.
	EnvAdminUsername = "ARK_ADMIN_USERNAME"

	// EnvDevMode is the environment variable for enabling dev mode ("true" or "1").
	EnvDevMode = "ARK_DEV_MODE"

	// DefaultHTTPAddr is the default HTTP server bind address.
	DefaultHTTPAddr = ":8080"

	// DefaultAdminUsername is the default admin login username.
	DefaultAdminUsername = "admin"

	// DaemonConfigDir is the system-level configuration directory for the daemon.
	DaemonConfigDir = "/etc/ark"
)

// ServeConfig holds all configuration for `ark serve`.
//
// AdminPassword is intentionally absent — it must be supplied via the
// ARK_ADMIN_PASSWORD environment variable or the --admin-password CLI flag.
// Writing passwords to disk in a config file is not supported.
type ServeConfig struct {
	// RepoPath is the repository the daemon serves when --repo and
	// ARK_REPOSITORY are unset.
	RepoPath string `json:"repoPath"`

	// DockerHost is the Docker daemon address when --docker-host and
	// DOCKER_HOST are unset. Empty means the platform default.
	DockerHost string `json:"dockerHost"`

	// HTTPAddr is the address the HTTP server listens on (e.g. ":8080").
	HTTPAddr string `json:"httpAddr"`

	// DBPath is the absolute path to the SQLite database file.
	// When empty, defaults to $HOME/.ark/ark.db.
	DBPath string `json:"dbPath"`

	// AdminUsername is the login username for the admin account.
	AdminUsername string `json:"adminUsername"`

	// DevMode disables embedded static-file serving; the Vite dev server is
	// expected at http://localhost:5173 for the SPA.
	DevMode bool `json:"devMode"`

	// CSRFEnabled controls CSRF protection. Should always be true in production.
	CSRFEnabled bool `json:"csrfEnabled"`
}

// DaemonConfigPath returns the system-level daemon config path (/etc/ark/daemon.json).
func DaemonConfigPath() string {
	return filepath.Join(DaemonConfigDir, "daemon.json")
}

// LoadServe builds a fully resolved ServeConfig.
//
// Resolution order (highest priority first):
//
//	CLI flags (applied by the caller via applyServeFlags after this call)
//	> env vars
//	> /etc/ark/daemon.json
//	> built-in defaults
func LoadServe(logFn func(msg string)) (*ServeConfig, error) {
	cfg := &ServeConfig{
		HTTPAddr:      DefaultHTTPAddr,
		AdminUsername: DefaultAdminUsername,
		CSRFEnabled:   true,
	}

	// Layer in /etc/ark/daemon.json when present.
	if data, err := os.ReadFile(DaemonConfigPath()); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("daemon config %s: %w", DaemonConfigPath(), err)
		}
		if logFn != nil {
			logFn("loaded " + DaemonConfigPath())
		}
	}

	// ARK_REPOSITORY and DOCKER_HOST are flag sources, main applies them
	// after this call. Only the serve-specific variables are read here.
	if v := os.Getenv(EnvHTTPAddr); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv(EnvDBPath); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv(EnvAdminUsername); v != "" {
		cfg.AdminUsername = v
	}
	if v := os.Getenv(EnvDevMode); v == "true" || v == "1" {
		cfg.DevMode = true
	}

	// Derive DBPath from the ark home directory when not explicitly configured.
	// The database always lives in $HOME/.ark/, independent of RepoPath.
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath()
	}

	return cfg, nil
}

// SaveDaemon writes cfg to /etc/ark/daemon.json, creating parent directories
// as needed. The file is written with mode 0640.
func SaveDaemon(cfg *ServeConfig) error {
	path := DaemonConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("failed to create daemon config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal daemon config: %w", err)
	}

	//nolint:gosec // group readable by design
	if err := os.WriteFile(path, data, 0640); err != nil {
		return fmt.Errorf("failed to write daemon config: %w", err)
	}
	return nil
}
