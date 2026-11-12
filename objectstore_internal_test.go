package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// testSession returns the write session of a repository on b that holds
// nothing but the key: enough for the stores, which tests of package ark
// cannot get from arkmem.
func testSession(b Backend, codec *objectCodec, key *masterKey) *packSession {
	return (&Repository{backend: b, codec: codec, key: key, logger: slog.Default()}).writeSession()
}

// TestVerifyingStoreRecomputesTheName covers what the tag cannot: an object
// sealed correctly under a name that is not the HMAC of its content, which
// only a writer bug produces. Plain reads trust the name, verifying reads
// recompute it.
func TestVerifyingStoreRecomputesTheName(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")

	session := testSession(newStubBackend(), codec, key)
	for _, s := range []*objectStore{session.chunks, session.trees} {
		typ := s.space.blobs
		data := []byte("content")
		wrong := s.name([]byte("other content"))
		name := s.name(data)
		for _, n := range []string{wrong, name} {
			if size, err := s.putNamed(ctx, n, data); err != nil || size == 0 {
				t.Fatalf("putNamed stored %d bytes: %v", size, err)
			}
			if size, err := s.putNamed(ctx, n, data); err != nil || size != 0 {
				t.Errorf("%s: second putNamed stored %d bytes, want 0: %v", typ, size, err)
			}
		}
		if _, err := s.Get(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Get before the index lists the object returned %v, want ErrNotFound", typ, err)
		}
		if err := session.finish(ctx); err != nil {
			t.Fatalf("finish: %v", err)
		}

		if _, err := s.Get(ctx, wrong); err != nil {
			t.Errorf("%s: plain Get: %v", typ, err)
		}
		if _, err := s.verifying().Get(ctx, wrong); !errors.Is(err, errNameMismatch) {
			t.Errorf("%s: verifying Get returned %v, want errNameMismatch", typ, err)
		}

		if _, err := s.verifying().Get(ctx, name); err != nil {
			t.Errorf("%s: verifying Get of a correct object: %v", typ, err)
		}

		// Verify reads chunks pack by pack, not through Get, and recomputes
		// the name as well.
		if typ != BlobFile {
			continue
		}
		refs := map[[sha256.Size]byte]int{}
		for _, n := range []string{wrong, name} {
			blob, _ := parseName(n)
			refs[blob] = 1
		}
		res := &VerifyResult{OK: true}
		if err := verifyChunks(ctx, s, refs, nil, res, func(int) {}); err != nil {
			t.Fatalf("verifyChunks: %v", err)
		}
		if res.OK || !slices.Equal(res.CorruptChunks, []string{wrong}) || res.ChunksChecked != 1 {
			t.Errorf("verifyChunks = %+v, want only %s corrupt and the other chunk checked", res, wrong)
		}
	}
}

