package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
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
