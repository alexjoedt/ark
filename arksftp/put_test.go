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
	"slices"
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
// requests the server receives and can fail renames of files with a status that
// rename does not retry.
type cmdRecorder struct {
	sftp.FileCmder
	list       sftp.FileLister
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
		if c.failRename && !isDir(c.list, r.Filepath) {
			return sftp.ErrSSHFxPermissionDenied
		}
	}
	return c.FileCmder.Filecmd(r)
}

// isDir reports whether p is a directory on the in-memory server of l.
func isDir(l sftp.FileLister, p string) bool {
	la, err := l.Filelist(sftp.NewRequest("Stat", p))
	if err != nil {
		return false
	}
	fi := make([]os.FileInfo, 1)
	n, _ := la.ListAt(fi, 0)
	return n == 1 && fi[0].IsDir()
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
		rec := &cmdRecorder{FileCmder: hs.FileCmd, list: hs.FileList}
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
	lock := func(dir string) string {
		t.Helper()
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		return locked
	}
	opens := 0
	syncFile = func(*sftp.File) error { opens++; return nil }

	// The server opened a directory above, so it supports that: a refusal
	// inside the repository is a failure, one above it skips that directory.
	if err := b.syncDir(lock(dir)); err == nil {
		t.Error("a refused open in the repository was swallowed after a directory had opened")
	}
	if err := b.syncDir(lock(t.TempDir())); err != nil {
		t.Errorf("syncDir on an ancestor the server refuses: %v", err)
	}
	if b.noDirSync.Load() || logs.Len() != 0 {
		t.Errorf("noDirSync %v, want directory syncs on and no warning:\n%s", b.noDirSync.Load(), logs.String())
	}
	if err := b.syncDir(dir); err != nil || opens != 1 {
		t.Errorf("syncDir after the refusals = %v with %d fsyncs, want nil and 1", err, opens)
	}

	// A server that refuses the first directory of the repository does not
	// support directory syncs.
	b = pipeBackend(t)
	b.fsync = true
	locked := lock(b.basePath)
	opens = 0
	if err := b.syncDir(lock(t.TempDir())); err != nil || b.noDirSync.Load() {
		t.Fatalf("an ancestor refused first = %v, noDirSync %v, want nil and syncs on", err, b.noDirSync.Load())
	}
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
	if err := b.syncDir(b.basePath); err != nil || opens != 0 {
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

func TestPutCreatesPrivateDirs(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	top := b.basePath
	b.basePath = filepath.Join(top, "new", "repo")
	// The in-process server makes a directory 0755 under the umask.
	ref := filepath.Join(top, "ref")
	if err := os.Mkdir(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []ark.Handle{
		{Type: ark.KeyFile, Name: "k1"},
		{Type: ark.DataPackFile, Name: "abcdef0123456789"},
	} {
		if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
			t.Fatalf("Put %v: %v", h, err)
		}
	}
	mode := func(p string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	if got, want := mode(filepath.Join(top, "new")), mode(ref); got != want {
		t.Errorf("parent above the repository mode %o, want %o from the umask", got, want)
	}
	for _, d := range []string{".", "keys", "datapacks", "datapacks/ab"} {
		if got := mode(filepath.Join(b.basePath, d)); got != 0o700 {
			t.Errorf("%s mode %o, want 700", d, got)
		}
	}
	for _, d := range []string{"new", "new/repo", "new/repo/datapacks"} {
		if temps, _ := filepath.Glob(filepath.Join(top, d, tmpPrefix+"*")); len(temps) > 0 {
			t.Errorf("temporary dirs left in %s: %v", d, temps)
		}
	}
	old := filepath.Join(b.basePath, "locks")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, ark.Handle{Type: ark.LockFile, Name: "l1"}, strings.NewReader("x")); err != nil {
		t.Fatalf("Put lock: %v", err)
	}
	if got := mode(old); got != 0o755 {
		t.Errorf("existing locks dir mode %o, want 755 untouched", got)
	}
}

type cmdFunc func(*sftp.Request) error

func (f cmdFunc) Filecmd(r *sftp.Request) error { return f(r) }

type listFunc func(*sftp.Request) (sftp.ListerAt, error)

func (f listFunc) Filelist(r *sftp.Request) (sftp.ListerAt, error) { return f(r) }

// memBackend serves the in-memory handlers hs at /repo.
func memBackend(t *testing.T, hs sftp.Handlers) *Backend {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	b, _ := stallOver(t, sftp.NewRequestServer(srvConn, hs), srvConn, cliConn, "/repo")
	return b
}

// TestPutOnARestrictedServer pins a server that refuses chmod with
// PERMISSION_DENIED, which pkg/sftp turns into [os.ErrPermission], and reports a
// missing path with SSH_FX_FAILURE instead of NO_SUCH_FILE: Put creates the
// directories anyway and warns once.
func TestPutOnARestrictedServer(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	hs := sftp.InMemHandler()
	cmd, list := hs.FileCmd, hs.FileList
	hs.FileCmd = cmdFunc(func(r *sftp.Request) error {
		if r.Method == "Setstat" {
			return os.ErrPermission
		}
		return cmd.Filecmd(r)
	})
	hs.FileList = listFunc(func(r *sftp.Request) (sftp.ListerAt, error) {
		l, err := list.Filelist(r)
		if r.Method == "Stat" && errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("failure")
		}
		return l, err
	})
	b := memBackend(t, hs)
	for _, name := range []string{"aa01", "bb01"} {
		h := ark.Handle{Type: ark.DataPackFile, Name: name}
		if err := b.Put(t.Context(), h, strings.NewReader("x")); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}

// TestPutMakesADirAnotherPutCreatedPrivate pins that a Put losing the race to
// create a directory still chmods it: the winner may be an older ark.
func TestPutMakesADirAnotherPutCreatedPrivate(t *testing.T) {
	hs := sftp.InMemHandler()
	cmd := hs.FileCmd
	var chmodded []string
	hs.FileCmd = cmdFunc(func(r *sftp.Request) error {
		switch {
		case r.Method == "Setstat":
			chmodded = append(chmodded, r.Filepath)
			return nil
		case r.Method == "Rename" && r.Target == "/repo/datapacks":
			for _, d := range []string{"/repo/datapacks", "/repo/datapacks/zz"} {
				if err := cmd.Filecmd(sftp.NewRequest("Mkdir", d)); err != nil {
					return err
				}
			}
		}
		return cmd.Filecmd(r)
	})
	b := memBackend(t, hs)
	if err := b.Put(t.Context(), ark.Handle{Type: ark.DataPackFile, Name: "aa01"}, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !slices.Contains(chmodded, "/repo/datapacks") {
		t.Errorf("chmodded %v, want /repo/datapacks among them", chmodded)
	}
	entries, err := b.client.ReadDir("/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temporary dir %s left in /repo", e.Name())
		}
	}
}
