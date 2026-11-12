package ark_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// dirOf is one directory of a treeOf source: a file per content, in order.
type dirOf struct {
	dir      string
	contents []string
}

// contents returns n file contents no other prefix yields, each its own chunk.
func contents(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s: content of chunk %03d", prefix, i)
	}
	return out
}

// treeOf returns a source of the given directories. Two sources that give a
// directory the same name and contents share its tree node, and equal
// contents share a chunk wherever they lie.
func treeOf(name string, dirs ...dirOf) multiSource {
	src := multiSource{name: name, files: map[string]string{}}
	for _, d := range dirs {
		src.dirs = append(src.dirs, d.dir)
		for i, c := range d.contents {
			p := fmt.Sprintf("%s/f%03d", d.dir, i)
			src.files[p] = c
			src.order = append(src.order, p)
		}
	}
	return src
}

// blobPlaces returns the pack and the sealed length of every blob of a type
// the index of repo lists.
func blobPlaces(
	t *testing.T,
	repo *ark.Repository,
	typ ark.HandleType,
) (packOf map[string]string, length map[string]int64) {
	t.Helper()
	packOf, length = map[string]string{}, map[string]int64{}
	for _, name := range blobNames(t, repo, typ) {
		h, _, n, err := ark.BlobPlace(context.Background(), repo, typ, name)
		if err != nil {
			t.Fatalf("BlobPlace %s/%s: %v", typ, name, err)
		}
		packOf[name], length[name] = h.Name, n
	}
	return packOf, length
}

func mustForget(t *testing.T, repo *ark.Repository, snaps ...*ark.Snapshot) {
	t.Helper()
	ids := make([]string, len(snaps))
	for i, s := range snaps {
		ids[i] = s.ID
	}
	if _, err := repo.Forget(context.Background(), ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: ids}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
}

// assertRestores fails unless the snapshot restores to the files of src.
func assertRestores(t *testing.T, repo *ark.Repository, snap *ark.Snapshot, src multiSource) {
	t.Helper()
	sink := &recordingSink{}
	if _, err := repo.Restore(context.Background(), snap.ID, sink); err != nil {
		t.Errorf("Restore of %s: %v", src.name, err)
		return
	}
	if !maps.Equal(sink.written, src.files) {
		t.Errorf("the restore of %s differs from its source", src.name)
	}
}

// packWrites returns the pack Puts and pack Deletes among the requests got.
func packWrites(got map[string]int) (puts, deletes int) {
	return got["put datapacks"] + got["put treepacks"], got["delete datapacks"] + got["delete treepacks"]
}

