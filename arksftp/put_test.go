package arksftp

import (
	"context"
	"errors"
	"io"
	"net"
	"path"
	"strings"
	"testing"
	"testing/iotest"

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

func TestPutRemovesTheTempFileOnlyOnFailure(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	var removed []string
	orig := removeFile
	t.Cleanup(func() { removeFile = orig })
	removeFile = func(c *sftp.Client, name string) error {
		removed = append(removed, name)
		return orig(c, name)
	}
	h := ark.Handle{Type: ark.BlobFile, Name: "abcdef0123456789"}

	if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("successful Put removed %v, want no remove request", removed)
	}

	for _, step := range []string{"write", "sync", "rename"} {
		removed = removed[:0]
		b.fsync = step == "sync"
		var r io.Reader = strings.NewReader("y")
		switch step {
		case "write":
			r = iotest.ErrReader(errors.New("read failed"))
		case "sync":
			syncFile = func(f *sftp.File) error {
				if strings.HasPrefix(path.Base(f.Name()), tmpPrefix) {
					return errors.New("sync failed")
				}
				return nil
			}
			t.Cleanup(func() { syncFile = (*sftp.File).Sync })
		case "rename":
			// the target is a non-empty directory, so the rename cannot succeed
			p, _ := b.remotePath(h)
			if err := b.client.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := b.client.MkdirAll(path.Join(p, "d")); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Put(ctx, h, r); err == nil {
			t.Fatalf("%s: Put succeeded, want an error", step)
		}
		if len(removed) != 1 || !strings.HasPrefix(path.Base(removed[0]), tmpPrefix) {
			t.Errorf("%s: removed %v, want exactly the temp file", step, removed)
		}
	}
}
