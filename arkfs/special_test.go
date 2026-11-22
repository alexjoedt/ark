package arkfs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
)

func TestFifoRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)

	src := t.TempDir()
	pipe := filepath.Join(src, "pipe")
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("fifos unsupported here: %v", err)
	}
	mtime := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chmod(pipe, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pipe, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	snap, err := arkfs.Backup(ctx, repo, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if snap.Stats.Skipped != 0 {
		t.Errorf("the backup skipped %d entries, want none", snap.Stats.Skipped)
	}
	dst := t.TempDir()
	res, err := arkfs.Restore(ctx, repo, snap.ID, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("the restore skipped %v", res.Skipped)
	}

	fi, err := os.Lstat(filepath.Join(dst, "pipe"))
	if err != nil {
		t.Fatalf("the fifo was not restored: %v", err)
	}
	if fi.Mode() != os.ModeNamedPipe|0o640 {
		t.Errorf("restored mode %v, want %v", fi.Mode(), os.ModeNamedPipe|0o640)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("restored mtime %v, want %v", fi.ModTime(), mtime)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("restored owner %d:%d, want %d:%d", st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
}

// canMknod reports whether this process may create a character device node.
func canMknod(t *testing.T) bool {
	t.Helper()
	err := unix.Mknod(filepath.Join(t.TempDir(), "probe"), unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3)))
	return err == nil
}

func TestDeviceRoundTrip(t *testing.T) {
	if !canMknod(t) {
		t.Skip("no privilege to create device nodes")
	}
	ctx := context.Background()
	repo := openRepo(t)

	src := t.TempDir()
	devices := map[string]struct {
		mode         uint32
		major, minor uint32
		goType       os.FileMode
	}{
		"null": {unix.S_IFCHR, 1, 3, os.ModeDevice | os.ModeCharDevice},
		"loop": {unix.S_IFBLK, 7, 5, os.ModeDevice},
		"high": {unix.S_IFCHR, 255, 1 << 19, os.ModeDevice | os.ModeCharDevice},
	}
	for name, d := range devices {
		if err := unix.Mknod(filepath.Join(src, name), d.mode|0o640, int(unix.Mkdev(d.major, d.minor))); err != nil {
			t.Fatalf("mknod %s: %v", name, err)
		}
	}

	snap, err := arkfs.Backup(ctx, repo, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	dst := t.TempDir()
	res, err := arkfs.Restore(ctx, repo, snap.ID, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("the restore skipped %v", res.Skipped)
	}
	for name, d := range devices {
		fi, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("%s was not restored: %v", name, err)
			continue
		}
		if fi.Mode() != d.goType|0o640 {
			t.Errorf("%s restored as %v, want %v", name, fi.Mode(), d.goType|0o640)
		}
		rdev := uint64(fi.Sys().(*syscall.Stat_t).Rdev) //nolint:unconvert // int32 on darwin
		if unix.Major(rdev) != d.major || unix.Minor(rdev) != d.minor {
			t.Errorf("%s restored as %d,%d, want %d,%d", name, unix.Major(rdev), unix.Minor(rdev), d.major, d.minor)
		}
	}
}

// deviceSource yields a file, a character device and a file after it, the
// snapshot a privileged backup of such a tree stores.
type deviceSource struct{}

func (deviceSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "devices", Type: ark.SourceTypeFile}
}

func (deviceSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	file := func(name string) error {
		return fn(ark.FileInfo{Path: name, Size: 4, Mode: 0o644, Type: ark.FileTypeRegular},
			func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("keep")), nil })
	}
	if err := file("a.txt"); err != nil {
		return err
	}
	dev := ark.FileInfo{Path: "null", Mode: 0o666, Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: 3}
	if err := fn(dev, nil); err != nil {
		return err
	}
	return file("z.txt")
}

func TestUnprivilegedRestoreSkipsDevices(t *testing.T) {
	if canMknod(t) {
		t.Skip("this process may create device nodes")
	}
	ctx := context.Background()
	repo := openRepo(t)
	snap, err := repo.Backup(ctx, deviceSource{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := t.TempDir()
	res, err := arkfs.Restore(ctx, repo, snap.ID, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Skipped) != 1 || !errors.Is(res.Skipped[0], ark.ErrEntrySkipped) ||
		!strings.Contains(res.Skipped[0].Error(), "null") {
		t.Errorf("Skipped = %v, want one ErrEntrySkipped naming null", res.Skipped)
	}
	if res.FilesRestored != 2 {
		t.Errorf("restored %d files, want 2", res.FilesRestored)
	}
	for _, name := range []string{"a.txt", "z.txt"} {
		if got, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(got) != "keep" {
			t.Errorf("%s restored %q (%v), want keep", name, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "null")); !os.IsNotExist(err) {
		t.Errorf("the device is on disk: %v", err)
	}
}
