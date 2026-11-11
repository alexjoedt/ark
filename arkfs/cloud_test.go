package arkfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
)

func TestExcludeCloudFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"keep.txt", ".doc.pdf.icloud", "sub/.x.icloud", "sub/ok.txt",
		"cloud/a.txt", "cloud/deep/b.txt", "gone.txt", "icloud", ".icloud.txt",
		".icloud", "..icloud",
	} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	walk := func(opts ...SourceOption) []string {
		t.Helper()
		src, err := NewSource(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		fs, ok := src.(*fileSource)
		if !ok {
			t.Fatalf("NewSource returned %T", src)
		}
		fs.dataless = func(fi os.FileInfo) bool {
			return fi.Name() == "cloud" || fi.Name() == "gone.txt"
		}
		var got []string
		err = src.Walk(context.Background(), func(e ark.FileInfo, _ func() (io.ReadCloser, error)) error {
			got = append(got, e.Path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	all := []string{
		"..icloud", ".doc.pdf.icloud", ".icloud", ".icloud.txt", "cloud", "cloud/a.txt", "cloud/deep",
		"cloud/deep/b.txt", "gone.txt", "icloud", "keep.txt", "sub", "sub/.x.icloud", "sub/ok.txt",
	}
	if got := walk(); !slices.Equal(got, all) {
		t.Errorf("without option: %v", got)
	}
	want := []string{"..icloud", ".icloud", ".icloud.txt", "icloud", "keep.txt", "sub", "sub/ok.txt"}
	if got := walk(WithExcludeCloudFiles()); !slices.Equal(got, want) {
		t.Errorf("with option: %v, want %v", got, want)
	}
}
