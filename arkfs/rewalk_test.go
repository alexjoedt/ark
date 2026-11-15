package arkfs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
)

func TestRewalkYieldsWalkEntriesFromTheStatAlone(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "a/one.txt", "a/skip.tmp", "b/two.txt", "drop/big.bin")
	if err := os.Link(filepath.Join(root, "a/one.txt"), filepath.Join(root, "b/link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("one.txt", filepath.Join(root, "a/sym")); err != nil {
		t.Fatal(err)
	}
	xattrs := unix.Setxattr(filepath.Join(root, "b/two.txt"), "user.ark", []byte("x"), 0) == nil
	src, err := arkfs.NewSource(root, arkfs.WithExclude("*.tmp", "drop"))
	if err != nil {
		t.Fatal(err)
	}

	type seen struct {
		path, link string
		typ        ark.FileType
		size       int64
	}
	var walked []seen
	sawXattrs := false
	if err := src.Walk(context.Background(), func(e ark.FileInfo, _ func() (io.ReadCloser, error)) error {
		sawXattrs = sawXattrs || len(e.Xattrs) > 0
		link := e.LinkDest
		if e.Type == ark.FileTypeSymlink {
			link = ""
		}
		walked = append(walked, seen{e.Path, link, e.Type, e.Size})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if xattrs && !sawXattrs {
		t.Fatal("Walk yielded no xattrs for a file that has one")
	}

	var rewalked []seen
	if err := src.(ark.RewalkSource).Rewalk(context.Background(), func(e ark.FileInfo) error {
		if len(e.Xattrs) > 0 || e.Type == ark.FileTypeSymlink && e.LinkDest != "" {
			t.Errorf("Rewalk read past the stat for %s", e.Path)
		}
		rewalked = append(rewalked, seen{e.Path, e.LinkDest, e.Type, e.Size})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rewalked) != len(walked) {
		t.Fatalf("Rewalk yielded %v, Walk %v", rewalked, walked)
	}
	for i := range walked {
		if rewalked[i] != walked[i] {
			t.Errorf("entry %d: Rewalk %+v, Walk %+v", i, rewalked[i], walked[i])
		}
	}
}
