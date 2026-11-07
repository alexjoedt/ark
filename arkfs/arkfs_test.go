package arkfs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

func openRepo(t *testing.T) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestBackupAndRestoreAPath(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "conf"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "conf", "app.toml"), []byte("key = 1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap, err := arkfs.Backup(ctx, repo, src, ark.WithComment("from a path"))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if snap.Comment != "from a path" {
		t.Errorf("snapshot comment is %q, want %q", snap.Comment, "from a path")
	}

	dst := t.TempDir()
	result, err := arkfs.Restore(ctx, repo, snap.ID, dst)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if result.FilesRestored != 1 {
		t.Errorf("restored %d files, want 1", result.FilesRestored)
	}

	got, err := os.ReadFile(filepath.Join(dst, "conf", "app.toml"))
	if err != nil {
		t.Fatalf("reading the restored file: %v", err)
	}
	if string(got) != "key = 1" {
		t.Errorf("restored %q, want %q", got, "key = 1")
	}
}

func TestBackupRejectsAMissingPath(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)

	if _, err := arkfs.Backup(ctx, repo, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Backup of a missing path succeeded")
	}
}

// A rewrite that restores the old mtime still changes the ctime, so the
// second backup reads it again and takes only the untouched file as is.
func TestIncrementalBackupCatchesMtimePreservingRewrite(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t)

	src := t.TempDir()
	same, edited := filepath.Join(src, "same.txt"), filepath.Join(src, "edited.txt")
	for _, p := range []string{same, edited} {
		if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := arkfs.Backup(ctx, repo, src); err != nil {
		t.Fatalf("first Backup: %v", err)
	}

	fi, err := os.Stat(edited)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // ctime granularity
	if err := os.WriteFile(edited, []byte("AFTER!"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(edited, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	snap, err := arkfs.Backup(ctx, repo, src)
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	if snap.Stats.FilesUnchanged != 1 {
		t.Errorf("FilesUnchanged = %d, want 1", snap.Stats.FilesUnchanged)
	}

	dst := t.TempDir()
	if _, err := arkfs.Restore(ctx, repo, snap.ID, dst); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for name, want := range map[string]string{"same.txt": "before", "edited.txt": "AFTER!"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != want {
			t.Errorf("%s restored %q (%v), want %q", name, got, err, want)
		}
	}
}
