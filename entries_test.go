package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"golang.org/x/sys/unix"
)

// roundTrip backs up root into a fresh in-memory repository and restores the
// snapshot into a new directory. The repository stays open: arkmem drops every
// object on Close, so a second Open would read an empty backend.
func roundTrip(t *testing.T, root string) (*ark.Repository, ark.Backend, *ark.Snapshot, string) {
	t.Helper()
	ctx := context.Background()

	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := t.TempDir()
	sink, err := arkfs.NewSink(dst)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return repo, b, snap, dst
}

// entriesOf lists the snapshot entries by path.
func entriesOf(t *testing.T, repo *ark.Repository, id string) map[string]ark.FileInfo {
	t.Helper()
	byPath := make(map[string]ark.FileInfo)
	if err := repo.Walk(context.Background(), id, func(f ark.FileInfo) error {
		byPath[f.Path] = f
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return byPath
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Lstat(a)
	if err != nil {
		t.Fatalf("lstat %s: %v", a, err)
	}
	fb, err := os.Lstat(b)
	if err != nil {
		t.Fatalf("lstat %s: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

func TestHardlinkIsStoredOnceAndRestoredAsALink(t *testing.T) {
	root := t.TempDir()
	// "a.txt" sorts before "z-link.txt", so the walk sees the content first.
	content := []byte("shared contents")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "z-link.txt")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}

	repo, _, snap, dst := roundTrip(t, root)

	entries := entriesOf(t, repo, snap.ID)
	link, ok := entries["z-link.txt"]
	if !ok {
		t.Fatalf("snapshot has no z-link.txt, got %v", entries)
	}
	if link.Type != ark.FileTypeHardlink {
		t.Errorf("z-link.txt has type %q, want %q", link.Type, ark.FileTypeHardlink)
	}
	if link.LinkDest != "a.txt" {
		t.Errorf("z-link.txt points at %q, want %q", link.LinkDest, "a.txt")
	}
	if entries["a.txt"].Type != ark.FileTypeRegular {
		t.Errorf("a.txt has type %q, want %q", entries["a.txt"].Type, ark.FileTypeRegular)
	}

	// Both names count, the bytes are read once.
	if snap.Stats.FileCount != 2 {
		t.Errorf("stats count %d files, want 2", snap.Stats.FileCount)
	}
	if snap.Stats.TotalSize != int64(len(content)) {
		t.Errorf("stats total size %d, want %d, the content is stored once",
			snap.Stats.TotalSize, len(content))
	}

	if !sameInode(t, filepath.Join(dst, "a.txt"), filepath.Join(dst, "z-link.txt")) {
		t.Error("restored a.txt and z-link.txt are not the same inode")
	}
	got, err := os.ReadFile(filepath.Join(dst, "z-link.txt"))
	if err != nil {
		t.Fatalf("read restored link: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("restored link holds %q, want %q", got, content)
	}
}

func TestXattrsSurviveTheRoundTrip(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("body"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := unix.Setxattr(file, "user.ark", []byte("value"), 0); err != nil {
		t.Skipf("xattrs unsupported here: %v", err)
	}
	if err := unix.Setxattr(root, "user.ark_dir", []byte("dir value"), 0); err != nil {
		t.Skipf("directory xattrs unsupported here: %v", err)
	}

	repo, b, snap, dst := roundTrip(t, root)

	entries := entriesOf(t, repo, snap.ID)
	if got := string(entries["file.txt"].Xattrs["user.ark"]); got != "value" {
		t.Errorf("snapshot entry carries user.ark = %q, want %q", got, "value")
	}

	// The tree entry, not FileInfo, owns the wire format: base64 values.
	var found bool
	for _, name := range objectNames(t, b, ark.TreeFile) {
		var node struct {
			Entries []struct {
				Name   string            `json:"name"`
				Xattrs map[string]string `json:"xattrs"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(openObject(t, repo, ark.TreeFile, name), &node); err != nil {
			t.Fatalf("parse tree %s: %v", name, err)
		}
		for _, e := range node.Entries {
			if e.Name == "file.txt" {
				found = true
				// base64 of "value".
				if e.Xattrs["user.ark"] != "dmFsdWU=" {
					t.Errorf("tree entry stores user.ark as %q, want base64 %q",
						e.Xattrs["user.ark"], "dmFsdWU=")
				}
			}
		}
	}
	if !found {
		t.Fatal("no tree entry for file.txt")
	}

	var buf [64]byte
	n, err := unix.Getxattr(filepath.Join(dst, "file.txt"), "user.ark", buf[:])
	if err != nil {
		t.Fatalf("getxattr on the restored file: %v", err)
	}
	if got := string(buf[:n]); got != "value" {
		t.Errorf("restored file carries user.ark = %q, want %q", got, "value")
	}
}

func TestSpecialFilesAreSkippedAndCounted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("fifos unsupported here: %v", err)
	}

	repo, _, snap, dst := roundTrip(t, root)

	if snap.Stats.Skipped != 1 {
		t.Errorf("stats count %d skipped entries, want 1", snap.Stats.Skipped)
	}
	if snap.Stats.FileCount != 1 {
		t.Errorf("stats count %d files, want 1, the fifo is not one", snap.Stats.FileCount)
	}
	if _, ok := entriesOf(t, repo, snap.ID)["pipe"]; ok {
		t.Error("the fifo made it into the snapshot tree")
	}
	if _, err := os.Lstat(filepath.Join(dst, "pipe")); !os.IsNotExist(err) {
		t.Errorf("restore recreated the fifo: %v", err)
	}
}

func TestExportKeepsHardlinksAndXattrs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("shared"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "z-link.txt")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	xattrOK := unix.Setxattr(filepath.Join(root, "a.txt"), "user.ark", []byte("value"), 0) == nil

	repo, _, snap, _ := roundTrip(t, root)

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}
	imported, err := repo.Import(ctx, &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	entries := entriesOf(t, repo, imported.ID)
	link, ok := entries["z-link.txt"]
	if !ok {
		t.Fatalf("imported snapshot has no z-link.txt, got %v", entries)
	}
	if link.Type != ark.FileTypeHardlink {
		t.Errorf("z-link.txt imported as %q, want %q", link.Type, ark.FileTypeHardlink)
	}
	if link.LinkDest != "a.txt" {
		t.Errorf("z-link.txt points at %q, want %q", link.LinkDest, "a.txt")
	}
	if xattrOK {
		if got := string(entries["a.txt"].Xattrs["user.ark"]); got != "value" {
			t.Errorf("a.txt imported with user.ark = %q, want %q", got, "value")
		}
	}
}