// TestPruneRepacksUntilTheUnusedBytesFitTheBudget forgets half of the
// snapshots. The data pack that is half dead is rewritten, the one with a
// single dead chunk is within the budget and stays as it is, the tree pack
// with one live node is rewritten whatever its share, and packs without a
// dead blob are not touched. The dry run reports the same and writes nothing.
func TestPruneRepacksUntilTheUnusedBytesFitTheBudget(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)

	a, c := contents("a", 10), contents("c", 40)
	one := treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)})
	two := treeOf("two", dirOf{"c", c})
	tail := treeOf("tail", dirOf{"t", contents("t", 5)})
	// Directory a as one has it, so its node is shared, and all but one chunk
	// of two under other paths.
	keep := treeOf("keep", dirOf{"a", a}, dirOf{"e", c[:39]})

	goneOne, goneTwo := mustBackup(t, repo, one), mustBackup(t, repo, two)
	keptTail, kept := mustBackup(t, repo, tail), mustBackup(t, repo, keep)
	mustForget(t, repo, goneOne, goneTwo)

	halfDead, _, _ := place(t, repo, a[0])
	nearlyLive, _, deadLen := place(t, repo, c[39])
	live, _, _ := place(t, repo, tail.files[tail.order[0]])
	before := packObjects(t, b)
	chunkPack, chunkLen := blobPlaces(t, repo, ark.BlobFile)
	nodePack, nodeLen := blobPlaces(t, repo, ark.TreeFile)
	if n := len(objectNames(t, b, ark.DataPackFile)); n != 3 || halfDead == nearlyLive {
		t.Fatalf("fixture: %d data packs, want one per backup with new chunks", n)
	}

	b.take()
	dry, err := repo.Prune(ctx, ark.WithPruneDryRun())
	if err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	for op, n := range b.take() {
		if strings.HasPrefix(op, "put ") || strings.HasPrefix(op, "delete ") {
			t.Errorf("the dry run issued %d %q", n, op)
		}
	}
	if !maps.Equal(packObjects(t, b), before) {
		t.Fatal("the dry run changed the packs or the index")
	}
	// A second handle that reports progress: one event before the copying
	// and one per pack that was copied from.
	var copying []ark.ProgressEvent
	watched, err := ark.Open(ctx, b, testPassword, ark.WithProgress(func(e ark.ProgressEvent) {
		if e.Phase == ark.PhaseStore {
			copying = append(copying, e)
		}
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer watched.Close()
	if _, err := watched.Prune(ctx, ark.WithPruneDryRun()); err != nil || len(copying) != 0 {
		t.Fatalf("watched dry run: %v, %d store events; want none", err, len(copying))
	}
	b.take()
	res, err := watched.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := b.take()
	if dry.DryRun = false; *dry != *res {
		t.Errorf("the dry run reports %+v, the prune %+v", *dry, *res)
	}
	if len(copying) != 3 || copying[0].BytesDone != 0 || copying[2].BytesDone != res.BytesRepacked ||
		copying[1].BytesDone <= 0 || copying[1].BytesDone >= res.BytesRepacked ||
		slices.ContainsFunc(copying, func(e ark.ProgressEvent) bool { return e.BytesTotal != res.BytesRepacked }) {
		t.Errorf("store events %+v, want 0, a part and all of %d bytes", copying, res.BytesRepacked)
	}

	// One new data pack for the ten live chunks of the half dead one, one new
	// tree pack for the node of directory a.
	if got["put datapacks"] != 1 || got["put treepacks"] != 1 {
		t.Errorf("prune stored %d data and %d tree packs, want 1 and 1 (%v)",
			got["put datapacks"], got["put treepacks"], got)
	}
	after := packObjects(t, b)
	for _, pack := range []string{nearlyLive, live} {
		if _, ok := after[ark.Handle{Type: ark.DataPackFile, Name: pack}]; !ok {
			t.Errorf("data pack %s is gone, want it kept as it was", pack)
		}
	}
	if _, ok := after[ark.Handle{Type: ark.DataPackFile, Name: halfDead}]; ok {
		t.Errorf("the half dead data pack %s is still stored", halfDead)
	}

	// What moved is what the result says was repacked, what stays unused is
	// the one dead chunk beside 39 live ones.
	var moved, stored int64
	var deleted int
	var freed int64
	nowChunk, nowChunkLen := blobPlaces(t, repo, ark.BlobFile)
	nowNode, nowNodeLen := blobPlaces(t, repo, ark.TreeFile)
	for name, pack := range nowChunk {
		stored += nowChunkLen[name]
		if pack != chunkPack[name] {
			moved += chunkLen[name]
		}
	}
	for name, pack := range nowNode {
		stored += nowNodeLen[name]
		if pack != nodePack[name] {
			moved += nodeLen[name]
		}
	}
	for h, size := range before {
		if _, ok := after[h]; !ok && (h.Type == ark.DataPackFile || h.Type == ark.TreePackFile) {
			deleted++
			freed += size
		}
	}
	removed := len(chunkPack) + len(nodePack) - len(nowChunk) - len(nowNode)
	want := ark.PruneResult{
		BlobsRemoved: removed, BytesFreed: freed - moved, PacksDeleted: deleted,
		BytesRepacked: moved, BytesUnused: deadLen,
	}
	if *res != want || moved == 0 || deleted != 3 || removed != 14 {
		t.Errorf("Prune = %+v, want %+v with 3 packs deleted and 14 blobs removed", *res, want)
	}
	if res.BytesUnused*100 > 5*stored {
		t.Errorf("%d of %d stored blob bytes are unused after the prune, want at most 5%%", res.BytesUnused, stored)
	}
	for _, content := range a {
		if pack, _, _ := place(t, repo, content); pack == halfDead {
			t.Errorf("a live chunk still resolves to the deleted pack %s", pack)
		}
	}

	assertRestores(t, repo, kept, keep)
	assertRestores(t, repo, keptTail, tail)
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check = %+v, want no findings", check)
	}

	// What is left fits the budget: a second prune writes and deletes nothing.
	b.take()
	if res, err := repo.Prune(ctx); err != nil || res.BytesRepacked != 0 || res.BytesUnused != deadLen {
		t.Errorf("second Prune = %+v, %v; want nothing repacked", res, err)
	}
	for op, n := range b.take() {
		if strings.HasPrefix(op, "put ") || strings.HasPrefix(op, "delete ") {
			t.Errorf("second prune issued %d %q", n, op)
		}
	}
}

// TestPruneAlwaysRewritesAPartlyDeadTreePack leaves one dead node, the root
// of the forgotten snapshot, beside ten live ones. That is far below the
// budget, which would leave a data pack alone, but a tree pack is read whole
// by every walk and is rewritten for any dead node.
func TestPruneAlwaysRewritesAPartlyDeadTreePack(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	var dirs []dirOf
	for i := range 10 {
		dirs = append(dirs, dirOf{fmt.Sprintf("d%02d", i), contents(fmt.Sprintf("d%02d", i), 20)})
	}
	keep := treeOf("keep", append(slices.Clone(dirs), dirOf{"x", contents("x", 1)})...)
	gone := mustBackup(t, repo, treeOf("one", dirs...))
	kept := mustBackup(t, repo, keep)
	mustForget(t, repo, gone)
	nodePack, nodeLen := blobPlaces(t, repo, ark.TreeFile)

	b.take()
	res, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := b.take()
	nowNode, _ := blobPlaces(t, repo, ark.TreeFile)
	var dead, stored, moved int64
	for name, n := range nodeLen {
		stored += n
		switch pack, ok := nowNode[name]; {
		case !ok:
			dead += n
		case pack != nodePack[name]:
			moved += n
		}
	}
	_, chunkLen := blobPlaces(t, repo, ark.BlobFile)
	for _, n := range chunkLen {
		stored += n
	}
	if len(nodePack)-len(nowNode) != 1 || dead*100 > 5*stored {
		t.Fatalf("fixture: %d dead nodes with %d of %d bytes, want one within the budget",
			len(nodePack)-len(nowNode), dead, stored)
	}
	want := ark.PruneResult{BlobsRemoved: 1, PacksDeleted: 1, BytesRepacked: moved, BytesFreed: res.BytesFreed}
	if puts, _ := packWrites(got); puts != 1 || got["put treepacks"] != 1 || got["delete treepacks"] != 1 ||
		*res != want || moved == 0 {
		t.Errorf("Prune = %+v, %v; want %+v with the tree pack rewritten and no data pack", *res, got, want)
	}
	assertRestores(t, repo, kept, keep)
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check = %+v, want no findings", check)
	}
}

