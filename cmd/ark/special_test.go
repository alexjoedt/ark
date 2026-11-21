package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
)

// deviceSource yields a file and a character device, what a privileged
// backup of a tree with a device node stores.
type deviceSource struct{}

func (deviceSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "devices", Type: ark.SourceTypeFile}
}

func (deviceSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	err := fn(ark.FileInfo{Path: "a.txt", Size: 4, Mode: 0o644, Type: ark.FileTypeRegular},
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("keep")), nil })
	if err != nil {
		return err
	}
	return fn(ark.FileInfo{Path: "null", Mode: 0o666, Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: 3}, nil)
}

func TestRestoreWarnsAboutASkippedDevice(t *testing.T) {
	// Not 0,0: Linux lets anyone make that one, an overlayfs whiteout.
	if unix.Mknod(filepath.Join(t.TempDir(), "probe"), unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))) == nil {
		t.Skip("this process may create device nodes")
	}
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(context.Background(), b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Backup(context.Background(), deviceSource{})
	r.Close()
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	code, _, stderr := runCaptureAll(t, "-r", repo, "restore", string(snap.ID), dest)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "warning: restore: skipped null:") {
		t.Errorf("stderr %q holds no warning about the device", stderr)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "keep" {
		t.Errorf("a.txt restored %q (%v), want keep", got, err)
	}
}
