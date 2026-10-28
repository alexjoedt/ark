package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
)

// TestVerifyingStoreRecomputesTheName covers what the tag cannot: an object
// sealed correctly under a name that is not the HMAC of its content, which
// only a writer bug produces. Plain reads trust the name, verifying reads
// recompute it.
func TestVerifyingStoreRecomputesTheName(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")

	for _, typ := range []HandleType{BlobFile, TreeFile} {
		s := newObjectStore(newStubBackend(), typ, codec, key)
		data := []byte("content")
		wrong := s.name([]byte("other content"))
		if _, err := s.putNamed(ctx, wrong, data); err != nil {
			t.Fatalf("putNamed: %v", err)
		}

		if _, err := s.Get(ctx, wrong); err != nil {
			t.Errorf("%s: plain Get: %v", typ, err)
		}
		if _, err := s.verifying().Get(ctx, wrong); !errors.Is(err, errNameMismatch) {
			t.Errorf("%s: verifying Get returned %v, want errNameMismatch", typ, err)
		}

		name, _, err := s.Put(ctx, data)
		if err != nil {
			t.Fatalf("Put: %v", err)
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
