package ark_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// readerTree writes a tree with nested dirs, an empty file, an empty dir, a
// multi-chunk file, a symlink and a hardlink, all with a fixed mtime.
func readerTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"top.bin":        payload(7),
		"empty":          "",
		"a/b/c/deep.txt": "deep",
		"a/b/f.txt":      "f",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "a", "emptydir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b/f.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "top.bin"), filepath.Join(root, "a", "hard")); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		return os.Chtimes(p, mtime, mtime)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// openReaderFS backs up readerTree and opens its file system in a session
// closed at test end.
func openReaderFS(t *testing.T) (*ark.Repository, *ark.Reader, *ark.SnapshotFS, *ark.Snapshot, string) {
	t.Helper()
	repo := chunkyRepo(t, arkmem.New())
	root := readerTree(t)
	snap := backupDir(t, repo, root)
	rd, err := repo.OpenReader(context.Background())
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	fsys, err := rd.FS(snap.ID)
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	return repo, rd, fsys, snap, root
}

// TestReaderFSPassesFSTest runs the io/fs conformance test. Open on a symlink
// fails by design and fstest opens every entry it lists, so the only errors
// allowed are that Open of the symlink, which its own Lstat check replaces.
func TestReaderFSPassesFSTest(t *testing.T) {
	_, _, fsys, _, _ := openReaderFS(t)
	err := fstest.TestFS(fsys, "top.bin", "empty", "a/b/c/deep.txt", "a/b/f.txt", "a/emptydir", "link", "a/hard")
	if err == nil {
		t.Fatal("TestFS passed although Open on the symlink fails")
	}
	joined, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("TestFS: %v", err)
	}
	for _, e := range joined.Unwrap() {
		if !strings.HasPrefix(e.Error(), "link: Open: ") || !errors.Is(e, fs.ErrInvalid) {
			t.Errorf("TestFS: %v", e)
		}
	}

	info, err := fsys.Lstat("link")
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode().Type() != fs.ModeSymlink || info.Name() != "link" {
		t.Errorf("Lstat(link) = %v %q, want a symlink named link", info.Mode(), info.Name())
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "link" && e.Type() != fs.ModeSymlink {
			t.Errorf("ReadDir: link has type %v", e.Type())
		}
	}
}

func TestReaderReadAtMatchesRestore(t *testing.T) {
	repo, _, fsys, snap, _ := openReaderFS(t)
	want, err := os.ReadFile(filepath.Join(restoreInto(t, repo, snap.ID), "top.bin"))
	if err != nil {
		t.Fatal(err)
	}
	offsets, err := ark.ChunkOffsets(fsys, "top.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) < 10 {
		t.Fatalf("top.bin has %d chunks, want many", len(offsets))
	}

	f, err := fsys.Open("top.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ra, ok := f.(io.ReaderAt)
	if !ok {
		t.Fatal("file is no io.ReaderAt")
	}
	if _, ok := f.(io.Seeker); !ok {
		t.Fatal("file is no io.Seeker")
	}

	size := int64(len(want))
	check := func(off, n int64) {
		t.Helper()
		got := make([]byte, n)
		k, err := ra.ReadAt(got, off)
		end := min(off+n, size)
		if end-off < n {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d, %d) past the end: err %v, want io.EOF", off, n, err)
			}
		} else if err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", off, n, err)
		}
		if !bytes.Equal(got[:k], want[off:end]) {
			t.Fatalf("ReadAt(%d, %d) = %d bytes that differ from the restored file", off, n, k)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 300 {
		off := rng.Int64N(size + 1)
		check(off, rng.Int64N(64<<10))
	}
	for _, o := range offsets[1:] {
		check(o-1, 2)
		check(o, 1)
		check(o-3, 3)
	}
	check(size-5, 10)

	if _, err := f.(io.Seeker).Seek(offsets[3]-7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, want[offsets[3]-7:]) {
		t.Error("Read after Seek differs from the restored file")
	}
}

