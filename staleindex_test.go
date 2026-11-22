package ark_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

// foreignPrune sets up the hazard a handle that kept its index across
// operations would fall into: snapshot A holds keep.bin and drop.bin, K holds
// keep.bin only, both written through repo. A second Repository on the same
// backend forgets A and prunes, which deletes drop.bin's chunks and repacks
// keep.bin's out of the pack they shared with them. An index repo loaded
// before the prune lists both chunks in a pack that is gone.
func foreignPrune(t *testing.T) (repo *ark.Repository, src string, keep *ark.Snapshot) {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	repo = chunkyRepo(t, b)
	src = t.TempDir()
	writeTestFile(t, filepath.Join(src, "keep.bin"), []byte(payload(1)))
	writeTestFile(t, filepath.Join(src, "drop.bin"), []byte(payload(2)))
	a := backupDir(t, repo, src)
	if err := os.Remove(filepath.Join(src, "drop.bin")); err != nil {
		t.Fatal(err)
	}
	keep = backupDir(t, repo, src)

	before := dataPacks(t, b)
	other, err := ark.Open(ctx, noClose{b}, testPassword)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer other.Close()
	if _, err := other.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{a.ID}}); err != nil {
		t.Fatalf("Forget A: %v", err)
	}
	pruned, err := other.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.BlobsRemoved == 0 || pruned.BytesRepacked == 0 {
		t.Fatalf("Prune removed %d blobs and repacked %d bytes, want both above zero",
			pruned.BlobsRemoved, pruned.BytesRepacked)
	}
	after := dataPacks(t, b)
	if len(before) == 0 || len(after) == 0 {
		t.Fatalf("data packs before the prune %v, after %v, want some in both", before, after)
	}
	for name := range before {
		if after[name] {
			t.Fatalf("data pack %s survived the prune, so keep.bin's chunks were not moved out of a deleted pack", name)
		}
	}
	return repo, src, keep
}

func dataPacks(t *testing.T, b ark.Backend) map[string]bool {
	t.Helper()
	packs := map[string]bool{}
	err := b.List(context.Background(), ark.DataPackFile, func(info ark.BlobInfo) error {
		packs[info.Name] = true
		return nil
	})
	if err != nil {
		t.Fatalf("List data packs: %v", err)
	}
	return packs
}

func checkAndRestore(t *testing.T, repo *ark.Repository, snap *ark.Snapshot, src string) {
	t.Helper()
	ctx := context.Background()
	check, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if findings := slices.Concat(check.Errors, check.Warnings); len(findings) > 0 {
		t.Fatalf("Check after the prune: %d errors, %d warnings, first: %s",
			len(check.Errors), len(check.Warnings), findings[0])
	}
	dst := t.TempDir()
	sink, err := arkfs.NewSink(dst)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore %s: %v", snap.ID, err)
	}
	if diff := diffTree(readTree(t, src), readTree(t, dst)); diff != "" {
		t.Fatalf("snapshot %s restores a different tree: %s", snap.ID, diff)
	}
}

// TestBackupAfterAForeignPruneStoresWhatThePruneRemoved backs drop.bin up
// again on the handle that wrote it before the prune. Deduplicating against
// an index from before the prune would leave the new snapshot without its
// chunks.
func TestBackupAfterAForeignPruneStoresWhatThePruneRemoved(t *testing.T) {
	repo, src, _ := foreignPrune(t)
	writeTestFile(t, filepath.Join(src, "drop.bin"), []byte(payload(2)))
	b := backupDir(t, repo, src)
	if b.Stats.ChunksNew == 0 {
		t.Errorf("backup B stored no new chunks, want drop.bin's again")
	}
	checkAndRestore(t, repo, b, src)
}

// TestCheckAndRestoreAfterAForeignPrune reads K on the handle that wrote it
// before the prune; an index from before the prune points keep.bin at the
// pack the repack deleted.
func TestCheckAndRestoreAfterAForeignPrune(t *testing.T) {
	repo, src, keep := foreignPrune(t)
	checkAndRestore(t, repo, keep, src)
}

// TestForeignPruneRefusedWhileAReaderIsOpen pins why a Reader may keep its
// trees and chunk cache for its whole session: its shared lock keeps a prune
// from any other handle out until Close.
func TestForeignPruneRefusedWhileAReaderIsOpen(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := chunkyRepo(t, b)
	backupDir(t, repo, readerTree(t))
	rd, err := repo.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	other, err := ark.Open(ctx, noClose{b}, testPassword)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer other.Close()
	if _, err := other.Prune(ctx); !errors.Is(err, ark.ErrRepoLocked) {
		t.Errorf("Prune beside an open Reader = %v, want ErrRepoLocked", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Prune(ctx); err != nil {
		t.Errorf("Prune after Close: %v", err)
	}
}
