package arksftp

import (
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// ssh_config files, in the order OpenSSH reads them. The user file wins.
var sshConfigFiles = []string{
	filepath.Join("~", ".ssh", "config"),
	filepath.Join("/etc", "ssh", "ssh_config"),
}

// sshConfig looks up ssh_config keywords for a Host alias.
type sshConfig interface {
	Get(alias, key string) string
}

// sshFiles queries several parsed ssh_config files in order, so the user file
// shadows the system one, matching OpenSSH.
type sshFiles []*ssh_config.Config

// Get returns the first non-empty value for key under alias.
func (f sshFiles) Get(alias, key string) string {
	for _, c := range f {
		if v, err := c.Get(alias, key); err == nil && v != "" {
			return v
		}
	}
	return ""
}

// loadSSHConfig parses the user and system ssh_config files. A file that is
// missing or unparsable is skipped: ssh_config is a convenience, and failing
// the connect over a stray line in an unrelated stanza would be wrong.
func loadSSHConfig() sshConfig {
	var files sshFiles
	for _, name := range sshConfigFiles {
		path := expandHome(name)
		f, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Debug("arksftp: skipping ssh_config", "path", path, "err", err)
			}
			continue
		}
		cfg, err := ssh_config.Decode(f)
		_ = f.Close()
		if err != nil {
			slog.Debug("arksftp: skipping unparsable ssh_config", "path", path, "err", err)
			continue
		}
		files = append(files, cfg)
	}
	return files
}

// resolve fills the blanks in cfg from ssh_config, then from the defaults.
// Precedence is explicit Config fields, then ssh_config, then the defaults:
// port 22 and the current OS user. cfg.Host is treated as the Host alias, so
// a HostName in the matching stanza replaces it.
func resolve(cfg Config, sc sshConfig) Config {
	alias := cfg.Host

	//nolint:nestif // flat field defaults
	if sc != nil {
		if cfg.User == "" {
			cfg.User = sc.Get(alias, "User")
		}
		if cfg.Port == 0 {
			if port, err := strconv.Atoi(sc.Get(alias, "Port")); err == nil {
				cfg.Port = port
			}
		}
		if cfg.IdentityFile == "" {
			cfg.IdentityFile = sc.Get(alias, "IdentityFile")
		}
		if hostName := sc.Get(alias, "HostName"); hostName != "" {
			cfg.Host = hostName
		}
	}

	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.User == "" {
		cfg.User = currentUser()
	}
	if cfg.IdentityFile != "" {
		cfg.IdentityFile = expandTokens(cfg.IdentityFile, cfg)
	}

	return cfg
}

// currentUser returns the OS user name, the default ssh uses when no user is
// given. An empty result lets the ssh handshake report the missing user.
func currentUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// expandTokens expands the ssh_config percent tokens arksftp can resolve, plus
// a leading ~. Unknown tokens are left alone.
func expandTokens(s string, cfg Config) string {
	s = strings.NewReplacer(
		"%h", cfg.Host,
		"%p", strconv.Itoa(cfg.Port),
		"%r", cfg.User,
		"%%", "%",
	).Replace(s)
	return expandHome(s)
}

// expandHome replaces a leading ~ with the home directory.
func expandHome(s string) string {
	if s != "~" && !strings.HasPrefix(s, "~"+string(filepath.Separator)) {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, strings.TrimPrefix(s, "~"))
}

// decodeSSHConfig parses one ssh_config from r. Used by tests to stay off the
// developer's own ~/.ssh/config.
func decodeSSHConfig(r io.Reader) (sshConfig, error) {
	cfg, err := ssh_config.Decode(r)
	if err != nil {
		return nil, err
	}
	return sshFiles{cfg}, nil
}
