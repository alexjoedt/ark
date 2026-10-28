package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/joho/godotenv"
)

// LoadEnvFile reads a .env-style file and sets each entry as an environment
// variable. Existing env vars are NOT overwritten — real env vars always win.
// Uses godotenv, which handles KEY=value, quoted values, export prefixes, and
// comment/blank lines. Never logs values to protect secrets.
func LoadEnvFile(path string) error {
	// godotenv.Load does not overwrite existing env vars.
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("env-file: %w", err)
	}
	return nil
}

// PreloadEnvFile scans args for --env-file or --env-file=<path> and loads the
// referenced file before urfave/cli resolves its EnvVars sources. This must be
// called before app.Run so that [os.Setenv] calls are visible to flag resolution.
//
// If --env-file is not present, PreloadEnvFile is a no-op.
// If the flag is present but the file cannot be loaded, it returns an error.
func PreloadEnvFile(args []string) error {
	for i, arg := range args {
		// --env-file=path form
		if after, ok := strings.CutPrefix(arg, "--env-file="); ok {
			path := after
			return LoadEnvFile(path)
		}
		// --env-file path form
		if arg == "--env-file" {
			if i+1 >= len(args) {
				return errors.New("--env-file requires a file path argument")
			}
			return LoadEnvFile(args[i+1])
		}
	}
	return nil
}
