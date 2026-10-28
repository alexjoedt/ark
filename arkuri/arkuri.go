// Package arkuri opens an [ark.Backend] from a single configuration string.
//
// It is a plain scheme switch, not a registry: it imports every backend it
// supports, so importing arkuri is what pulls those backends into a binary.
// Callers who construct backends directly do not need this package.
//
// Supported forms:
//
//	/srv/ark                            a local directory
//	local:///srv/ark                    the same, written as a URI
//	sftp://user@host:22/srv/ark         over SFTP
//
// Non-secret sftp options ride as query parameters: known_hosts, identity and
// insecure. Secrets never appear in a URI; see [arksftp] for the environment
// variables that carry them.
package arkuri

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arksftp"
)

// Supported URI schemes.
const (
	SchemeLocal = "local"
	SchemeSFTP  = "sftp"
)

// Open resolves s to a backend. A string with no scheme is a local path.
// The caller closes the returned backend.
func Open(ctx context.Context, s string) (ark.Backend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	scheme, u, err := parse(s)
	if err != nil {
		return nil, err
	}

	switch scheme {
	case SchemeLocal:
		dir, err := localPath(u, s)
		if err != nil {
			return nil, err
		}
		return arklocal.New(dir)
	case SchemeSFTP:
		cfg, err := sftpConfig(u)
		if err != nil {
			return nil, err
		}
		return arksftp.New(cfg)
	default:
		return nil, unsupportedScheme(scheme)
	}
}

// parse splits s into a scheme and, for URI forms, the parsed URL. A string
// that does not start with a known scheme is a local path and comes back with
// a nil URL.
func parse(s string) (string, *url.URL, error) {
	if s == "" {
		return "", nil, fmt.Errorf("arkuri: empty repository location, %s", supportedForms())
	}
	if !hasScheme(s) {
		return SchemeLocal, nil, nil
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", nil, fmt.Errorf("arkuri: parse %q: %w", s, err)
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return "", nil, fmt.Errorf("arkuri: %s://: a password must not appear in the URI, "+
				"set %s instead", u.Scheme, arksftp.EnvPassword)
		}
	}
	return u.Scheme, u, nil
}

// hasScheme reports whether s is a URI rather than a path. A Windows drive
// letter looks like a one-character scheme, so those stay paths.
func hasScheme(s string) bool {
	i := strings.Index(s, "://")
	return i > 1
}

// localPath returns the directory a local location points at. Without a URI,
// the whole string is the path.
func localPath(u *url.URL, s string) (string, error) {
	if u == nil {
		return s, nil
	}
	if u.Host != "" {
		return "", fmt.Errorf("arkuri: local:// takes no host, write local:///%s", strings.TrimPrefix(u.Path, "/"))
	}
	if u.Path == "" {
		return "", fmt.Errorf("arkuri: local:// needs a path, %s", supportedForms())
	}
	return u.Path, nil
}

// sftpConfig maps an sftp URI onto an [arksftp.Config]. The host may be an
// ssh_config alias, so a missing user or port is left zero for arksftp to
// resolve.
func sftpConfig(u *url.URL) (arksftp.Config, error) {
	if u.Hostname() == "" {
		return arksftp.Config{}, fmt.Errorf("arkuri: sftp:// needs a host, %s", supportedForms())
	}
	if u.Path == "" || u.Path == "/" {
		return arksftp.Config{}, fmt.Errorf("arkuri: sftp:// needs a repository path, %s", supportedForms())
	}

	cfg := arksftp.Config{
		Host: u.Hostname(),
		Path: u.Path,
	}
	if u.User != nil {
		cfg.User = u.User.Username()
	}
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return arksftp.Config{}, fmt.Errorf("arkuri: sftp:// port %q: %w", p, err)
		}
		cfg.Port = port
	}

	q := u.Query()
	cfg.KnownHostsFile = q.Get("known_hosts")
	cfg.IdentityFile = q.Get("identity")
	if raw := q.Get("insecure"); raw != "" {
		insecure, err := strconv.ParseBool(raw)
		if err != nil {
			return arksftp.Config{}, fmt.Errorf("arkuri: sftp:// insecure=%q: want 1 or 0", raw)
		}
		cfg.InsecureIgnoreHostKey = insecure
	}

	return cfg, nil
}

// unsupportedScheme reports a scheme arkuri does not know.
func unsupportedScheme(scheme string) error {
	return fmt.Errorf("arkuri: unsupported scheme %q, %s", scheme, supportedForms())
}

// supportedForms names the accepted spellings, for error messages.
func supportedForms() string {
	return "write /path, local:///path or sftp://user@host:port/path"
}
