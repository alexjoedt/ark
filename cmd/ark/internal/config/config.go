// Package config holds what ark serve reads beyond the global flags: the
// daemon.json file and the serve environment variables. The CLI itself has no
// config file; flags, environment and --env-file are resolved by main.
package config

import (
	"os"
	"path/filepath"
)

// DefaultDBPath returns the default SQLite database path ($HOME/.ark/ark.db).
// The database always lives in the ark home directory, independent of the
// repository.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/var/lib/ark/ark.db"
	}
	return filepath.Join(home, ".ark", "ark.db")
}
