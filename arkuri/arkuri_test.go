package arkuri

import (
	"context"
	"strings"
	"testing"

	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arksftp"
)

// openLocal opens s and asserts it produced a local backend rooted at want.
func openLocal(t *testing.T, s, want string) {
	t.Helper()
	b, err := Open(t.Context(), s)
	if err != nil {
		t.Fatalf("Open(%q): %v", s, err)
	}
	t.Cleanup(func() { _ = b.Close() })

	local, ok := b.(*arklocal.Backend)
	if !ok {
		t.Fatalf("Open(%q) returned %T, want *arklocal.Backend", s, b)
	}
	if local.BasePath() != want {
		t.Errorf("BasePath = %q, want %q", local.BasePath(), want)
	}
}

func TestOpenBarePath(t *testing.T) {
	dir := t.TempDir()
	openLocal(t, dir, dir)
}

func TestOpenLocalURI(t *testing.T) {
	dir := t.TempDir()
	openLocal(t, "local://"+dir, dir)
}

func TestOpenRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	openLocal(t, "repo", "repo")
}

// sftpCfg parses an sftp URI without connecting to anything.
func sftpCfg(t *testing.T, s string) arksftp.Config {
	t.Helper()
	scheme, u, err := parse(s)
	if err != nil {
		t.Fatalf("parse(%q): %v", s, err)
	}
	if scheme != SchemeSFTP {
		t.Fatalf("scheme = %q, want %q", scheme, SchemeSFTP)
	}
	cfg, err := sftpConfig(u)
	if err != nil {
		t.Fatalf("sftpConfig(%q): %v", s, err)
	}
	return cfg
}

func TestSFTPURICarriesTheConnection(t *testing.T) {
	cfg := sftpCfg(t, "sftp://ark@storage.example.com:2222/srv/ark")

	if cfg.Host != "storage.example.com" {
		t.Errorf("Host = %q", cfg.Host)
	}
	if cfg.User != "ark" {
		t.Errorf("User = %q", cfg.User)
	}
	if cfg.Port != 2222 {
		t.Errorf("Port = %d", cfg.Port)
	}
	if cfg.Path != "/srv/ark" {
		t.Errorf("Path = %q", cfg.Path)
	}
}

func TestSFTPURILeavesAliasFieldsToSSHConfig(t *testing.T) {
	cfg := sftpCfg(t, "sftp://backup/srv/ark")

	if cfg.Host != "backup" {
		t.Errorf("Host = %q, want the alias", cfg.Host)
	}
	if cfg.User != "" || cfg.Port != 0 {
		t.Errorf("User = %q, Port = %d, want both zero so ssh_config decides", cfg.User, cfg.Port)
	}
}

func TestSFTPURIQueryParameters(t *testing.T) {
	cfg := sftpCfg(t, "sftp://ark@host/srv/ark?known_hosts=/etc/ark/known_hosts&identity=/keys/id&insecure=1")

	if cfg.KnownHostsFile != "/etc/ark/known_hosts" {
		t.Errorf("KnownHostsFile = %q", cfg.KnownHostsFile)
	}
	if cfg.IdentityFile != "/keys/id" {
		t.Errorf("IdentityFile = %q", cfg.IdentityFile)
	}
	if !cfg.InsecureIgnoreHostKey {
		t.Error("InsecureIgnoreHostKey = false, want true")
	}
}

func TestOpenRejects(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{"unknown scheme", "s3://bucket/prefix", `unsupported scheme "s3"`},
		{"password in uri", "sftp://ark:hunter2@host/srv/ark", arksftp.EnvPassword},
		{"empty location", "", "empty repository location"},
		{"local with host", "local://host/srv/ark", "takes no host"},
		{"sftp without path", "sftp://host", "needs a repository path"},
		{"sftp without host", "sftp:///srv/ark", "needs a host"},
		{"bad insecure value", "sftp://host/srv/ark?insecure=maybe", "want 1 or 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Open(t.Context(), tc.uri)
			if err == nil {
				_ = b.Close()
				t.Fatalf("Open(%q) succeeded", tc.uri)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is missing %q:\n%v", tc.want, err)
			}
		})
	}
}

func TestOpenHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := Open(ctx, t.TempDir()); err == nil {
		t.Fatal("Open ignored a cancelled context")
	}
}
