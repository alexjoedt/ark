package arksftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
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
	h := ark.Handle{Type: ark.DataPackFile, Name: "abcdef0123456789"}

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
			[]string{"datapacks", ".", "..", "tmp", "datapacks/ab"}},
		{"Put into known dir", func() error { return b.Put(ctx, h, strings.NewReader("y")) },
			[]string{"tmp", "datapacks/ab"}},
		{"Put after its shard dir was removed", func() error {
			if err := b.client.RemoveAll(path.Join(b.basePath, "datapacks", "ab")); err != nil {
				return err
			}
			return b.Put(ctx, h, strings.NewReader("y"))
		}, []string{"datapacks", ".", "..", "tmp", "datapacks/ab"}},
		{"Delete", func() error { return b.Delete(ctx, h) }, []string{"datapacks/ab"}},
		{"Delete missing", func() error { return b.Delete(ctx, h) }, []string{"datapacks/ab"}},
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

// cmdRecorder wraps the in-memory command handler: it records the remove
// requests the server receives and can fail renames with a status that rename
// does not retry.
type cmdRecorder struct {
	sftp.FileCmder
	mu         sync.Mutex
	removed    []string
	failRename bool
}

func (c *cmdRecorder) Filecmd(r *sftp.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.Method {
	case "Remove":
		c.removed = append(c.removed, r.Filepath)
	case "Rename", "PosixRename":
		if c.failRename {
			return sftp.ErrSSHFxPermissionDenied
		}
	}
	return c.FileCmder.Filecmd(r)
}

func (c *cmdRecorder) takeRemoved() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.removed
	c.removed = nil
	return r
}

func TestPutRemovesTheTempFileOnlyOnFailure(t *testing.T) {
	ctx := context.Background()
	h := ark.Handle{Type: ark.DataPackFile, Name: "abcdef0123456789"}

	newBackend := func(t *testing.T, fsync, posixRename bool) (*Backend, *cmdRecorder) {
		t.Helper()
		srvConn, cliConn := net.Pipe()
		hs := sftp.InMemHandler()
		rec := &cmdRecorder{FileCmder: hs.FileCmd}
		hs.FileCmd = rec
		srv := sftp.NewRequestServer(srvConn, hs)
		go func() { _ = srv.Serve() }()
		client, err := sftp.NewClientPipe(cliConn, cliConn)
		if err != nil {
			t.Fatalf("NewClientPipe: %v", err)
		}
		t.Cleanup(func() { _ = client.Close(); _ = srv.Close() })
		return &Backend{client: client, basePath: "/repo", fsync: fsync, posixRename: posixRename}, rec
	}

	for _, fsync := range []bool{false, true} {
		for _, posix := range []bool{false, true} {
			t.Run(fmt.Sprintf("success fsync=%v posix=%v", fsync, posix), func(t *testing.T) {
				orig := syncFile
				t.Cleanup(func() { syncFile = orig })
				syncFile = func(*sftp.File) error { return nil }
				b, rec := newBackend(t, fsync, posix)
				if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if got := rec.takeRemoved(); len(got) != 0 {
					t.Errorf("successful Put sent remove requests for %v, want none", got)
				}
			})
		}
	}

	failures := []struct {
		name, errPrefix string
		fsync, posix    bool
	}{
		{"write", "arksftp: write ", false, false},
		{"sync", "arksftp: sync ", true, false},
		{"rename", "arksftp: rename ", false, false},
		{"posix rename", "arksftp: rename ", false, true},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			orig := syncFile
			t.Cleanup(func() { syncFile = orig })
			syncFile = func(*sftp.File) error { return nil }
			b, rec := newBackend(t, f.fsync, f.posix)
			var r io.Reader = strings.NewReader("y")
			switch f.name {
			case "write":
				r = iotest.ErrReader(errors.New("read failed"))
			case "sync":
				syncFile = func(*sftp.File) error { return errors.New("sync failed") }
			default:
				rec.failRename = true
			}
			err := b.Put(ctx, h, r)
			if err == nil || !strings.HasPrefix(err.Error(), f.errPrefix) {
				t.Fatalf("Put error %v, want prefix %q", err, f.errPrefix)
			}
			got := rec.takeRemoved()
			if len(got) != 1 || !strings.HasPrefix(path.Base(got[0]), tmpPrefix) {
				t.Errorf("removed %v, want exactly the temp file", got)
			}
		})
	}
}

func TestSyncDirReportsAFailedFsyncAndTurnsOffAnUnsupportedOpen(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	b := pipeBackend(t)
	b.fsync = true
	dir := b.basePath

	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncFile = func(*sftp.File) error { return &sftp.StatusError{Code: 4} }
	if err := b.syncDir(dir); err == nil {
		t.Fatal("a failed fsync on an opened directory was swallowed")
	}
	if b.noDirSync.Load() {
		t.Fatal("a failed fsync turned directory syncs off")
	}

	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	opens := 0
	syncFile = func(*sftp.File) error { opens++; return nil }
	for range 2 {
		if err := b.syncDir(locked); err != nil {
			t.Fatalf("syncDir on a directory the server refuses: %v", err)
		}
	}
	if !b.noDirSync.Load() || opens != 0 {
		t.Errorf("noDirSync %v, fsyncs %d, want off and none", b.noDirSync.Load(), opens)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
	if err := b.syncDir(dir); err != nil || opens != 0 {
		t.Errorf("syncDir after the switch-off = %v with %d fsyncs, want nil and none", err, opens)
	}
}

func TestNewBackendWarnsOnceWithoutFsync(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	b := pipeBackend(t)
	nb := newBackend(b.client, nil, Config{Host: "nas", Path: "/repo"})
	if nb.fsync {
		t.Fatal("in-process server announced fsync@openssh.com; test needs a server without it")
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "host=nas") {
		t.Errorf("want one warning naming the host:\n%s", logs.String())
	}
	if err := nb.Sync(context.Background()); err != nil {
		t.Errorf("Sync without fsync = %v, want nil", err)
	}
}
