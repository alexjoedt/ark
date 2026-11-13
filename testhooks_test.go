package ark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

// OpenObject returns the raw content of an object of r for the tests in
// package ark_test, which cannot unseal an object themselves. A chunk or tree
// is read through the index from its pack, and also fails when its content
// does not hash to name.
func OpenObject(ctx context.Context, r *Repository, typ HandleType, name string) ([]byte, error) {
	chunks, trees := r.stores()
	//nolint:exhaustive // the other handle types share the sealed path
	switch typ {
	case BlobFile:
		return chunks.verifying().Get(ctx, name)
	case TreeFile:
		return trees.verifying().Get(ctx, name)
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

// LostPackMarker returns the handle of the lost-pack marker of r.
func LostPackMarker(r *Repository) Handle { return r.lostPacksMarker() }

// SealOverhead is what sealing adds to the zstd frame of an object.
const SealOverhead = sealOverhead

// ObjectName returns the name r stores a chunk or tree of this content under.
func ObjectName(r *Repository, data []byte) string { return keyedName(r.key.IDKey[:], data) }

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
// random names, perPack to a pack, and an empty object under the name of
// every pack: a load finds each listed pack stored, as in a whole repository.
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
		if err := r.backend.Put(ctx, Handle{Type: DataPackFile, Name: p.name}, bytes.NewReader(nil)); err != nil {
			return err
		}
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
	return t.count(), t, nil
}

// BlobNames returns the name of every chunk (BlobFile) or tree node
// (TreeFile) the index of r lists, sorted and each once.
func BlobNames(ctx context.Context, r *Repository, typ HandleType) ([]string, error) {
	t, err := newPackIndex(r.backend, newPackSpace(r.codec, r.key, typ), r.logger).load(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, records := range &t.records {
		for _, rec := range records {
			names = append(names, hex.EncodeToString(rec.name[:]))
		}
	}
	return slices.Compact(names), nil
}

// BlobPlace returns the pack that holds a chunk or tree node of r, and the
// offset and length of its sealed bytes in that pack.
func BlobPlace(ctx context.Context, r *Repository, typ HandleType, name string) (Handle, int64, int64, error) {
	space := newPackSpace(r.codec, r.key, typ)
	pack, e, ok, err := newPackIndex(r.backend, space, r.logger).lookup(ctx, name)
	if err == nil && !ok {
		err = fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return Handle{Type: space.packs, Name: pack}, int64(e.offset), int64(e.length), err
}

// SetClock replaces the clock of r, which times its locks and the index
// checkpoints of a backup.
func SetClock(r *Repository, now func() time.Time) { r.clock = now }

// IndexedPacks returns the names of the packs the index object under h lists
// and the number of blobs it lists in them.
func IndexedPacks(ctx context.Context, r *Repository, h Handle) ([]string, int, error) {
	typ := BlobFile
	if h.Type == TreeIndexFile {
		typ = TreeFile
	}
	var t indexTable
	if err := newPackIndex(r.backend, newPackSpace(r.codec, r.key, typ), r.logger).read(ctx, h.Name, &t); err != nil {
		return nil, 0, err
	}
	names := make([]string, len(t.packs))
	for i, p := range t.packs {
		names[i] = hex.EncodeToString(p[:])
	}
	return names, t.count(), nil
}

// SetPackLimits lowers the size that closes a pack of r and the number of
// blobs a run of r stores before it lists them in an index object, so a small
// source fills several packs and several index objects.
func SetPackLimits(r *Repository, packBytes, checkpointBlobs int) {
	r.packTarget, r.checkpointBlobs = packBytes, checkpointBlobs
}
