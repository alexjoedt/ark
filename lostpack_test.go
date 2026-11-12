package ark_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
)

// TestBackupStoresAgainWhatALostPackHeld deletes a pack and leaves the index
// object that lists it. A backup that reads the same contents again finds
// its blobs in no stored pack and stores them: its snapshot restores, and so
// does the one that lost them, whose lookups now end at the new copies. Check
// names the lost pack and its blobs as an error while no stored pack holds
// them, then the pack alone as a warning until a prune drops what the index
// says about it.
func TestBackupStoresAgainWhatALostPackHeld(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []ark.HandleType{ark.BlobFile, ark.TreeFile} {
		t.Run(typ.String(), func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			src := smallFiles("app", 6)
			first := mustBackup(t, repo, src)
			blob := blobNames(t, repo, typ)[0]
			lost, _, _, err := ark.BlobPlace(ctx, repo, typ, blob)
			if err != nil {
				t.Fatalf("BlobPlace: %v", err)
			}
			deletePackOf(t, b, repo, typ, blob)
			if check := mustCheck(t, repo); !naming(check.Errors, lost.Name, blob) {
				t.Errorf("Check errors before the backup = %q, want one naming the lost pack %s and %s",
					check.Errors, lost.Name, blob)
			}

			b.take()
			again := mustBackup(t, repo, src)
			if got := b.take(); got["put "+lost.Type.String()] != 1 {
				t.Errorf("backup after the loss: %v, want one %s object stored", got, lost.Type)
			}
			assertRestores(t, repo, again, src)
			assertRestores(t, repo, first, src)

			check := mustCheck(t, repo)
			if len(check.Errors) != 0 || len(check.Warnings) != 1 || !naming(check.Warnings, lost.Name) ||
				naming(check.Warnings, blob) {
				t.Errorf("Check = %+v, want no error and one warning naming the lost pack %s and not %s",
					check, lost.Name, blob)
			}
			if _, err := repo.Prune(ctx); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
				t.Errorf("Check after the prune = %+v, want no findings", check)
			}
			assertRestores(t, repo, again, src)
		})
	}
}

// TestRestoreReadsTheStoredCopyOfAChunkInTwoPacks stores every chunk in two
// packs and deletes the one with the smaller name, which a lookup prefers
// while both are stored.
func TestRestoreReadsTheStoredCopyOfAChunkInTwoPacks(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	src := smallFiles("app", 6)
	snap := mustBackup(t, repo, src)
	withoutDataIndex(t, b, func() { mustBackup(t, repo, src) })
	packs := objectNames(t, b, ark.DataPackFile)
	if len(packs) != 2 {
		t.Fatalf("%d data packs, want the chunks in 2", len(packs))
	}
	lost := slices.Min(packs)
	if in, _, _ := place(t, repo, src.files[src.order[0]]); in != lost {
		t.Fatalf("a chunk in two packs resolves to %s, want the smaller name %s", in, lost)
	}
	if err := b.Delete(ctx, ark.Handle{Type: ark.DataPackFile, Name: lost}); err != nil {
		t.Fatalf("delete pack: %v", err)
	}

	assertRestores(t, repo, snap, src)
	if res, err := repo.Verify(ctx, snap.ID); err != nil || !res.OK {
		t.Errorf("Verify = %+v, %v; want OK", res, err)
	}
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 1 ||
		!naming(check.Warnings, lost) {
		t.Errorf("Check = %+v, want no error and one warning naming the lost pack %s", check, lost)
	}
}

// TestUnchangedFileKeepsTheChunksOfItsParentAfterALostPack pins where the
// repair by the next backup ends. A file a backup takes as unchanged keeps the
// chunks of its parent without a lookup, so no index is asked about them: its
// snapshot misses the chunk of the lost pack like the parent does. A backup
// that reads the file stores the chunk again, which is the case once the
// snapshots Check names are forgotten.
func TestUnchangedFileKeepsTheChunksOfItsParentAfterALostPack(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	const content = "content of a file that does not change"
	src := newStatSource("app", map[string]statFile{
		"f": {content: content, mtime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	first := mustBackup(t, repo, src)
	chunk := ark.ObjectName(repo, []byte(content))
	deletePackOf(t, b, repo, ark.BlobFile, chunk)

	second := mustBackup(t, repo, src)
	if src.opens["f"] != 1 {
		t.Fatalf("the file was opened %d times, want once: the second backup takes it from its parent", src.opens["f"])
	}
	res, err := repo.Verify(ctx, second.ID)
	if err != nil || !slices.Equal(res.MissingChunks, []string{chunk}) {
		t.Errorf("Verify = %+v, %v; want the chunk %s missing", res, err, chunk)
	}
	if check := mustCheck(t, repo); !naming(check.Errors, first.ID) || !naming(check.Errors, second.ID) {
		t.Errorf("Check errors = %q, want both snapshots named", check.Errors)
	}

	mustForget(t, repo, first, second)
	third := mustBackup(t, repo, src)
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, third.ID, sink); err != nil || sink.written["f"] != content {
		t.Errorf("Restore of a backup that read the file = %q, %v; want its content", sink.written["f"], err)
	}
}

// TestPruneStopsOnATreeOfALostPack deletes the tree pack of a snapshot. Prune
// cannot walk the tree to the chunks it references, so it deletes nothing,
// and its error says the pack is lost instead of sending the user to
// RebuildIndex, which cannot bring the tree back.
func TestPruneStopsOnATreeOfALostPack(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	mustBackup(t, repo, smallFiles("app", 3))
	deletePackOf(t, b, repo, ark.TreeFile, blobNames(t, repo, ark.TreeFile)[0])

	packs := objectNames(t, b, ark.DataPackFile)
	_, err := repo.Prune(ctx)
	if !errors.Is(err, ark.ErrNotFound) || !strings.Contains(err.Error(), "the pack that held it is lost") ||
		strings.Contains(err.Error(), "index rebuild restores") {
		t.Fatalf("Prune = %v, want ErrNotFound saying the pack is lost and not advising an index rebuild", err)
	}
	if got := objectNames(t, b, ark.DataPackFile); !slices.Equal(got, packs) {
		t.Errorf("data packs after the prune = %v, want %v untouched", got, packs)
	}
}