// TestPruneLeavesALostPackToCheck: a pack that is gone cannot be copied from.
// Its live chunks stay in the index, where Check reports them, and the prune
// does the rest of its work.
func TestPruneLeavesALostPackToCheck(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	a := contents("a", 10)
	gone := mustBackup(t, repo, treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)}))
	mustBackup(t, repo, treeOf("keep", dirOf{"a", a}, dirOf{"e", contents("e", 3)}))
	mustForget(t, repo, gone)
	lost, _, _ := place(t, repo, a[0])
	deletePackOf(t, b, repo, ark.BlobFile, ark.ObjectName(repo, []byte(a[0])))

	b.take()
	res, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := b.take(); got["put datapacks"] != 0 || got["put treepacks"] != 1 || res.BytesUnused == 0 {
		t.Errorf(
			"Prune = %+v, %v; want the tree pack rewritten and the lost data pack counted as it is listed",
			*res,
			got,
		)
	}
	if now, _, _ := place(t, repo, a[0]); now != lost {
		t.Errorf("chunk of the lost pack resolves to %s, want its entry kept", now)
	}
	if check := mustCheck(t, repo); !naming(check.Errors, lost) {
		t.Errorf("Check errors = %v, want one naming the lost pack %s", check.Errors, lost)
	}
}

