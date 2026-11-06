package arksftp

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/pkg/sftp"
)

// pipeBackend serves t.TempDir() from an in-process SFTP server. The server
// announces no fsync@openssh.com, so b.fsync starts false like on such a host.
func pipeBackend(t *testing.T) *Backend {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	srv, err := sftp.NewServer(srvConn)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	client, err := sftp.NewClientPipe(cliConn, cliConn)
	if err != nil {
		t.Fatalf("NewClientPipe: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = srv.Close() })
	_, fsync := client.HasExtension("fsync@openssh.com")
	return &Backend{client: client, basePath: t.TempDir(), fsync: fsync}
}

func TestPutSyncsOnlyWhenAnnounced(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	var synced []string
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncFile = func(f *sftp.File) error {
		synced = append(synced, f.Name())
		return nil
	}
	h := ark.Handle{Type: ark.SnapshotFile, Name: "s1"}

	if b.fsync {
		t.Fatal("in-process server announced fsync@openssh.com; test needs a server without it")
	}
	if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
		t.Fatalf("Put without fsync extension: %v", err)
	}
	if len(synced) != 0 {
		t.Errorf("synced %v without the extension, want none", synced)
	}

	b.fsync = true
	if err := b.Put(ctx, h, strings.NewReader("y")); err != nil {
		t.Fatalf("Put with fsync extension: %v", err)
	}
	if len(synced) != 1 || !strings.Contains(synced[0], tmpPrefix) {
		t.Errorf("synced %v with the extension, want the temp file once", synced)
	}
}
