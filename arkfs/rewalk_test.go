package arkfs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
)

func TestRewalkHonoursExcludes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "a/one.txt", "a/skip.tmp", "b/two.txt", "drop/big.bin")
	if err := os.Link(filepath.Join(root, "a/skip.tmp"), filepath.Join(root, "b/link.txt")); err != nil {
		t.Fatal(err)
	}
	src, err := arkfs.NewSource(root, arkfs.WithExclude("*.tmp", "drop"))
	if err != nil {
		t.Fatal(err)
	}

	type seen struct {
		path string
		typ  ark.FileType
	}
	var walked, rewalked []seen
	if err := src.Walk(context.Background(), func(e ark.FileInfo, _ func() (io.ReadCloser, error)) error {
		walked = append(walked, seen{e.Path, e.Type})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.(ark.RewalkSource).Rewalk(context.Background(), func(e ark.FileInfo) error {
		rewalked = append(rewalked, seen{e.Path, e.Type})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rewalked, walked) {
		t.Errorf("Rewalk yielded %v, Walk %v", rewalked, walked)
	}
	excluded := slices.ContainsFunc(walked, func(s seen) bool { return s.path == "a/skip.tmp" || s.path == "drop" })
	if excluded || !slices.Contains(walked, seen{"b/link.txt", ark.FileTypeRegular}) {
		t.Errorf("Walk did not apply the excludes: %v", walked)
	}
}
