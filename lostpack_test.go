package ark_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
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

// TestUnchangedFileIsReadAgainOnceALoadFoundALostPack: a file a backup takes
// as unchanged keeps the chunks of its parent without a lookup. Until an index
// load has seen the lost pack nothing tells the backup, and its snapshot
// misses the chunk like the parent does. Any load that sees it stores the
// marker, readers included, and every backup after that asks the data index
// about the chunks of the files it would take unread, reads the file again and
// writes a snapshot that restores. A prune keeps the marker while it keeps the
// records of the lost pack and drops it once the chunk is stored again; the
// backup after that costs what an unchanged backup always did.
func TestUnchangedFileIsReadAgainOnceALoadFoundALostPack(t *testing.T) {
	ctx := context.Background()
	const content = "content of a file that does not change"
	notices := map[string]func(t *testing.T, repo *ark.Repository, snap *ark.Snapshot){
		"check": func(t *testing.T, repo *ark.Repository, _ *ark.Snapshot) { mustCheck(t, repo) },
		"verify": func(t *testing.T, repo *ark.Repository, snap *ark.Snapshot) {
			if _, err := repo.Verify(ctx, snap.ID); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		},
		"restore": func(t *testing.T, repo *ark.Repository, snap *ark.Snapshot) {
			if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}); !errors.Is(err, ark.ErrNotFound) {
				t.Fatalf("Restore = %v, want ErrNotFound for the lost chunk", err)
			}
		},
		"export": func(t *testing.T, repo *ark.Repository, snap *ark.Snapshot) {
			if err := repo.Export(ctx, snap.ID, io.Discard); !errors.Is(err, ark.ErrNotFound) {
				t.Fatalf("Export = %v, want ErrNotFound for the lost chunk", err)
			}
		},
		"backup": func(t *testing.T, repo *ark.Repository, _ *ark.Snapshot) {
			mustBackup(t, repo, smallFiles("other", 1))
		},
	}
	for name, notice := range notices {
		t.Run(name, func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			src := newStatSource("app", map[string]statFile{
				"f": {content: content, mtime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
			})
			first := mustBackup(t, repo, src)
			chunk := ark.ObjectName(repo, []byte(content))
			deletePackOf(t, b, repo, ark.BlobFile, chunk)

			second := mustBackup(t, repo, src)
			if src.opens["f"] != 1 || len(objectNames(t, b, ark.LostPackFile)) != 0 {
				t.Fatalf("before any load saw the loss: file opened %d times, markers %v; want once and none",
					src.opens["f"], objectNames(t, b, ark.LostPackFile))
			}

			notice(t, repo, second)
			if n := len(objectNames(t, b, ark.LostPackFile)); n != 1 {
				t.Fatalf("%d markers after the %s, want 1", n, name)
			}
			if _, err := repo.Prune(ctx); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if n := len(objectNames(t, b, ark.LostPackFile)); n != 1 {
				t.Fatalf("%d markers after a prune that keeps the lost pack's records, want 1", n)
			}

			third := mustBackup(t, repo, src)
			if src.opens["f"] != 2 {
				t.Fatalf("the file was opened %d times, want twice: the marker makes the backup read it",
					src.opens["f"])
			}
			for _, snap := range []*ark.Snapshot{first, second, third} {
				sink := &recordingSink{}
				if _, err := repo.Restore(ctx, snap.ID, sink); err != nil || sink.written["f"] != content {
					t.Errorf("Restore of %s = %q, %v; want the content", snap.ID, sink.written["f"], err)
				}
			}

			if _, err := repo.Prune(ctx); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if n := len(objectNames(t, b, ark.LostPackFile)); n != 0 {
				t.Fatalf("%d markers after a prune that dropped the lost pack, want none", n)
			}
			if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
				t.Errorf("Check = %+v, want no findings", check)
			}
			b.take()
			mustBackup(t, repo, src)
			got := b.take()
			if src.opens["f"] != 2 || got["exists lostpacks"] != 1 || got["list dataindex"] != 0 {
				t.Errorf("backup after the marker went: file opened %d times, requests %v; want twice, "+
					"one marker request and no data index", src.opens["f"], got)
			}
		})
	}
}