func TestReaderStatAndLinks(t *testing.T) {
	_, rd, fsys, _, root := openReaderFS(t)

	for _, name := range []string{"top.bin", "empty", "a/b/c/deep.txt", "a/emptydir", "a", "a/hard"} {
		got, err := fsys.Stat(name)
		if err != nil {
			t.Fatalf("Stat(%s): %v", name, err)
		}
		src, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		wantSize := src.Size()
		if src.IsDir() {
			wantSize = 0
		}
		if got.Mode() != src.Mode() || got.Size() != wantSize || !got.ModTime().Equal(src.ModTime()) {
			t.Errorf("Stat(%s) = %v %d %v, want %v %d %v",
				name, got.Mode(), got.Size(), got.ModTime(), src.Mode(), wantSize, src.ModTime())
		}
		if fi, ok := got.Sys().(ark.FileInfo); !ok || fi.Path != name {
			t.Errorf("Stat(%s).Sys() = %#v, want an ark.FileInfo for the name", name, got.Sys())
		}
	}

	// The walk meets a/hard first, so top.bin is the hardlink entry.
	fi, _ := mustStat(t, fsys, "top.bin").Sys().(ark.FileInfo)
	if fi.Type != ark.FileTypeHardlink || fi.LinkDest != "a/hard" {
		t.Fatalf("top.bin Sys() = %s to %q, want a hardlink to a/hard", fi.Type, fi.LinkDest)
	}
	hard, err := fs.ReadFile(fsys, "top.bin")
	if err != nil {
		t.Fatal(err)
	}
	if string(hard) != payload(7) {
		t.Error("hardlink does not read with its target's content")
	}

	if _, err := fsys.Open("link"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("Open(link) = %v, want fs.ErrInvalid", err)
	}
	if _, err := fsys.Stat("link"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("Stat(link) = %v, want fs.ErrInvalid", err)
	}
	if target, err := fsys.ReadLink("link"); err != nil || target != "a/b/f.txt" {
		t.Errorf("ReadLink(link) = %q, %v", target, err)
	}
	if _, err := fsys.ReadLink("top.bin"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ReadLink(top.bin) = %v, want fs.ErrInvalid", err)
	}
	if _, err := fsys.Open("a/b/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(a/b/nope) = %v, want fs.ErrNotExist", err)
	}
	if _, err := rd.FS("ffffffffffffffffffffffffffffffff"); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("FS(unknown) = %v, want ErrNotFound", err)
	}
}

func mustStat(t *testing.T, fsys fs.StatFS, name string) fs.FileInfo {
	t.Helper()
	info, err := fsys.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%s): %v", name, err)
	}
	return info
}

func TestReaderHoldsOneLockAndLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	repo := chunkyRepo(t, arkmem.New())
	snap := backupDir(t, repo, readerTree(t))

	before := runtime.NumGoroutine()
	rd, err := repo.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := rd.FS(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open("top.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}
	if locks, err := repo.Locks(ctx); err != nil || len(locks) != 1 || locks[0].Exclusive {
		t.Fatalf("Locks during the session = %v, %v, want one shared lock", locks, err)
	}
	if snaps, err := rd.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(snaps) != 1 {
		t.Errorf("Snapshots = %d, %v", len(snaps), err)
	}
	if _, err := repo.Prune(ctx); !errors.Is(err, ark.ErrRepoLocked) {
		t.Errorf("Prune during the session = %v, want ErrRepoLocked", err)
	}

	if err := rd.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if locks, err := repo.Locks(ctx); err != nil || len(locks) != 0 {
		t.Errorf("Locks after Close = %v, %v, want none", locks, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines before the session, %d after Close", before, after)
	}

	if _, err := f.(io.ReaderAt).ReadAt(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("ReadAt after Close = %v, want fs.ErrClosed", err)
	}
	if _, err := rd.FS(snap.ID); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("FS after Close = %v, want fs.ErrClosed", err)
	}
}
