package ark

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"
)

// formatSealed is the format byte of every data object: a nonce and an
// XChaCha20-Poly1305 sealed zstd frame follow it. The first byte names the
// codec of everything after it, so a later codec gets a new byte.
const formatSealed byte = 0x02

// sealOverhead is what seal adds to the zstd frame: format byte, nonce, tag.
const sealOverhead = 1 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

var (
	// errSealBroken is returned by open when the tag does not verify.
	errSealBroken = errors.New("ark: sealed object does not authenticate")
	// errNameMismatch is returned by a verifying store when an object opens
	// but its content does not hash to its name.
	errNameMismatch = errors.New("ark: object content does not match its name")
	// errNotIndexed says why an object is missing: no index object lists it.
	// The pack that holds it may still be stored.
	errNotIndexed = errors.New("no index object lists it")
)

// objectCodec seals and opens data objects and key files.
// The zstd encoder and decoder are safe for concurrent use, so one codec
// serves every objectStore of a repository.
type objectCodec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newObjectCodec() *objectCodec {
	// These allocations never fail with the default options.
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	return &objectCodec{enc: enc, dec: dec}
}

// Close releases the pooled zstd encoder and decoder.
func (c *objectCodec) Close() {
	_ = c.enc.Close()
	c.dec.Close()
}

// seal returns the sealed form of data: formatSealed, a random nonce, then
// XChaCha20-Poly1305 over the zstd frame of data with aad bound to the tag.
func (c *objectCodec) seal(key *[32]byte, aad string, data []byte) []byte {
	aead, _ := chacha20poly1305.NewX(key[:]) // fails only on a wrong key size
	const hdr = 1 + chacha20poly1305.NonceSizeX
	out := make([]byte, hdr, sealOverhead+len(data)/2)
	out[0] = formatSealed
	rand.Read(out[1:hdr])
	// Compress behind the header and seal in place, one buffer for both.
	out = slices.Grow(c.enc.EncodeAll(data, out), chacha20poly1305.Overhead)
	return aead.Seal(out[:hdr], out[1:hdr], out[hdr:], []byte(aad))
}

// open reverses seal. A changed byte, an unknown format byte, a different key
// or a different aad all return errSealBroken.
func (c *objectCodec) open(key *[32]byte, aad string, obj []byte) ([]byte, error) {
	if len(obj) == 0 {
		return nil, fmt.Errorf("%w: object is empty", errSealBroken)
	}
	if obj[0] != formatSealed {
		return nil, fmt.Errorf("%w: unknown object format byte 0x%02x", errSealBroken, obj[0])
	}
	if len(obj) < sealOverhead {
		return nil, errSealBroken
	}
	aead, _ := chacha20poly1305.NewX(key[:])
	nonce := obj[1 : 1+chacha20poly1305.NonceSizeX]
	frame, err := aead.Open(nil, nonce, obj[1+chacha20poly1305.NonceSizeX:], []byte(aad))
	if err != nil {
		return nil, errSealBroken
	}
	return c.dec.DecodeAll(frame, nil)
}

// objectAAD is the associated data an object is sealed with, so an object
// moved to another name or namespace does not open.
func objectAAD(h Handle) string { return h.Type.String() + "/" + h.Name }

// putSealed seals data with the data key and writes it under h, replacing
// what is there. Manifests and locks go through it: their names are chosen,
// not derived from content.
func (r *Repository) putSealed(ctx context.Context, h Handle, data []byte) error {
	obj := r.codec.seal((*[32]byte)(&r.key.DataKey), objectAAD(h), data)
	if err := r.backend.Put(ctx, h, bytes.NewReader(obj)); err != nil {
		return fmt.Errorf("ark: failed to store %s object %s: %w", h.Type, h.Name, err)
	}
	return nil
}

// getSealed reads and opens the object under h with the data key.
func (r *Repository) getSealed(ctx context.Context, h Handle) ([]byte, error) {
	return readSealed(ctx, r.backend, r.codec, (*[32]byte)(&r.key.DataKey), h)
}

// readSealed reads and opens the object under h. Errors wrap ErrNotFound from
// the backend, and errSealBroken for an object that was changed or moved.
func readSealed(ctx context.Context, b Backend, c *objectCodec, key *[32]byte, h Handle) ([]byte, error) {
	obj, err := readWhole(ctx, b, h)
	if err != nil {
		return nil, err
	}
	data, err := c.open(key, objectAAD(h), obj)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to open %s object %s: %w", h.Type, h.Name, err)
	}
	return data, nil
}

// readWhole returns the whole object stored under h, as stored.
func readWhole(ctx context.Context, b Backend, h Handle) ([]byte, error) {
	rc, err := b.Get(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, h.Name, err)
	}
	defer rc.Close()

	obj, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, h.Name, err)
	}
	return obj, nil
}

// treePackCacheBytes bounds the whole tree packs an operation keeps in memory:
// a walk reads the nodes of one pack close together, so a pack is fetched
// once and its neighbours stay for the walks that follow.
const treePackCacheBytes = 64 << 20

