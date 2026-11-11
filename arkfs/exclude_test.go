package arkfs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
)

func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func walkPaths(t *testing.T, src ark.Source) ([]string, error) {
	t.Helper()
	var got []string
	err := src.Walk(context.Background(), func(e ark.FileInfo, _ func() (io.ReadCloser, error)) error {
		got = append(got, e.Path)
		return nil
	})
	return got, err
}

func TestWithExcludeLeavesOutMatchedPaths(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"top.log", "q1.txt", "q22.txt",
		"src/main.go", "src/main.log", "src/lib/util.go", "src/lib/deep/x.log",
		"cache/big.bin", "cache/sub/blob", "docs/cache/note.md",
	)
	all := []string{
		"cache", "cache/big.bin", "cache/sub", "cache/sub/blob",
		"docs", "docs/cache", "docs/cache/note.md",
		"q1.txt", "q22.txt",
		"src", "src/lib", "src/lib/deep", "src/lib/deep/x.log", "src/lib/util.go",
		"src/main.go", "src/main.log",
		"top.log",
	}

	tests := []struct {
		name     string
		pattern  string
		excluded []string
	}{
		{"star within a component", "src/*.go", []string{"src/main.go"}},
		{"question mark within a component", "q?.txt", []string{"q1.txt"}},
		{"double star across components", "src/**/*.log", []string{"src/main.log", "src/lib/deep/x.log"}},
		{
			"leading slash anchors at the root",
			"/cache",
			[]string{"cache", "cache/big.bin", "cache/sub", "cache/sub/blob"},
		},
		{"no slash matches at any depth", "*.log", []string{"top.log", "src/main.log", "src/lib/deep/x.log"}},
		{
			"matched directory drops its subtree",
			"lib",
			[]string{"src/lib", "src/lib/deep", "src/lib/deep/x.log", "src/lib/util.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := arkfs.NewSource(root, arkfs.WithExclude(tc.pattern))
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			got, err := walkPaths(t, src)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			want := slices.DeleteFunc(slices.Clone(all), func(p string) bool {
				return slices.Contains(tc.excluded, p)
			})
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("walk yielded %q, want %q", got, want)
			}
		})
	}
}

func TestExcludedDirectoryIsNeverRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 directory")
	}
	root := t.TempDir()
	writeTree(t, root, "keep.txt", "secret/inside.txt")
	secret := filepath.Join(root, "secret")
	if err := os.Chmod(secret, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })

	plain, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := walkPaths(t, plain); err == nil {
		t.Fatal("walk without excludes read the mode 000 directory; the test proves nothing")
	}

	src, err := arkfs.NewSource(root, arkfs.WithExclude("secret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := walkPaths(t, src)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !slices.Equal(got, []string{"keep.txt"}) {
		t.Errorf("walk yielded %q, want [keep.txt]", got)
	}
}

func TestWithExcludeFile(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	root := t.TempDir()
	writeTree(t, root, "a", "b", "keep.txt", "x.log", "$HOME", "cache/c", "#x")

	excludes := filepath.Join(t.TempDir(), "excludes")
	content := "  # a comment\n*.log  \n\n\t/cache\n$HOME\n\\#x"
	if err := os.WriteFile(excludes, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := arkfs.NewSource(root,
		arkfs.WithExclude("a"), arkfs.WithExcludeFile(excludes), arkfs.WithExclude("b", "a"))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	want := []string{"a", "*.log", "/cache", "$HOME", `\#x`, "b", "a"}
	if got := src.Meta().Excludes; !slices.Equal(got, want) {
		t.Errorf("Meta().Excludes = %q, want %q", got, want)
	}
	got, err := walkPaths(t, src)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !slices.Equal(got, []string{"keep.txt"}) {
		t.Errorf("walk yielded %q, want [keep.txt]", got)
	}

	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	stored, err := repo.Snapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !slices.Equal(stored.Excludes, want) {
		t.Errorf("snapshot Excludes = %q, want %q", stored.Excludes, want)
	}
}

func TestNewSourceRejectsBadExcludes(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("ok\n[\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := arkfs.NewSource(root, arkfs.WithExcludeFile(missing)); err == nil ||
		!strings.Contains(err.Error(), missing) {
		t.Errorf("missing exclude file: err = %v, want one naming %s", err, missing)
	}
	for name, opt := range map[string]arkfs.SourceOption{
		"WithExclude":     arkfs.WithExclude("ok", "!neg"),
		"WithExcludeFile": arkfs.WithExcludeFile(bad),
	} {
		if _, err := arkfs.NewSource(root, opt); !errors.Is(err, path.ErrBadPattern) {
			t.Errorf("%s: err = %v, want path.ErrBadPattern", name, err)
		}
	}
}

// The parent snapshot holds only what the patterns left in, so a second
// backup with the same patterns takes every remaining file as unchanged; a
// hardlinked file whose first name is excluded is stored under the other.
func TestExcludedBackupStaysIncremental(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)
	root := t.TempDir()
	writeTree(t, root, "a.txt", "keep.txt", "skip.log", "dir/inner.txt")
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{"a.txt", "keep.txt", "skip.log", "dir/inner.txt"} {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(f)), old, old); err != nil {
			t.Fatal(err)
		}
	}

	backup := func() *ark.Snapshot {
		t.Helper()
		src, err := arkfs.NewSource(root, arkfs.WithExclude("a.txt", "*.log"))
		if err != nil {
			t.Fatalf("NewSource: %v", err)
		}
		snap, err := repo.Backup(ctx, src)
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		return snap
	}
	backup()
	snap := backup()
	// b.txt, keep.txt and dir/inner.txt
	if snap.Stats.FilesUnchanged != 3 {
		t.Errorf("FilesUnchanged = %d, want 3", snap.Stats.FilesUnchanged)
	}

	dst := t.TempDir()
	if _, err := arkfs.Restore(ctx, repo, snap.ID, dst); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "b.txt")); err != nil || string(got) != "a.txt" {
		t.Errorf("b.txt restored %q (%v), want %q", got, err, "a.txt")
	}
	for _, gone := range []string{"a.txt", "skip.log"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s restored (err %v), want it excluded", gone, err)
		}
	}
}
