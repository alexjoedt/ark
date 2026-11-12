package ark_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// smallFiles returns a source of n files in two directories, each with its
// own content and so its own chunk, in the order a backup stores them.
func smallFiles(name string, n int) multiSource {
	src := multiSource{name: name, dirs: []string{"a", "b"}, files: map[string]string{}}
	for i := range n {
		p := fmt.Sprintf("%s/f%03d", src.dirs[i%2], i)
		src.files[p] = fmt.Sprintf("content of file %d of %s", i, name)
		src.order = append(src.order, p)
	}
	return src
}

func mustBackup(t *testing.T, repo *ark.Repository, src ark.Source) *ark.Snapshot {
	t.Helper()
	snap, err := repo.Backup(context.Background(), src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return snap
}

func mustCheck(t *testing.T, repo *ark.Repository) *ark.CheckResult {
	t.Helper()
	res, err := repo.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

// naming reports whether one of findings holds every one of parts.
func naming(findings []string, parts ...string) bool {
	return slices.ContainsFunc(findings, func(f string) bool {
		for _, p := range parts {
			if !strings.Contains(f, p) {
				return false
			}
		}
		return true
	})
}

// place returns the data pack that holds the chunk of content, and where.
func place(t *testing.T, repo *ark.Repository, content string) (pack string, offset, length int64) {
	t.Helper()
	h, offset, length, err := ark.BlobPlace(
		context.Background(), repo, ark.BlobFile, ark.ObjectName(repo, []byte(content)))
	if err != nil {
		t.Fatalf("BlobPlace: %v", err)
	}
	return h.Name, offset, length
}

// TestVerifyReadsEachPackRangeOnce pins what a verify costs: the chunks it
// needs from one pack come in one range read while they lie at most 1 MiB
// apart, no byte range is asked for twice, and progress arrives once per
// pack with the chunk references settled so far.
func TestVerifyReadsEachPackRangeOnce(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var fetches []ark.ProgressEvent
	// One writer, so the chunks reach the pack in the order of the source.
	repo, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(1), ark.WithProgress(func(e ark.ProgressEvent) {
		if e.Operation == ark.OperationVerify && e.Phase == ark.PhaseFetch {
			fetches = append(fetches, e)
		}
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// Three files stored in one pack, in this order: the middle one is wider
	// than the gap a range read spans.
	filler := make([]byte, 3<<20)
	_, _ = rand.Read(filler)
	all := multiSource{
		name:  "all",
		files: map[string]string{"first": "the first file", "middle": string(filler), "last": "the last file"},
		order: []string{"first", "middle", "last"},
	}
	// The outer two again, one of them twice, and a file of its own that
	// lands in a second pack.
	ends := multiSource{
		name: "ends",
		files: map[string]string{
			"first": "the first file", "again": "the first file", "last": "the last file", "own": "a new file",
		},
		order: []string{"first", "again", "last", "own"},
	}
	snapAll := mustBackup(t, repo, all)
	snapEnds := mustBackup(t, repo, ends)

	pack, firstAt, firstLen := place(t, repo, "the first file")
	lastPack, lastAt, _ := place(t, repo, "the last file")
	ownPack, _, _ := place(t, repo, "a new file")
	if pack != lastPack || lastAt-(firstAt+firstLen) <= 1<<20 || ownPack == pack || snapEnds.Stats.ChunksNew != 1 {
		t.Fatalf("fixture: first at %d+%d of %s, last at %d of %s, own in %s, %d new chunks; "+
			"want first and last in one pack more than 1 MiB apart and one new chunk in another",
			firstAt, firstLen, pack, lastAt, lastPack, ownPack, snapEnds.Stats.ChunksNew)
	}

	verify := func(snap *ark.Snapshot, wantReads, wantPacks, wantChunks int) {
		t.Helper()
		b.reset()
		fetches = nil
		res, err := repo.Verify(ctx, snap.ID)
		if err != nil || !res.OK || res.ChunksChecked != wantChunks {
			t.Fatalf("Verify = %+v, %v; want OK with %d chunks", res, err, wantChunks)
		}
		b.mu.Lock()
		gets := maps.Clone(b.gets)
		b.mu.Unlock()
		if len(gets) != wantReads {
			t.Errorf("verify read %d ranges of data packs, want %d: %v", len(gets), wantReads, gets)
		}
		for r, n := range gets {
			if n != 1 {
				t.Errorf("verify read %s %d times, want once", r, n)
			}
		}
		if len(fetches) != wantPacks {
			t.Fatalf("verify reported %d fetch events, want one per pack, %d", len(fetches), wantPacks)
		}
		done := 0
		for _, e := range fetches {
			if e.ChunksDone <= done || e.ChunksTotal != snap.Stats.ChunksTotal {
				t.Errorf("fetch event %d/%d after %d, want a growing count of %d references",
					e.ChunksDone, e.ChunksTotal, done, snap.Stats.ChunksTotal)
			}
			done = e.ChunksDone
		}
		if done != snap.Stats.ChunksTotal {
			t.Errorf("the last fetch event reports %d references, want all %d", done, snap.Stats.ChunksTotal)
		}
	}
	// Every chunk of the pack: one read. The two ends and the second pack:
	// three reads, and the file stored twice is read once.
	verify(snapAll, 1, 1, snapAll.Stats.ChunksTotal)
	verify(snapEnds, 3, 2, 3)
}

// TestVerifyBlamesExactlyTheChunksADamageCovers: every chunk is opened on its
// own, so a flipped byte fails the chunk it lies in and a pack that was cut
// off fails the chunks behind the cut, both as corrupt chunks of a verify
// that returns no error. The chunks beside them still count as checked.
func TestVerifyBlamesExactlyTheChunksADamageCovers(t *testing.T) {
	ctx := context.Background()
	src := smallFiles("app", 9)
	names := func(repo *ark.Repository, paths ...string) []string {
		var out []string
		for _, p := range paths {
			out = append(out, ark.ObjectName(repo, []byte(src.files[p])))
		}
		slices.Sort(out)
		return out
	}
	cases := []struct {
		name string
		// damage breaks the pack and returns the paths whose chunks it hit.
		damage func(t *testing.T, b *arkmem.Backend, repo *ark.Repository) []string
	}{
		{"flipped byte", func(t *testing.T, b *arkmem.Backend, repo *ark.Repository) []string {
			hit := src.order[4]
			name := ark.ObjectName(repo, []byte(src.files[hit]))
			obj := readBlob(t, b, repo, ark.BlobFile, name)
			obj[len(obj)/2] ^= 0x01
			writeBlob(t, b, repo, ark.BlobFile, name, obj)
			return []string{hit}
		}},
		{"cut off", func(t *testing.T, b *arkmem.Backend, repo *ark.Repository) []string {
			pack, offset, length := place(t, repo, src.files[src.order[4]])
			cut := offset + length/2
			whole := readObject(t, b, ark.DataPackFile, pack)
			putObject(t, b, ark.DataPackFile, pack, whole[:cut])
			var hit []string
			for _, p := range src.order {
				if _, at, n := place(t, repo, src.files[p]); at+n > cut {
					hit = append(hit, p)
				}
			}
			if len(hit) < 2 || len(hit) == len(src.order) {
				t.Fatalf("fixture: the cut hits %d of %d chunks, want some and not all", len(hit), len(src.order))
			}
			return hit
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := arkmem.New()
			repo := openRepo(t, b)
			snap := mustBackup(t, repo, src)
			want := names(repo, tc.damage(t, b, repo)...)

			res, err := repo.Verify(ctx, snap.ID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.OK || !slices.Equal(res.CorruptChunks, want) || len(res.MissingChunks) != 0 {
				t.Errorf("Verify = OK %v, corrupt %v, missing %v; want not OK with exactly the corrupt chunks %v",
					res.OK, res.CorruptChunks, res.MissingChunks, want)
			}
			if res.ChunksChecked != len(src.files)-len(want) {
				t.Errorf("Verify checked %d chunks, want the %d intact ones",
					res.ChunksChecked, len(src.files)-len(want))
			}
			check := mustCheck(t, repo)
			if len(check.Errors) != len(want) {
				t.Errorf("Check reports %d errors, want one per corrupt chunk, %d: %v",
					len(check.Errors), len(want), check.Errors)
			}
			for _, chunk := range want {
				if !naming(check.Errors, snap.ID, "corrupt chunk "+chunk) {
					t.Errorf("Check errors %q name no corrupt chunk %s of snapshot %s", check.Errors, chunk, snap.ID)
				}
			}
		})
	}
}

// TestCheckReportsAPackThatDisagreesWithTheIndex stores two packs under each
// other's name. Every blob in them fails to open where the index says it is,
// and the table each pack carries shows why: it lists other blobs. Check says
// so per pack, naming the blob the index expects there, beside the finding of
// each snapshot that could not read it.
func TestCheckReportsAPackThatDisagreesWithTheIndex(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []ark.HandleType{ark.BlobFile, ark.TreeFile} {
		t.Run(typ.String(), func(t *testing.T) {
			b := arkmem.New()
			repo := openRepo(t, b)
			contents := []string{"contents of one", "what the second holds"}
			var snaps, blobs []string
			var packs []ark.Handle
			for _, content := range contents {
				snap := mustBackup(t, repo, stubSource{name: content, content: content})
				blob := ark.ObjectName(repo, []byte(content))
				if typ == ark.TreeFile {
					var err error
					if blob, err = ark.RootTree(ctx, repo, snap.ID); err != nil {
						t.Fatalf("RootTree: %v", err)
					}
				}
				pack, _, _, err := ark.BlobPlace(ctx, repo, typ, blob)
				if err != nil {
					t.Fatalf("BlobPlace: %v", err)
				}
				snaps, blobs, packs = append(snaps, snap.ID), append(blobs, blob), append(packs, pack)
			}
			first := readObject(t, b, packs[0].Type, packs[0].Name)
			putObject(t, b, packs[0].Type, packs[0].Name, readObject(t, b, packs[1].Type, packs[1].Name))
			putObject(t, b, packs[1].Type, packs[1].Name, first)

			check := mustCheck(t, repo)
			for i, pack := range packs {
				if !naming(check.Errors, pack.Type.String()+" object "+pack.Name, "disagrees with the index",
					blobs[i], "RebuildIndex") {
					t.Errorf("Check errors %q have none saying that pack %s disagrees with the index about %s",
						check.Errors, pack.Name, blobs[i])
				}
				if !naming(check.Errors, "snapshot "+snaps[i], blobs[i]) {
					t.Errorf("Check errors %q have none of snapshot %s naming %s", check.Errors, snaps[i], blobs[i])
				}
			}
			if len(check.Errors) != 4 {
				t.Errorf("Check reports %d errors, want one per pack and one per snapshot: %v",
					len(check.Errors), check.Errors)
			}
		})
	}
}

// TestCheckWarnsAboutWhatPruneRemoves: a pack no index lists and an index
// object that does not open cost no snapshot anything, so they are warnings
// and the check passes. The index object that lists a referenced chunk is
// another matter: without it the chunk is missing, which is an error.
func TestCheckWarnsAboutWhatPruneRemoves(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	snap := mustBackup(t, repo, smallFiles("app", 5))

	stray := []ark.Handle{
		{Type: ark.DataPackFile, Name: strings.Repeat("0a", 32)},
		{Type: ark.TreePackFile, Name: strings.Repeat("0b", 32)},
		{Type: ark.DataIndexFile, Name: strings.Repeat("0c", 32)},
		{Type: ark.TreeIndexFile, Name: strings.Repeat("0d", 32)},
	}
	for _, h := range stray {
		putObject(t, b, h.Type, h.Name, []byte("left behind by something that never finished"))
	}

	check := mustCheck(t, repo)
	if len(check.Errors) != 0 {
		t.Errorf("Check reports errors over objects no snapshot needs: %v", check.Errors)
	}
	for _, h := range stray {
		detail := "no index lists"
		if h.Type == ark.DataIndexFile || h.Type == ark.TreeIndexFile {
			detail = "does not open"
		}
		if !naming(check.Warnings, h.Type.String(), h.Name, detail) {
			t.Errorf("Check warnings %q have none naming %s object %s with %q", check.Warnings, h.Type, h.Name, detail)
		}
	}
	if len(check.Warnings) != len(stray) {
		t.Errorf("Check reports %d warnings, want %d: %v", len(check.Warnings), len(stray), check.Warnings)
	}
	if res, err := repo.Verify(ctx, snap.ID); err != nil || !res.OK {
		t.Errorf("Verify beside the stray objects = %+v, %v; want OK", res, err)
	}

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check after the prune = %+v, want no findings", check)
	}

	for _, name := range objectNames(t, b, ark.DataIndexFile) {
		if err := b.Delete(ctx, ark.Handle{Type: ark.DataIndexFile, Name: name}); err != nil {
			t.Fatalf("delete index object: %v", err)
		}
	}
	check = mustCheck(t, repo)
	chunks := 0
	for _, e := range check.Errors {
		if strings.Contains(e, snap.ID) && strings.Contains(e, "missing chunk") {
			chunks++
		}
	}
	if chunks != 5 || len(check.Errors) != 5 {
		t.Errorf("Check without the data index reports %v, want the 5 chunks of %s as missing", check.Errors, snap.ID)
	}
	if !naming(check.Warnings, "datapacks", "no index lists", "RebuildIndex") {
		t.Errorf("Check warnings %q do not say that the pack is unlisted and RebuildIndex lists it", check.Warnings)
	}
}

// TestRebuildIndexRestoresALostIndex deletes every index object. The packs
// describe themselves, so RebuildIndex brings back an index under which the
// check passes and every snapshot restores what was backed up.
func TestRebuildIndexRestoresALostIndex(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	first := smallFiles("app", 40)
	second := smallFiles("app", 60)
	second.files[second.order[3]] = "changed in the second backup"
	sources := map[string]multiSource{
		mustBackup(t, repo, first).ID:  first,
		mustBackup(t, repo, second).ID: second,
	}
	packs := len(objectNames(t, b, ark.DataPackFile)) + len(objectNames(t, b, ark.TreePackFile))
	blobs := len(blobNames(t, repo, ark.BlobFile)) + len(blobNames(t, repo, ark.TreeFile))

	for _, typ := range []ark.HandleType{ark.DataIndexFile, ark.TreeIndexFile} {
		for _, name := range objectNames(t, b, typ) {
			if err := b.Delete(ctx, ark.Handle{Type: typ, Name: name}); err != nil {
				t.Fatalf("delete index object: %v", err)
			}
		}
	}
	if check := mustCheck(t, repo); len(check.Errors) != len(sources) {
		t.Fatalf("Check without an index reports %v, want one error per snapshot", check.Errors)
	}
	if _, err := repo.Prune(ctx); !errors.Is(err, ark.ErrNotFound) || !strings.Contains(err.Error(), "RebuildIndex") {
		t.Fatalf("Prune without an index = %v, want ErrNotFound naming RebuildIndex", err)
	}

	putForeignLock(t, repo, false)
	if _, err := repo.RebuildIndex(ctx); !errors.Is(err, ark.ErrRepoLocked) {
		t.Errorf("RebuildIndex beside a shared lock = %v, want ErrRepoLocked", err)
	}
	if _, err := repo.Unlock(ctx, ark.WithForce()); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	res, err := repo.RebuildIndex(ctx)
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if res.PacksIndexed != packs || res.BlobsIndexed != blobs || len(res.UnreadablePacks) != 0 {
		t.Errorf("RebuildIndex = %+v, want %d packs with %d blobs and none unreadable", res, packs, blobs)
	}
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check after the rebuild = %+v, want no findings", check)
	}
	for id, src := range sources {
		sink := &recordingSink{}
		if _, err := repo.Restore(ctx, id, sink); err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		if !maps.Equal(sink.written, src.files) {
			t.Errorf("restore of %s wrote %d files that differ from the %d backed up",
				id, len(sink.written), len(src.files))
		}
	}

	// A rebuild over a complete index replaces it: the index objects of two
	// backups become one per type, and nothing a reader sees changes.
	mustBackup(t, repo, smallFiles("later", 10))
	count := func() int {
		return len(objectNames(t, b, ark.DataIndexFile)) + len(objectNames(t, b, ark.TreeIndexFile))
	}
	if before := count(); before != 4 {
		t.Fatalf("fixture: %d index objects after another backup, want 4", before)
	}
	if _, err := repo.RebuildIndex(ctx); err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}
	if after := count(); after != 2 {
		t.Errorf("%d index objects after a rebuild over a complete index, want one per type", after)
	}
	if check := mustCheck(t, repo); len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check after the second rebuild = %+v, want no findings", check)
	}
	if res, err := repo.Prune(ctx); err != nil || res.BlobsRemoved != 0 {
		t.Errorf("Prune after the rebuild = %+v, %v; want nothing removed", res, err)
	}
}

// TestRebuildIndexKeepsWhatTheOldIndexKnowsOfADamagedPack: one flipped byte
// makes the table of a pack unreadable, and with it the place of every chunk
// beside the damaged one. A rebuild names the pack and keeps the old entries,
// so those chunks stay readable. Once the old index is gone as well, nothing
// locates them: they are missing, never other bytes.
func TestRebuildIndexKeepsWhatTheOldIndexKnowsOfADamagedPack(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	src := smallFiles("app", 6)
	snap := mustBackup(t, repo, src)
	// A second pack is lost altogether: that one the rebuild forgets.
	lost := mustBackup(t, repo, stubSource{name: "lost", content: "in a pack that goes missing"})
	deletePackOf(t, b, repo, ark.BlobFile, ark.ObjectName(repo, []byte("in a pack that goes missing")))
	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{lost.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	broken := ark.ObjectName(repo, []byte(src.files[src.order[2]]))
	pack, _, _ := place(t, repo, src.files[src.order[2]])
	obj := readBlob(t, b, repo, ark.BlobFile, broken)
	obj[len(obj)/2] ^= 0x01
	writeBlob(t, b, repo, ark.BlobFile, broken, obj)

	res, err := repo.RebuildIndex(ctx)
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if !slices.Equal(res.UnreadablePacks, []string{pack}) {
		t.Errorf("RebuildIndex names the unreadable packs %v, want [%s]", res.UnreadablePacks, pack)
	}
	vr, err := repo.Verify(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !slices.Equal(vr.CorruptChunks, []string{broken}) || len(vr.MissingChunks) != 0 || vr.ChunksChecked != 5 {
		t.Errorf("Verify after the rebuild = %+v, want only chunk %s corrupt and the other 5 checked", vr, broken)
	}
	if check := mustCheck(t, repo); len(check.Errors) != 1 || naming(check.Errors, "not stored") {
		t.Errorf("Check after the rebuild reports %q, want the corrupt chunk and no pack that is not stored",
			check.Errors)
	}

	for _, name := range objectNames(t, b, ark.DataIndexFile) {
		if err := b.Delete(ctx, ark.Handle{Type: ark.DataIndexFile, Name: name}); err != nil {
			t.Fatalf("delete index object: %v", err)
		}
	}
	if _, err := repo.RebuildIndex(ctx); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	vr, err = repo.Verify(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(vr.MissingChunks) != 6 || vr.ChunksChecked != 0 {
		t.Errorf("Verify without the old index = %+v, want all 6 chunks of the damaged pack missing", vr)
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Restore = %v, want ErrNotFound", err)
	}
	for p, got := range sink.written {
		if got != "" && got != src.files[p] {
			t.Errorf("restore wrote other bytes for %s", p)
		}
	}
}