// objectStore is a content-addressable store of the chunks or the tree nodes
// of a repository. An object is named by the HMAC-SHA256 of its raw content
// under the ID key and sealed with the data key, so its name reveals nothing
// without the keys and does not depend on how it is encoded. The sealed
// objects are stored in packs, and an object is present once an index object
// lists it.
//
// A store serves one operation: its index is loaded at the first lookup and
// never listed again, so it does not see what another writer stores later.
type objectStore struct {
	backend Backend
	space   *packSpace
	index   *packIndex
	// verify makes Get recompute the name of every object it reads.
	verify bool
	// packs holds whole packs, by name. With it Get fetches the pack of an
	// object whole and serves the objects beside it from memory; without it
	// every Get is one range read.
	packs *chunkCache
	// held and writer take what putNamed stores. They are set on the stores
	// of a packSession; any other store only reads.
	held   *heldBlobs
	writer *packWriter
}

// stores returns the chunk and the tree store of one operation. They issue no
// request before their first use.
func (r *Repository) stores() (chunks, trees *objectStore) {
	open := func(blobs HandleType) *objectStore {
		space := newPackSpace(r.codec, r.key, blobs)
		return &objectStore{backend: r.backend, space: space, index: newPackIndex(r.backend, space, r.logger)}
	}
	chunks, trees = open(BlobFile), open(TreeFile)
	trees.packs = newChunkCache(treePackCacheBytes)
	return chunks, trees
}

// verifying returns a copy of s whose Get fails with errNameMismatch when an
// object does not hash to its name.
func (s *objectStore) verifying() *objectStore {
	v := *s
	v.verify = true
	return &v
}

// name returns the object name of data: its HMAC-SHA256 under the ID key, hex.
func (s *objectStore) name(data []byte) string { return keyedName(s.space.idKey, data) }

// keyedName returns the name of an object that is named by its content: the
// HMAC-SHA256 of data under the ID key, hex.
func keyedName(idKey, data []byte) string {
	mac := hmac.New(sha256.New, idKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// putNamed stores data, whose name the caller holds, and returns the number
// of bytes it adds to a pack. A stored size of 0 means the object was already
// present, in the index or in a pack this run wrote, and nothing was written.
// The object is durable and present for others only once the session of s
// has listed its pack in an index object.
func (s *objectStore) putNamed(ctx context.Context, name string, data []byte) (int, error) {
	_, _, present, err := s.index.lookup(ctx, name)
	if err != nil || present {
		return 0, err
	}
	blob, _ := parseName(name) // lookup rejected a name that is none
	if !s.held.claim(blob) {
		return 0, nil
	}
	obj := s.space.codec.seal(s.space.dataKey, objectAAD(Handle{Type: s.space.blobs, Name: name}), data)
	if err := s.writer.add(ctx, name, obj); err != nil {
		s.held.release(blob)
		return 0, err
	}
	return len(obj), nil
}

// Get returns the raw content of the object stored under name. An object no
// index lists, or whose pack is gone, wraps ErrNotFound. One that was changed
// or moved from another name fails to open, with ErrCorruptChunk or
// ErrCorruptTree by namespace.
func (s *objectStore) Get(ctx context.Context, name string) ([]byte, error) {
	pack, e, ok, err := s.index.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w: %w",
			s.space.blobs, name, ErrNotFound, errNotIndexed)
	}
	var data []byte
	if s.packs == nil {
		data, err = s.space.readBlob(ctx, s.backend, pack, e)
	} else {
		data, err = s.fromWholePack(ctx, pack, e)
	}
	if err != nil {
		return nil, err
	}
	if s.verify && s.name(data) != name {
		return nil, fmt.Errorf("%w: %s object %s: %w", s.space.corruptErr, s.space.blobs, name, errNameMismatch)
	}
	return data, nil
}

// fromWholePack returns the content of the blob at e in the pack named pack,
// fetching the pack whole unless s holds it.
func (s *objectStore) fromWholePack(ctx context.Context, pack string, e packEntry) ([]byte, error) {
	whole, ok := s.packs.get(pack)
	for retried := false; ; retried = true {
		if !ok {
			var err error
			if whole, err = readWhole(ctx, s.backend, Handle{Type: s.space.packs, Name: pack}); err != nil {
				return nil, err
			}
			s.packs.put(pack, whole)
		}
		data, err := s.blobIn(pack, e, whole)
		if retried || !errors.Is(err, s.space.corruptErr) {
			return data, err
		}
		// A read that came back short or garbled looks like a damaged pack,
		// and what s holds is served to every later read: ask once more.
		s.packs.drop(pack)
		ok = false
	}
}

// blobIn returns the content of the blob at e in whole, the pack named pack.
func (s *objectStore) blobIn(pack string, e packEntry, whole []byte) ([]byte, error) {
	end := uint64(e.offset) + uint64(e.length)
	if end > uint64(len(whole)) {
		return nil, fmt.Errorf("%w: %s object %s ends before %x: %w",
			s.space.corruptErr, s.space.packs, pack, e.name, io.ErrUnexpectedEOF)
	}
	return s.space.openBlob(pack, e, whole[e.offset:end])
}