// TestPruneFinishesAnInterruptedRepack stops a prune after it copied the live
// blobs into new packs and listed them, before or while it rewrites the
// index. The blobs are then listed in the old pack and in the new one. The
// next prune counts them as stored in the new pack, which holds nothing
// unreferenced, and deletes the old pack without copying anything again.
func TestPruneFinishesAnInterruptedRepack(t *testing.T) {
	ctx := context.Background()
	// A repack lists its packs with one index object per blob type: the third
	// index Put of the prune is the first of the rewrite.
	for _, failAt := range []int{3, 4} {
		t.Run(fmt.Sprintf("index put %d", failAt), func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			a := contents("a", 10)
			keep := treeOf("keep", dirOf{"a", a}, dirOf{"e", contents("e", 3)})
			gone := mustBackup(t, repo, treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)}))
			kept := mustBackup(t, repo, keep)
			mustForget(t, repo, gone)
			oldPack, _, _ := place(t, repo, a[0])

			errInterrupted := errors.New("interrupted")
			indexPuts := 0
			b.mu.Lock()
			b.onPut = func(h ark.Handle) error {
				if h.Type != ark.DataIndexFile && h.Type != ark.TreeIndexFile {
					return nil
				}
				if indexPuts++; indexPuts >= failAt {
					return errInterrupted
				}
				return nil
			}
			b.mu.Unlock()
			b.take()
			if _, err := repo.Prune(ctx); !errors.Is(err, errInterrupted) {
				t.Fatalf("Prune = %v, want the interrupt", err)
			}
			b.mu.Lock()
			b.onPut = nil
			b.mu.Unlock()
			got := b.take()
			if puts, deletes := packWrites(
				got,
			); puts != 2 || deletes != 0 ||
				got["delete dataindex"]+got["delete treeindex"] != 0 {
				t.Fatalf(
					"the interrupted prune stored %d packs and deleted something: %v; want 2 packs, no delete",
					puts,
					got,
				)
			}
			assertRestores(t, repo, kept, keep)
			if check := mustCheck(t, repo); len(check.Errors) != 0 {
				t.Fatalf("Check after the interrupt = %+v, want no errors", check)
			}

			b.take()
			res, err := repo.Prune(ctx)
			if err != nil {
				t.Fatalf("second Prune: %v", err)
			}
			got = b.take()
			if puts, _ := packWrites(got); puts != 0 || res.BytesRepacked != 0 || res.BytesUnused != 0 {
				t.Errorf(
					"the second prune stored %d packs and reports %+v; want no pack written, nothing repacked",
					puts,
					res,
				)
			}
			if got["delete datapacks"] != 1 || got["delete treepacks"] != 1 {
				t.Errorf("the second prune deleted %d data and %d tree packs, want the old one of each (%v)",
					got["delete datapacks"], got["delete treepacks"], got)
			}
			if slices.Contains(objectNames(t, b, ark.DataPackFile), oldPack) {
				t.Errorf("the old data pack %s is still stored", oldPack)
			}
			if d, tr := len(
				objectNames(t, b, ark.DataIndexFile),
			), len(
				objectNames(t, b, ark.TreeIndexFile),
			); d != 1 ||
				tr != 1 {
				t.Errorf("%d data and %d tree index objects after the second prune, want 1 and 1", d, tr)
			}
			assertRestores(t, repo, kept, keep)
			if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
				t.Errorf("Check = %+v, want no findings", check)
			}
		})
	}
}

