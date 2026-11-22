package arkfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
)

// TestCopyFallbackSetsTheOwnerBeforeTheMode: chown clears setuid and setgid,
// so the copy that stands in for a hardlink sets the mode last, as Receive
// does for a regular file.
func TestCopyFallbackSetsTheOwnerBeforeTheMode(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mode := os.ModeSetuid | 0o755
	if err := copyFile(src, dst, ark.FileInfo{Mode: mode, UID: os.Getuid(), GID: os.Getgid()}); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got := fi.Mode() &^ os.ModeType; got != mode {
		t.Errorf("the copy has mode %v, want %v", got, mode)
	}
}

// TestSinkDropsSetuidAndSetgidWithoutTheOwner: an unprivileged restore cannot
// give an entry to another user, and a setuid file left behind would run as
// whoever restored it. The sticky bit names no owner and stays, and a
// directory keeps all three: nothing runs as it.
func TestSinkDropsSetuidAndSetgidWithoutTheOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can set any owner")
	}
	ctx := context.Background()
	root := t.TempDir()
	sink, err := NewSink(root)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := sink.Open(ctx, ark.SourceMeta{Type: ark.SourceTypeFile}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	special := os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	for _, e := range []ark.FileInfo{
		{Path: "dir", Type: ark.FileTypeDirectory, Mode: special | 0o775},
		{Path: "file", Type: ark.FileTypeRegular, Mode: os.ModeSetuid | os.ModeSetgid | 0o755},
	} {
		if err := sink.Receive(ctx, e, nil); err != nil {
			t.Fatalf("Receive(%s): %v", e.Path, err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, want := range map[string]os.FileMode{"dir": special | 0o775, "file": 0o755} {
		fi, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if got := fi.Mode() &^ os.ModeType; got != want {
			t.Errorf("%s of root restored with mode %v, want %v", name, got, want)
		}
	}
}

// TestSinkKeepsTheXattrsOfAReadOnlyFile: setxattr needs write permission, so
// the mode comes after the xattrs.
func TestSinkKeepsTheXattrsOfAReadOnlyFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes xattrs to a read-only file")
	}
	root := t.TempDir()
	probe := filepath.Join(root, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(probe, "user.k", []byte("v"), 0); err != nil {
		t.Skipf("the file system takes no user xattrs: %v", err)
	}
	f := receiveAll(t, root, ark.FileInfo{Path: "f", Type: ark.FileTypeRegular, Mode: 0o444,
		Xattrs: map[string][]byte{"user.k": []byte("v")}})
	buf := make([]byte, 8)
	n, err := unix.Getxattr(f["f"], "user.k", buf)
	if err != nil || string(buf[:n]) != "v" {
		t.Errorf("user.k of a 0444 file = %q, %v, want v", buf[:n], err)
	}
}

// TestSinkReplacesWhatRootHolds: a symlink left in root is replaced, never
// followed, whether a directory or a file is restored at its path.
func TestSinkReplacesWhatRootHolds(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, name := range []string{"dir", "file"} {
		if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	receiveAll(t, root,
		ark.FileInfo{Path: "dir", Type: ark.FileTypeDirectory, Mode: 0o755},
		ark.FileInfo{Path: "dir/x", Type: ark.FileTypeRegular, Mode: 0o644},
		ark.FileInfo{Path: "file", Type: ark.FileTypeRegular, Mode: 0o644})
	if ents, err := os.ReadDir(outside); err != nil || len(ents) > 0 {
		t.Fatalf("the restore wrote %d entries through a symlink in root: %v", len(ents), err)
	}
	for _, name := range []string{"dir", "file"} {
		if fi, err := os.Lstat(filepath.Join(root, name)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is still a symlink: %v", name, err)
		}
	}
}

// receiveAll restores entries into root with a fresh sink and returns their
// paths on disk.
func receiveAll(t *testing.T, root string, entries ...ark.FileInfo) map[string]string {
	t.Helper()
	ctx := context.Background()
	sink, err := NewSink(root)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := sink.Open(ctx, ark.SourceMeta{Type: ark.SourceTypeFile}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	paths := map[string]string{}
	for _, e := range entries {
		if err := sink.Receive(ctx, e, nil); err != nil {
			t.Fatalf("Receive(%s): %v", e.Path, err)
		}
		paths[e.Path] = filepath.Join(root, e.Path)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return paths
}

// TestRefusedNode: the errnos a filesystem without fifos or device nodes
// returns skip the entry; any other error still fails the restore.
func TestRefusedNode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{unix.EPERM, true},
		{unix.EOPNOTSUPP, true},
		{unix.ENOTSUP, true},
		{&os.PathError{Op: "mkfifo", Path: "p", Err: unix.EPERM}, true},
		{unix.EACCES, false},
		{unix.ENOSPC, false},
	} {
		if got := refusedNode(tc.err); got != tc.want {
			t.Errorf("refusedNode(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestMakeNodeRefusesADeviceNumberMknodCannotTake: mknod would cut the
// number to 32 bits and make another device.
func TestMakeNodeRefusesADeviceNumberMknodCannotTake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	err := makeNode(path, ark.FileInfo{Type: ark.FileTypeCharDevice, Mode: 0o600, DevMajor: 5000, DevMinor: 1})
	if !errors.Is(err, ark.ErrEntrySkipped) {
		t.Fatalf("makeNode = %v, want ErrEntrySkipped", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("a node is on disk: %v", err)
	}
}
