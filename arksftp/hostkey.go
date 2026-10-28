package arksftp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeys returns the callback that verifies the server host key. It reads
// KnownHostsFile, defaulting to ~/.ssh/known_hosts. A missing or unreadable
// file is an error: falling back to accepting any key would defeat the point.
func hostKeys(cfg Config) (ssh.HostKeyCallback, error) {
	if cfg.InsecureIgnoreHostKey {
		slog.Warn("arksftp: host key verification disabled, the connection is open to interception",
			"host", cfg.Host)
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit opt-in
	}

	file := cfg.KnownHostsFile
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("arksftp: locate ~/.ssh/known_hosts: %w", err)
		}
		file = filepath.Join(home, ".ssh", "known_hosts")
	}

	verify, err := knownhosts.New(file)
	if err != nil {
		return nil, fmt.Errorf("arksftp: load known_hosts %s: %w", file, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) == 0 {
			return unknownHostError(hostname, file, key)
		}
		return mismatchError(hostname, keyErr, key)
	}, nil
}

// unknownHostError reports a host that has no entry in the known_hosts file,
// with the command that adds one.
func unknownHostError(hostname, file string, key ssh.PublicKey) error {
	host, port := splitHostPort(hostname)
	return fmt.Errorf("arksftp: host key for %s is not in %s\n"+
		"  offered %s %s\n"+
		"  add it with: ssh-keyscan -p %s -H %s >> %s",
		hostname, file,
		key.Type(), ssh.FingerprintSHA256(key),
		port, host, file)
}

// mismatchError reports a host key that differs from the stored one. Either
// the server was rebuilt or the connection is intercepted, so the caller has
// to resolve it by hand.
func mismatchError(hostname string, keyErr *knownhosts.KeyError, key ssh.PublicKey) error {
	known := keyErr.Want[0]
	return fmt.Errorf("arksftp: host key mismatch for %s\n"+
		"  offered %s %s\n"+
		"  known   %s %s (%s:%d)\n"+
		"  remove the stale line only if you know the host key changed",
		hostname,
		key.Type(), ssh.FingerprintSHA256(key),
		known.Key.Type(), ssh.FingerprintSHA256(known.Key), known.Filename, known.Line)
}

// splitHostPort splits the "host:port" form ssh hands the callback. A bare
// host, or the bracketed [host]:port form of a non-default port, both come
// back with a usable port for the ssh-keyscan hint.
func splitHostPort(hostname string) (host, port string) {
	h, p, err := net.SplitHostPort(hostname)
	if err != nil {
		return strings.Trim(hostname, "[]"), "22"
	}
	return h, p
}