// TestPruneCountsABlobStoredTwiceOnce stores two chunks in two packs. While
// every blob of both packs is referenced, the second copies are unused bytes
// far above the budget and still no reason to rewrite a pack. Once one pack
// holds unreferenced chunks, the shared ones count as stored in the other,
// whichever name it has, and the pack goes without a rewrite.
func TestPruneCountsABlobStoredTwiceOnce(t *testing.T) {
	ctx := context.Background()
	for _, forgetFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("forget first %v", forgetFirst), func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			x := contents("x", 4)
			srcs := []multiSource{
				treeOf("first", dirOf{"x", x}),
				treeOf("second", dirOf{"y", slices.Concat(x[:2], contents("y", 2))}),
			}
			snaps := []*ark.Snapshot{mustBackup(t, repo, srcs[0])}
			// Without the data index the second backup stores x[0] and x[1] again.
			saved := map[string][]byte{}
			for _, name := range objectNames(t, b, ark.DataIndexFile) {
				saved[name] = readObject(t, b, ark.DataIndexFile, name)
				if err := b.Delete(ctx, ark.Handle{Type: ark.DataIndexFile, Name: name}); err != nil {
					t.Fatalf("delete index object: %v", err)
				}
			}
			snaps = append(snaps, mustBackup(t, repo, srcs[1]))
			for name, obj := range saved {
				putObject(t, b, ark.DataIndexFile, name, obj)
			}
			_, _, len0 := place(t, repo, x[0])
			_, _, len1 := place(t, repo, x[1])
			if n := len(objectNames(t, b, ark.DataPackFile)); n != 2 {
				t.Fatalf("fixture: %d data packs, want 2", n)
			}

			b.take()
			res, err := repo.Prune(ctx)
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			want := ark.PruneResult{BytesUnused: len0 + len1}
			if puts, deletes := packWrites(b.take()); puts != 0 || deletes != 0 || *res != want {
				t.Errorf("Prune with second copies only = %+v, %d pack puts, %d pack deletes; want %+v and none",
					*res, puts, deletes, want)
			}

			gone, kept := 0, 1
			if !forgetFirst {
				gone, kept = 1, 0
			}
			mustForget(t, repo, snaps[gone])
			b.take()
			res, err = repo.Prune(ctx)
			if err != nil {
				t.Fatalf("Prune after the forget: %v", err)
			}
			got := b.take()
			if puts, _ := packWrites(got); puts != 0 || got["delete datapacks"] != 1 ||
				res.BytesRepacked != 0 || res.BytesUnused != 0 {
				t.Errorf("Prune after the forget = %+v, %v; want one data pack deleted, none written", *res, got)
			}
			assertRestores(t, repo, snaps[kept], srcs[kept])
			if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
				t.Errorf("Check = %+v, want no findings", check)
			}
		})
	}
}

// TestPruneAbortsOnABlobThatDoesNotOpen changes one byte of a live chunk in a
// pack the prune is about to rewrite. A copy would carry the damage into a
// pack the index trusts, so the prune stops with every old pack and the index
// as they were.
func TestPruneAbortsOnABlobThatDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	a := contents("a", 10)
	keep := treeOf("keep", dirOf{"a", a}, dirOf{"e", contents("e", 3)})
	gone := mustBackup(t, repo, treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)}))
	kept := mustBackup(t, repo, keep)
	mustForget(t, repo, gone)

	victim := ark.ObjectName(repo, []byte(a[7]))
	intact := readBlob(t, b, repo, ark.BlobFile, victim)
	broken := slices.Clone(intact)
	broken[len(broken)-1] ^= 0x01
	writeBlob(t, b, repo, ark.BlobFile, victim, broken)
	before := packObjects(t, b)

	b.take()
	if _, err := repo.Prune(ctx); !errors.Is(err, ark.ErrCorruptChunk) || !strings.Contains(fmt.Sprint(err), victim) {
		t.Fatalf("Prune = %v, want ErrCorruptChunk naming %s", err, victim)
	}
	for op, n := range b.take() {
		if strings.HasPrefix(op, "delete ") {
			t.Errorf("the aborted prune issued %d %q", n, op)
		}
	}
	after := packObjects(t, b)
	for h := range before {
		if _, ok := after[h]; !ok {
			t.Errorf("the aborted prune removed %s/%s", h.Type, h.Name)
		}
	}

	writeBlob(t, b, repo, ark.BlobFile, victim, intact)
	if res, err := repo.Prune(ctx); err != nil || res.BytesRepacked == 0 {
		t.Fatalf("Prune with the chunk restored = %+v, %v; want a repack", res, err)
	}
	assertRestores(t, repo, kept, keep)
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check = %+v, want no findings", check)
	}
}

