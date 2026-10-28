package arkfs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
