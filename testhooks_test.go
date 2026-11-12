package ark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
)

// OpenObject returns the raw content of an object of r for the tests in
// package ark_test, which cannot unseal an object themselves. A chunk or tree
// also fails when its content does not hash to name.
func OpenObject(ctx context.Context, r *Repository, typ HandleType, name string) ([]byte, error) {
	//nolint:exhaustive // the other handle types share the sealed path
	switch typ {
	case BlobFile:
		return r.chunks.verifying().Get(ctx, name)
	case TreeFile:
		return r.trees.verifying().Get(ctx, name)
	}
	return r.getSealed(ctx, Handle{Type: typ, Name: name})
}

// PutObject seals data and stores it under h, the way r writes manifests and
// locks.
func PutObject(ctx context.Context, r *Repository, h Handle, data []byte) error {
	return r.putSealed(ctx, h, data)
}

// UnsealObject opens obj as r would read it back from h.
func UnsealObject(r *Repository, h Handle, obj []byte) ([]byte, error) {
	return r.codec.open((*[32]byte)(&r.key.DataKey), objectAAD(h), obj)
}

// SealOverhead is what sealing adds to the zstd frame of an object.
const SealOverhead = sealOverhead

// ObjectName returns the name r stores a chunk or tree of this content under.
func ObjectName(r *Repository, data []byte) string { return r.chunks.name(data) }

// RootTree returns the root tree hash of snapshot id.
func RootTree(ctx context.Context, r *Repository, id string) (string, error) {
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return "", err
	}
	return m.RootTree, nil
}

// SetPrefetchBytes lowers the floor of the restore prefetch budget of r for
// a test.
func SetPrefetchBytes(r *Repository, n int64) { r.prefetchBytes = n }

// LockClass names the class of a lock as lock errors do.
func LockClass(exclusive bool) string { return lockClass(exclusive) }

// WriteSyntheticIndex stores data index objects that list blobs blobs of
// random names, perPack to a pack, without storing a pack.
func WriteSyntheticIndex(ctx context.Context, r *Repository, blobs, perPack int) error {
	rng := rand.NewChaCha8([32]byte{13})
	packs := make([]indexedPack, 0, blobs/perPack+1)
	for left := blobs; left > 0; left -= perPack {
		var name [sha256.Size]byte
		_, _ = rng.Read(name[:])
		p := indexedPack{name: hex.EncodeToString(name[:]), entries: make([]packEntry, min(perPack, left))}
		for i := range p.entries {
			_, _ = rng.Read(p.entries[i].name[:])
			p.entries[i].length = 1 << 20
		}
		place(p.entries)
		packs = append(packs, p)
	}
	_, err := newPackIndex(r.backend, newPackSpace(r.codec, r.key, BlobFile), r.logger).write(ctx, packs)
	return err
}

// LoadDataIndex loads the data index of r from the backend, as the first
// lookup of a run does, and returns the number of blobs it lists with the
// table, for a caller that measures what the table keeps alive.
func LoadDataIndex(ctx context.Context, r *Repository) (int, any, error) {
	t, err := newPackIndex(r.backend, newPackSpace(r.codec, r.key, BlobFile), r.logger).load(ctx)
	if err != nil {
		return 0, nil, err
	}
	return len(t.records), t, nil
}
