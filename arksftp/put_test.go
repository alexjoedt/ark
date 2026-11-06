package arksftp

import (
	"context"
	"net"
	"path"
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

func TestPutAndDeleteSyncOnlyWhenAnnounced(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	var synced []string
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	// Records each synced path relative to the base, temp files as "tmp".
	// Ancestors above the parent of the base depend on t.TempDir.
	syncFile = func(f *sftp.File) error {
		switch name := f.Name(); {
		case strings.HasPrefix(path.Base(name), tmpPrefix):
			synced = append(synced, "tmp")
		case name == b.basePath:
			synced = append(synced, ".")
		case name == path.Dir(b.basePath):
			synced = append(synced, "..")
		case strings.HasPrefix(name, b.basePath+"/"):
			synced = append(synced, strings.TrimPrefix(name, b.basePath+"/"))
		}
		return nil
	}
	h := ark.Handle{Type: ark.BlobFile, Name: "abcdef0123456789"}

	if b.fsync {
		t.Fatal("in-process server announced fsync@openssh.com; test needs a server without it")
	}
	if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
		t.Fatalf("Put without fsync extension: %v", err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("Delete without fsync extension: %v", err)
	}
	if len(synced) != 0 {
		t.Errorf("synced %v without the extension, want none", synced)
	}

	b.fsync = true
	steps := []struct {
		name string
		run  func() error
		want []string
	}{
		{"first Put", func() error { return b.Put(ctx, h, strings.NewReader("y")) },
			[]string{"chunks/ab", "chunks", ".", "..", "tmp", "chunks/ab/cd"}},
		{"Put into known dir", func() error { return b.Put(ctx, h, strings.NewReader("y")) },
			[]string{"tmp", "chunks/ab/cd"}},
		{"Put after its shard dir was removed", func() error {
			if err := b.client.RemoveAll(path.Join(b.basePath, "chunks", "ab")); err != nil {
				return err
			}
			return b.Put(ctx, h, strings.NewReader("y"))
		}, []string{"chunks/ab", "chunks", ".", "..", "tmp", "chunks/ab/cd"}},
		{"Delete", func() error { return b.Delete(ctx, h) }, []string{"chunks/ab/cd"}},
		{"Delete missing", func() error { return b.Delete(ctx, h) }, []string{"chunks/ab/cd"}},
		{"Delete in missing namespace", func() error {
			return b.Delete(ctx, ark.Handle{Type: ark.TreeFile, Name: "t1"})
		}, nil},
	}
	for _, s := range steps {
		synced = nil
		if err := s.run(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if strings.Join(synced, " ") != strings.Join(s.want, " ") {
			t.Errorf("%s synced %v, want %v", s.name, synced, s.want)
		}
	}
}