// TestPruneWritesInAnOrderAKillCannotBreak pins the steps of a prune that
// repacks: new packs, new index objects, the old index objects deleted, the
// old packs deleted, with a Sync between two steps and one at the end. A step
// that started before the Sync of the one before it could lose a blob to a
// power loss: an index object that lists a pack not yet durable, a deleted
// pack whose index entry comes back.
func TestPruneWritesInAnOrderAKillCannotBreak(t *testing.T) {
	ctx := context.Background()
	b := &syncRecordingBackend{Backend: arkmem.New()}
	repo := openRepo(t, b)
	a := contents("a", 10)
	keep := treeOf("keep", dirOf{"a", a}, dirOf{"e", contents("e", 3)})
	gone := mustBackup(t, repo, treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)}))
	kept := mustBackup(t, repo, keep)
	mustForget(t, repo, gone)

	b.takeEvents()
	if res, err := repo.Prune(ctx); err != nil || res.BytesRepacked == 0 {
		t.Fatalf("Prune = %+v, %v; want a repack", res, err)
	}
	steps := []string{"put pack", "put index", "delete index", "delete pack"}
	step, synced, seen := -1, true, map[int]int{}
	var last string
	for _, e := range b.takeEvents() {
		kind := ""
		switch e.h.Type { //nolint:exhaustive // locks come and go around the prune
		case ark.DataPackFile, ark.TreePackFile:
			kind = e.op + " pack"
		case ark.DataIndexFile, ark.TreeIndexFile:
			kind = e.op + " index"
		}
		if e.op == "sync" {
			synced, last = true, "sync"
			continue
		}
		n := slices.Index(steps, kind)
		if n < 0 {
			continue
		}
		last = kind
		seen[n]++
		switch {
		case n < step:
			t.Errorf("%q after %q", kind, steps[step])
		case n > step && !synced:
			t.Errorf("%q without a Sync after the last %q", kind, steps[step])
		}
		step, synced = n, false
	}
	// Both types repack: two packs, their two index objects and the two of
	// the rewrite, the two old index objects and the two the repack stored,
	// then the packs of the forgotten snapshot and the two that were copied.
	if seen[0] != 2 || seen[1] != 4 || seen[2] < 4 || seen[3] < 2 || last != "sync" {
		t.Errorf("prune stored %d packs and %d index objects, deleted %d index objects and %d packs, "+
			"last event %q; want 2, 4, at least 4 and 2, and a Sync last", seen[0], seen[1], seen[2], seen[3], last)
	}

	for _, typ := range []ark.HandleType{ark.DataIndexFile, ark.TreeIndexFile} {
		names := objectNames(t, b, typ)
		if len(names) != 1 {
			t.Fatalf("%d %s objects after the prune, want the index rewritten into one", len(names), typ)
		}
		listed, _, err := ark.IndexedPacks(ctx, repo, ark.Handle{Type: typ, Name: names[0]})
		stored := objectNames(t, b, ark.DataPackFile)
		if typ == ark.TreeIndexFile {
			stored = objectNames(t, b, ark.TreePackFile)
		}
		slices.Sort(listed)
		slices.Sort(stored)
		if err != nil || !slices.Equal(listed, stored) {
			t.Errorf("%s lists the packs %v, %v; stored are %v", typ, listed, err, stored)
		}
	}
	assertRestores(t, repo, kept, keep)
}

