package ark_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfind"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"golang.org/x/sys/unix"
)

// Names that differ only in bytes that are not valid UTF-8. Before the binary
// tree node both were stored as "caf�.txt" and restore wrote one file.
const (
	rawE9     = "caf\xe9.txt"
	rawE8     = "caf\xe8.txt"
	rawDir    = "d\xfe"
	rawTarget = "caf\xe9\xff"
	rawXattr  = "user.caf\xe9"
)

// rawTree fills a directory with the raw names above: two files, a directory
// holding a file, a symlink with a raw target and, when the filesystem takes
// user xattrs, a raw xattr name on one file.
func rawTree(t *testing.T) (string, bool) {
	t.Helper()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	write(rawE9, "e9")
	write(rawE8, "e8")
	if err := os.Mkdir(filepath.Join(root, rawDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(filepath.Join(rawDir, rawE9), "inner")
	if err := os.Symlink(rawTarget, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	xattrOK := unix.Setxattr(filepath.Join(root, rawE9), rawXattr, []byte("v"), 0) == nil
	if !xattrOK {
		t.Log("user xattrs unsupported here, the xattr name goes unchecked")
	}
	return root, xattrOK
}

func checkRawTree(t *testing.T, dst string, xattrOK bool) {
	t.Helper()
	for name, want := range map[string]string{
		rawE9:                       "e9",
		rawE8:                       "e8",
		filepath.Join(rawDir, rawE9): "inner",
	} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("restored %q: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q holds %q, want %q", name, got, want)
		}
	}
	if got, err := os.Readlink(filepath.Join(dst, "link")); err != nil || got != rawTarget {
		t.Errorf("restored link points at %q (%v), want %q", got, err, rawTarget)
	}
	if xattrOK {
		var buf [8]byte
		n, err := unix.Getxattr(filepath.Join(dst, rawE9), rawXattr, buf[:])
		if err != nil || string(buf[:n]) != "v" {
			t.Errorf("restored xattr %q = %q (%v), want %q", rawXattr, buf[:n], err, "v")
		}
	}
}

func checkRawEntries(t *testing.T, entries map[string]ark.FileInfo, xattrOK bool) {
	t.Helper()
	for _, name := range []string{rawE9, rawE8, rawDir + "/", rawDir + "/" + rawE9, "link"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("no entry %q", name)
		}
	}
	if got := entries["link"].LinkDest; got != rawTarget {
		t.Errorf("link entry points at %q, want %q", got, rawTarget)
	}
	if xattrOK {
		if got := string(entries[rawE9].Xattrs[rawXattr]); got != "v" {
			t.Errorf("entry %q carries %q = %q, want %q", rawE9, rawXattr, got, "v")
		}
	}
}

func TestRawNamesSurviveTheRoundTrip(t *testing.T) {
	root, xattrOK := rawTree(t)
	repo, snap, dst := roundTrip(t, root)
	checkRawEntries(t, entriesOf(t, repo, snap.ID), xattrOK)
	checkRawTree(t, dst, xattrOK)
}

func TestRawNamesMatchExcludeRestorePathAndFind(t *testing.T) {
	ctx := context.Background()
	root, _ := rawTree(t)
	repo, snap, _ := roundTrip(t, root)

	src, err := arkfs.NewSource(root, arkfs.WithExclude(rawE8))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	excluded, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	entries := entriesOf(t, repo, excluded.ID)
	if _, ok := entries[rawE8]; ok {
		t.Errorf("exclude %q kept the entry", rawE8)
	}
	if _, ok := entries[rawE9]; !ok {
		t.Errorf("exclude %q dropped %q too", rawE8, rawE9)
	}

	dst := t.TempDir()
	if _, err := arkfs.Restore(ctx, repo, snap.ID, dst, ark.WithRestorePaths(rawDir+"/"+rawE9)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, rawDir, rawE9)); err != nil {
		t.Errorf("restore path %q: %v", rawDir+"/"+rawE9, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, rawE9)); !os.IsNotExist(err) {
		t.Errorf("restore path %q also restored %q: %v", rawDir+"/"+rawE9, rawE9, err)
	}

	for pattern, want := range map[string][]string{
		"/" + rawE8: {rawE8},
		// ? consumes one byte that is not valid UTF-8.
		"/caf?.txt": {rawE8, rawE9},
		rawDir:      {rawDir + "/", rawDir + "/" + rawE9},
	} {
		var got []string
		err := arkfind.Find(ctx, repo, ark.SnapshotFilter{}, []string{pattern}, func(m arkfind.Match) error {
			if m.Snapshot.ID == snap.ID {
				got = append(got, m.File.Path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Find(%q): %v", pattern, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("Find(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestRawNamesSurviveExportAndImport(t *testing.T) {
	ctx := context.Background()
	root, xattrOK := rawTree(t)
	repo, snap, _ := roundTrip(t, root)

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}
	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	checkRawEntries(t, entriesOf(t, target, imported.ID), xattrOK)

	dst := t.TempDir()
	if _, err := arkfs.Restore(ctx, target, imported.ID, dst); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	checkRawTree(t, dst, xattrOK)
}