// BenchmarkObjectCodec measures seal and open of a 1 MiB chunk, zstd
// included, for random and for compressible content.
func BenchmarkObjectCodec(b *testing.B) {
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	dataKey := (*[32]byte)(&key.DataKey)

	random := make([]byte, 1<<20)
	rand.Read(random)
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog "), (1<<20)/44+1)[:1<<20]

	for _, tc := range []struct {
		name string
		data []byte
	}{{"random", random}, {"text", text}} {
		obj := codec.seal(dataKey, "data/x", tc.data)
		b.Run("seal/"+tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.data)))
			for b.Loop() {
				codec.seal(dataKey, "data/x", tc.data)
			}
		})
		b.Run("open/"+tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.data)))
			for b.Loop() {
				if _, err := codec.open(dataKey, "data/x", obj); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestWholePackReadThatComesBackShortIsAskedAgain: a pack Get that returns
// half the pack without an error must not stay in the store, where every
// later read of the pack would see a damaged object that is stored intact.
func TestWholePackReadThatComesBackShortIsAskedAgain(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	b := newIndexBackend(newStubBackend())

	session := testSession(b, codec, key)
	var names []string
	for i := range 8 { // several of them lie past the cut
		data := fmt.Appendf(nil, "tree node %d", i)
		names = append(names, session.trees.name(data))
		if _, err := session.trees.putNamed(ctx, names[i], data); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.finish(ctx); err != nil {
		t.Fatal(err)
	}

	_, trees := (&Repository{backend: b, codec: codec, key: key, logger: quietLogger}).stores()
	if _, err := trees.index.load(ctx); err != nil {
		t.Fatal(err)
	}
	b.reset()
	b.cutGets = 1
	for _, name := range slices.Backward(names) {
		if _, err := trees.Get(ctx, name); err != nil {
			t.Fatalf("Get after a short pack read: %v", err)
		}
	}
	if _, gets, _ := b.calls(TreePackFile); gets != 2 {
		t.Errorf("%d Gets of the tree pack, want 2: the short one and one more", gets)
	}

	// A pack that is damaged where it is stored is read twice and no more.
	_, trees = (&Repository{backend: b, codec: codec, key: key, logger: quietLogger}).stores()
	if _, err := trees.index.load(ctx); err != nil {
		t.Fatal(err)
	}
	b.reset()
	b.cutGets = 2
	last := names[len(names)-1]
	if _, err := trees.Get(ctx, last); !errors.Is(err, ErrCorruptTree) {
		t.Errorf("Get of a pack that is short twice returned %v, want ErrCorruptTree", err)
	}
	if _, gets, _ := b.calls(TreePackFile); gets != 2 {
		t.Errorf("%d Gets of the damaged tree pack, want 2", gets)
	}
}

// TestTreePackPushedOutOfTheCacheIsReadByRangeForAWhile: walks whose tree
// packs do not fit the cache must neither fetch a pack whole for every node
// they read nor, once it was pushed out, ask for every node on its own for
// the rest of the operation, as the walks of many snapshots would.
func TestTreePackPushedOutOfTheCacheIsReadByRangeForAWhile(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	b := newIndexBackend(newStubBackend())

	session := testSession(b, codec, key)
	nodes := map[string][]byte{}
	for range 8 { // a pack of 8 MiB, worth a few requests
		data := make([]byte, 1<<20)
		rand.Read(data)
		nodes[session.trees.name(data)] = data
		if _, err := session.trees.putNamed(ctx, session.trees.name(data), data); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.finish(ctx); err != nil {
		t.Fatal(err)
	}

	_, trees := (&Repository{backend: b, codec: codec, key: key, logger: quietLogger}).stores()
	trees.packs.chunkCache = newChunkCache(1) // holds no pack
	if _, err := trees.index.load(ctx); err != nil {
		t.Fatal(err)
	}
	b.reset()
	const walks = 4
	for range walks {
		for name, want := range nodes {
			got, err := trees.Get(ctx, name)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("Get %s returned %d bytes, %v, want the %d stored", name, len(got), err, len(want))
			}
		}
	}
	reads := walks * len(nodes)
	if _, gets, total := b.calls(TreePackFile); gets < 2 || gets > reads/4 || total != reads {
		t.Errorf("%d whole reads of the tree pack and %d requests for %d node reads, want 2 to %d and %d",
			gets, total, reads, reads/4, reads)
	}
}

// TestTreePackWhoseFetchFailedIsFetchedWholeAgain: a pack the store never got
// is not one the walk pushed out, so the next node asks for it whole.
func TestTreePackWhoseFetchFailedIsFetchedWholeAgain(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	b := newIndexBackend(newStubBackend())

	session := testSession(b, codec, key)
	var names []string
	for i := range 8 {
		data := fmt.Appendf(nil, "tree node %d", i)
		names = append(names, session.trees.name(data))
		if _, err := session.trees.putNamed(ctx, names[i], data); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.finish(ctx); err != nil {
		t.Fatal(err)
	}

	_, trees := (&Repository{backend: b, codec: codec, key: key, logger: quietLogger}).stores()
	if _, err := trees.index.load(ctx); err != nil {
		t.Fatal(err)
	}
	b.getErr = errors.New("the backend is away")
	if _, err := trees.Get(ctx, names[0]); !errors.Is(err, b.getErr) {
		t.Fatalf("Get from a backend that is away returned %v", err)
	}
	b.getErr = nil
	b.reset()
	for _, name := range names {
		if _, err := trees.Get(ctx, name); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if _, gets, total := b.calls(TreePackFile); gets != 1 || total != 1 {
		t.Errorf("%d whole reads of the tree pack and %d requests for %d nodes, want 1 and 1", gets, total, len(names))
	}
}

// TestVerifyDoesNotBlameTheChunkForALostIndexObject: an index object
// that was listed and is gone fails the load with ErrNotFound, which is no
// verdict on any chunk.
func TestVerifyDoesNotBlameTheChunkForALostIndexObject(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	b := newIndexBackend(newStubBackend())

	session := testSession(b, codec, key)
	data := []byte("content")
	name := session.chunks.name(data)
	if _, err := session.chunks.putNamed(ctx, name, data); err != nil {
		t.Fatal(err)
	}
	if err := session.finish(ctx); err != nil {
		t.Fatal(err)
	}

	chunks, _ := (&Repository{backend: b, codec: codec, key: key, logger: quietLogger}).stores()
	b.getErr = fmt.Errorf("%w: the index object", ErrNotFound)
	result := &VerifyResult{OK: true}
	blob, _ := parseName(name)
	refs := map[[sha256.Size]byte]int{blob: 1}
	if err := verifyChunks(ctx, chunks, refs, nil, result, func(int) {}); !errors.Is(err, b.getErr) {
		t.Errorf("verifyChunks returned %v, want the load error", err)
	}
	if !result.OK || len(result.MissingChunks) != 0 {
		t.Errorf("a failed index load reported the chunk: %+v", result)
	}

	b.getErr = nil
	if err := verifyChunks(ctx, chunks, refs, nil, result, func(int) {}); err != nil || result.ChunksChecked != 1 {
		t.Errorf("verifyChunks after the index is back: %v, %d checked", err, result.ChunksChecked)
	}
}

// TestCheckComparesASuspectPackWithTheIndex: an index that lists a chunk at
// the place of another makes the chunk fail to open in a pack that is whole.
// The pack's own table shows it, and since the pack is named by its hash a
// rebuild repairs the index. A pack that cannot be read for the comparison
// is a warning: the check goes on and keeps what it found.
func TestCheckComparesASuspectPackWithTheIndex(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	b := newIndexBackend(newStubBackend())
	repo := &Repository{backend: b, codec: codec, key: key, logger: quietLogger}

	session := repo.writeSession()
	var names []string
	for _, content := range []string{"one chunk", "another"} {
		names = append(names, session.chunks.name([]byte(content)))
		if _, err := session.chunks.putNamed(ctx, names[len(names)-1], []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.finish(ctx); err != nil {
		t.Fatal(err)
	}

	chunks, _ := repo.stores()
	old, err := chunks.index.load(ctx)
	if err != nil || len(old.packs) != 1 {
		t.Fatalf("load: %v, %d packs, want one", err, len(old.packs))
	}
	packs := old.listed(slices.Repeat([]bool{true}, len(old.packs)))
	entries := packs[0].entries
	entries[0].name, entries[1].name = entries[1].name, entries[0].name
	for _, name := range old.objects {
		if err := b.Delete(ctx, Handle{Type: DataIndexFile, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	writeTestIndex(t, newPackIndex(b, chunks.space, quietLogger), packs)

	chunks, _ = repo.stores()
	chunks.suspects = make(map[string][]packEntry)
	if _, err := chunks.Get(ctx, names[0]); !errors.Is(err, ErrCorruptChunk) {
		t.Fatalf("Get under the wrong index = %v, want ErrCorruptChunk", err)
	}

	b.getErr = errors.New("the backend is away")
	result := &CheckResult{}
	if err := chunks.checkSuspects(ctx, result); err != nil {
		t.Errorf("checkSuspects with a pack that cannot be read = %v, want a finding and no error", err)
	}
	if len(result.Errors) != 0 || len(result.Warnings) != 1 ||
		!strings.Contains(result.Warnings[0], packs[0].name) || !strings.Contains(result.Warnings[0], "is away") {
		t.Errorf("findings %+v, want one warning naming pack %s and the read error", result, packs[0].name)
	}

	b.getErr = nil
	result = &CheckResult{}
	if err := chunks.checkSuspects(ctx, result); err != nil {
		t.Fatalf("checkSuspects: %v", err)
	}
	if len(result.Errors) != 1 || len(result.Warnings) != 0 ||
		!strings.Contains(result.Errors[0], packs[0].name+": its own table disagrees with the index") ||
		!strings.Contains(result.Errors[0], names[0]) ||
		!strings.Contains(result.Errors[0], "RebuildIndex restores the index from the packs") {
		t.Errorf("findings %+v, want one error saying that pack %s disagrees with the index about %s "+
			"and that RebuildIndex repairs it", result, packs[0].name, names[0])
	}
}