// cutBackend fails the write that would be number cut, and every write after
// it, the way a process that died there issues none. Locks are left alone:
// the lock of a dead process is a different matter.
type cutBackend struct {
	ark.Backend
	mu          sync.Mutex
	writes, cut int
}

var errCut = errors.New("cut")

func (b *cutBackend) write(h ark.Handle) error {
	if h.Type == ark.LockFile {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writes++; b.cut > 0 && b.writes >= b.cut {
		return errCut
	}
	return nil
}

func (b *cutBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := b.write(h); err != nil {
		return err
	}
	return b.Backend.Put(ctx, h, r)
}

func (b *cutBackend) Delete(ctx context.Context, h ark.Handle) error {
	if err := b.write(h); err != nil {
		return err
	}
	return b.Backend.Delete(ctx, h)
}

func (b *cutBackend) Sync(ctx context.Context) error {
	if err := b.write(ark.Handle{}); err != nil {
		return err
	}
	return b.Backend.Sync(ctx)
}

// TestPruneCutAtAnyWriteIsFinishedByTheNext stops a prune that deletes,
// repacks and rewrites the index before each of its Puts, Deletes and Syncs
// in turn. Whatever it got done, the repository has no error, every kept
// snapshot restores, and the next prune leaves nothing for a third.
func TestPruneCutAtAnyWriteIsFinishedByTheNext(t *testing.T) {
	ctx := context.Background()
	a, c := contents("a", 10), contents("c", 40)
	tail := treeOf("tail", dirOf{"t", contents("t", 5)})
	keep := treeOf("keep", dirOf{"a", a}, dirOf{"e", c[:39]})
	for cut := 1; ; cut++ {
		b := &cutBackend{Backend: arkmem.New()}
		repo := openRepo(t, b)
		goneOne := mustBackup(t, repo, treeOf("one", dirOf{"a", a}, dirOf{"b", contents("b", 10)}))
		goneTwo := mustBackup(t, repo, treeOf("two", dirOf{"c", c}))
		keptTail, kept := mustBackup(t, repo, tail), mustBackup(t, repo, keep)
		mustForget(t, repo, goneOne, goneTwo)

		b.mu.Lock()
		b.writes, b.cut = 0, cut
		b.mu.Unlock()
		_, err := repo.Prune(ctx)
		b.mu.Lock()
		writes := b.writes
		b.cut = 0
		b.mu.Unlock()
		if err == nil {
			// Two packs, their index objects, the rewrite, the deletes of the
			// index objects and the packs, and four Syncs.
			if writes < 20 {
				t.Fatalf("the whole prune issued %d writes, want the fixture to repack", writes)
			}
			return
		}
		if !errors.Is(err, errCut) {
			t.Fatalf("cut %d: Prune = %v, want the cut", cut, err)
		}
		if check := mustCheck(t, repo); len(check.Errors) != 0 {
			t.Fatalf("cut %d: Check = %+v, want no errors", cut, check)
		}
		assertRestores(t, repo, kept, keep)
		assertRestores(t, repo, keptTail, tail)

		if _, err := repo.Prune(ctx); err != nil {
			t.Fatalf("cut %d: second Prune: %v", cut, err)
		}
		if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
			t.Fatalf("cut %d: Check after the second prune = %+v, want no findings", cut, check)
		}
		assertRestores(t, repo, kept, keep)
		assertRestores(t, repo, keptTail, tail)
		b.mu.Lock()
		b.writes = 0
		b.mu.Unlock()
		res, err := repo.Prune(ctx)
		// An idle prune ends with its barrier and writes nothing else.
		if err != nil || b.writes != 1 || res.PacksDeleted != 0 || res.BytesRepacked != 0 {
			t.Fatalf("cut %d: third Prune = %+v, %v with %d writes; want nothing left to do", cut, res, err, b.writes)
		}
		if t.Failed() {
			t.Fatalf("cut %d", cut)
		}
	}
}