// TestOnlyAWritingRunNeedsTheMarker: on a repository that refuses the
// marker, a read, a check and a repair still run and log the failed write,
// while a backup, which writes, fails. A prune dry run writes nothing, so it
// does not even try.
func TestOnlyAWritingRunNeedsTheMarker(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	src := smallFiles("app", 6)
	snap := mustBackup(t, openRepo(t, b), src)
	var logged bytes.Buffer
	repo, err := ark.Open(ctx, b, testPassword, ark.WithLogger(slog.New(slog.NewTextHandler(&logged, nil))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	withoutDataIndex(t, b, func() { mustBackup(t, repo, src) })
	lost := slices.Min(objectNames(t, b, ark.DataPackFile))
	if err := b.Delete(ctx, ark.Handle{Type: ark.DataPackFile, Name: lost}); err != nil {
		t.Fatalf("delete pack: %v", err)
	}

	b.take()
	if _, err := repo.Prune(ctx, ark.WithPruneDryRun()); err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if got := b.take(); got["exists lostpacks"] != 0 || got["put lostpacks"] != 0 {
		t.Errorf("prune dry run asked for or wrote the marker: %v", got)
	}

	refused := errors.New("read-only")
	b.onPut = func(h ark.Handle) error {
		if h.Type == ark.LostPackFile {
			return refused
		}
		return nil
	}
	assertRestores(t, repo, snap, src)
	if res, err := repo.Verify(ctx, snap.ID); err != nil || !res.OK {
		t.Errorf("Verify = %+v, %v; want OK", res, err)
	}
	if res := mustCheck(t, repo); len(res.Errors)+len(res.Warnings) == 0 {
		t.Errorf("Check = %+v, want the lost pack reported", res)
	}
	if _, err := repo.Backup(ctx, src); !errors.Is(err, refused) {
		t.Errorf("Backup = %v, want the failed marker write", err)
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Errorf("Prune: %v", err)
	}
	// The prune dropped the lost pack, whose chunks had stored copies: the
	// rebuild gets a loss of its own.
	lost = slices.Min(objectNames(t, b, ark.DataPackFile))
	if err := b.Delete(ctx, ark.Handle{Type: ark.DataPackFile, Name: lost}); err != nil {
		t.Fatalf("delete pack: %v", err)
	}
	if _, err := repo.RebuildIndex(ctx); err != nil {
		t.Errorf("RebuildIndex: %v", err)
	}
	if n := len(objectNames(t, b, ark.LostPackFile)); n != 0 {
		t.Fatalf("%d markers, want none", n)
	}
	if n := strings.Count(logged.String(), "failed to store the lost-pack marker"); n < 5 {
		t.Errorf("the failed marker write was logged %d times, want once each by restore, verify, check, "+
			"prune and rebuild:\n%s", n, logged.String())
	}
}

// TestOnlyPruneDropsTheMarker: an index rebuild lists stored packs only. The
// records of a lost data pack it drops may be all that a snapshot's chunk was
// listed by, and a second rebuild no longer sees them, so no rebuild drops
// the marker and the next backup reads the file again. A prune drops it once
// every chunk a snapshot references is in a stored pack.
func TestOnlyPruneDropsTheMarker(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	const content = "content of a file that does not change"
	src := newStatSource("app", map[string]statFile{
		"f": {content: content, mtime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	mustBackup(t, repo, src)
	deletePackOf(t, b, repo, ark.BlobFile, ark.ObjectName(repo, []byte(content)))

	for range 2 {
		if _, err := repo.RebuildIndex(ctx); err != nil {
			t.Fatalf("RebuildIndex: %v", err)
		}
	}
	if n := len(objectNames(t, b, ark.LostPackFile)); n != 1 {
		t.Fatalf("%d markers after two rebuilds that dropped a lost pack, want 1", n)
	}
	if _, err := repo.Prune(ctx); err == nil {
		t.Fatal("Prune ran while a snapshot references a chunk no index lists")
	}
	snap := mustBackup(t, repo, src)
	if src.opens["f"] != 2 {
		t.Fatalf("the file was opened %d times, want twice", src.opens["f"])
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil || sink.written["f"] != content {
		t.Errorf("Restore = %q, %v; want the content", sink.written["f"], err)
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n := len(objectNames(t, b, ark.LostPackFile)); n != 0 {
		t.Fatalf("%d markers after the prune, want none", n)
	}
}

// TestBackupThatFindsTheLossCommitsNoUnreadFileOfIt: no load has seen the
// loss, a lost pack or an index object that does not open, so the backup
// takes the unchanged file unread; its changed file makes it load the data
// index, which finds the loss. The backup fails rather than commit a snapshot
// whose unread file references a chunk no lookup resolves, and the next
// backup reads the file again.
func TestBackupThatFindsTheLossCommitsNoUnreadFileOfIt(t *testing.T) {
	ctx := context.Background()
	const content = "content of a file that does not change"
	losses := map[string]func(t *testing.T, b ark.Backend, repo *ark.Repository){
		"pack": func(t *testing.T, b ark.Backend, repo *ark.Repository) {
			deletePackOf(t, b, repo, ark.BlobFile, ark.ObjectName(repo, []byte(content)))
		},
		"index object": garbleDataIndex,
	}
	for name, lose := range losses {
		t.Run(name, func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			mtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			src := newStatSource("app", map[string]statFile{
				"f": {content: content, mtime: mtime},
				"g": {content: "first content of a file that changes", mtime: mtime},
			})
			mustBackup(t, repo, src)
			lose(t, b, repo)
			snaps := objectNames(t, b, ark.SnapshotFile)

			src.files["g"] = statFile{content: "second content of a file that changes", mtime: mtime.Add(time.Hour)}
			_, err := repo.Backup(ctx, src)
			if err == nil || !strings.Contains(err.Error(), "next backup reads them again") {
				t.Fatalf("Backup = %v, want it to fail on the file it took unread", err)
			}
			if got := objectNames(t, b, ark.SnapshotFile); !slices.Equal(got, snaps) {
				t.Fatalf("snapshots %v after the failed backup, want %v", got, snaps)
			}
			if n := len(objectNames(t, b, ark.LostPackFile)); n != 1 {
				t.Fatalf("%d markers after the backup that found the loss, want 1", n)
			}
			snap := mustBackup(t, repo, src)
			if src.opens["f"] != 2 {
				t.Fatalf("the file was opened %d times, want twice", src.opens["f"])
			}
			sink := &recordingSink{}
			if _, err := repo.Restore(ctx, snap.ID, sink); err != nil || sink.written["f"] != content {
				t.Errorf("Restore = %q, %v; want the content", sink.written["f"], err)
			}
		})
	}
}

// garbleDataIndex replaces every data index object with bytes that do not
// open.
func garbleDataIndex(t *testing.T, b ark.Backend, _ *ark.Repository) {
	t.Helper()
	ctx := context.Background()
	for _, name := range objectNames(t, b, ark.DataIndexFile) {
		h := ark.Handle{Type: ark.DataIndexFile, Name: name}
		if err := b.Delete(ctx, h); err != nil {
			t.Fatalf("delete index object: %v", err)
		}
		if err := b.Put(ctx, h, strings.NewReader("not an index object")); err != nil {
			t.Fatalf("put index object: %v", err)
		}
	}
}

// TestIndexObjectThatDoesNotOpenStoresTheMarker: the chunks of a data index
// object that does not open are stored but listed nowhere. A load that skips
// the object stores the marker, so the next backup reads a file with such a
// chunk again instead of keeping a reference no lookup resolves.
func TestIndexObjectThatDoesNotOpenStoresTheMarker(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	const content = "content of a file that does not change"
	src := newStatSource("app", map[string]statFile{
		"f": {content: content, mtime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	mustBackup(t, repo, src)
	garbleDataIndex(t, b, repo)

	mustCheck(t, repo)
	if n := len(objectNames(t, b, ark.LostPackFile)); n != 1 {
		t.Fatalf("%d markers after a check that skipped an index object, want 1", n)
	}
	snap := mustBackup(t, repo, src)
	if src.opens["f"] != 2 {
		t.Fatalf("the file was opened %d times, want twice", src.opens["f"])
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil || sink.written["f"] != content {
		t.Errorf("Restore = %q, %v; want the content", sink.written["f"], err)
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

// TestMarkerGoesWhileTheExclusiveLockIsHeld pins the order that keeps a
// reader's marker: a prune deletes it after it acquired the exclusive lock
// and before it releases it. A reader stores the marker under a shared lock,
// which cannot overlap that window, so the delete never removes a marker for
// a loss the new index still lists.
func TestMarkerGoesWhileTheExclusiveLockIsHeld(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	mustBackup(t, openRepo(t, b), smallFiles("app", 3))
	var events []string
	repo, err := ark.Open(ctx, b, testPassword, ark.WithLockEvent(func(ev ark.LockEvent) {
		if ev.Exclusive && (ev.Kind == ark.LockAcquired || ev.Kind == ark.LockReleased) {
			events = append(events, fmt.Sprintf("%s %d", ev.Kind, len(objectNames(t, b, ark.LostPackFile))))
		}
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lost := slices.Min(objectNames(t, b, ark.DataPackFile))
	if err := b.Delete(ctx, ark.Handle{Type: ark.DataPackFile, Name: lost}); err != nil {
		t.Fatalf("delete pack: %v", err)
	}
	mustCheck(t, repo)
	mustBackup(t, repo, smallFiles("app", 3))

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if want := []string{"acquired 1", "released 0"}; !slices.Equal(events, want) {
		t.Errorf("markers at the exclusive lock events = %q, want %q", events, want)
	}
}

// TestMarkerIsDurableOnceStored: a power loss after a read found the loss
// must not take the marker with it, or the next unchanged backup would not
// see the loss. The marker's Put is followed by a Sync.
func TestMarkerIsDurableOnceStored(t *testing.T) {
	ctx := context.Background()
	b := &syncRecordingBackend{Backend: arkmem.New()}
	repo := openRepo(t, b)
	snap := mustBackup(t, repo, smallFiles("app", 3))
	if err := b.Delete(ctx, ark.Handle{Type: ark.DataPackFile, Name: objectNames(t, b, ark.DataPackFile)[0]}); err != nil {
		t.Fatalf("delete pack: %v", err)
	}
	b.takeEvents()
	if _, err := repo.Verify(ctx, snap.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var ops []string
	for _, e := range b.takeEvents() {
		switch {
		case e.op == "sync":
			ops = append(ops, e.op)
		case e.h.Type != ark.LockFile:
			ops = append(ops, e.op+" "+e.h.Type.String())
		}
	}
	if want := []string{"put lostpacks", "sync"}; !slices.Equal(ops, want) {
		t.Errorf("Verify of a snapshot with a lost pack wrote %q, want %q", ops, want)
	}
	if _, err := repo.Verify(ctx, snap.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, e := range b.takeEvents() {
		if e.op == "sync" || e.h.Type != ark.LockFile {
			t.Errorf("Verify with the marker stored already wrote %s %s, want nothing", e.op, e.h.Type)
		}
	}
}

// failingMarkerBackend fails every request for the lost-pack marker's
// presence.
type failingMarkerBackend struct{ *opCountingBackend }

func (b failingMarkerBackend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	if h.Type == ark.LostPackFile {
		return false, errors.New("cannot ask")
	}
	return b.opCountingBackend.Exists(ctx, h)
}

// TestMarkerThatCannotBeAskedForCounts: a backup that cannot learn whether
// the marker is stored loads the data index, so it sees a lost pack a load
// stored no marker for.
func TestMarkerThatCannotBeAskedForCounts(t *testing.T) {
	ctx := context.Background()
	b := failingMarkerBackend{newOpCountingBackend()}
	repo := openRepo(t, b)
	const content = "content of a file that does not change"
	src := newStatSource("app", map[string]statFile{
		"f": {content: content, mtime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	mustBackup(t, repo, src)
	deletePackOf(t, b, repo, ark.BlobFile, ark.ObjectName(repo, []byte(content)))

	snap := mustBackup(t, repo, src)
	if src.opens["f"] != 2 {
		t.Fatalf("the file was opened %d times, want twice", src.opens["f"])
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil || sink.written["f"] != content {
		t.Errorf("Restore = %q, %v; want the content", sink.written["f"], err)
	}
}
