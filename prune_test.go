package ark_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

// TestPruneSweepsBothNamespaces guards the split of trees out of the chunk
// namespace: prune has to match each namespace against its own live set, or it
// deletes the tree nodes of the snapshots it is meant to keep.
func TestPruneSweepsBothNamespaces(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()

	root := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("first revision")

	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	first, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	write("second revision")
	second, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{first.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	result, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// One orphaned chunk and one orphaned tree node.
	if result.BlobsRemoved != 2 {
		t.Errorf("prune removed %d objects, want 2", result.BlobsRemoved)
	}

	vr, err := repo.Verify(ctx, second.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK {
		t.Errorf("kept snapshot broke: missing %v, corrupt %v", vr.MissingChunks, vr.CorruptChunks)
	}

	dest := t.TempDir()
	sink, err := arkfs.NewSink(dest)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(ctx, second.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "data.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "second revision" {
		t.Errorf("restored %q, want %q", got, "second revision")
	}
}
