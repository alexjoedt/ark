package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// TestVerifyChunkDoesNotBlameTheChunkForALostIndexObject: an index object
// that was listed and is gone fails the load with ErrNotFound, which is no
// verdict on any chunk.
func TestVerifyChunkDoesNotBlameTheChunkForALostIndexObject(t *testing.T) {
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
	if err := verifyChunk(ctx, chunks, name, result); !errors.Is(err, b.getErr) {
		t.Errorf("verifyChunk returned %v, want the load error", err)
	}
	if !result.OK || len(result.MissingChunks) != 0 {
		t.Errorf("a failed index load reported the chunk: %+v", result)
	}

	b.getErr = nil
	if err := verifyChunk(ctx, chunks, name, result); err != nil || result.ChunksChecked != 1 {
		t.Errorf("verifyChunk after the index is back: %v, %d checked", err, result.ChunksChecked)
	}
}
