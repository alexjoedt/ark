package ark

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"

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
	// errNotIndexed says why an object is missing: no index object lists it
	// in a pack that was stored when the index was loaded. A pack that holds
	// it may still be stored, unlisted.
	errNotIndexed = errors.New("no index object lists it in a stored pack")
	// errPackLost says why an object is missing: the index lists it only in
	// packs that were not stored when the index was loaded.
	errPackLost = errors.New("the index lists it only in packs that are not stored")
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
	frame, err := unseal(key, aad, obj)
	if err != nil {
		return nil, err
	}
	return c.dec.DecodeAll(frame, nil)
}

// unseal authenticates obj and returns the zstd frame it seals: all of open
// but the decompression.
func unseal(key *[32]byte, aad string, obj []byte) ([]byte, error) {
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
	return frame, nil
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
	packs *packCache
	// suspects, when set, collects the blobs Get could not open, by pack.
	// Check sets it: nothing that reads concurrently does.
	suspects map[string][]packEntry
	// held and writer take what putNamed stores. They are set on the stores
	// of a packSession; any other store only reads.
	held   *heldBlobs
	writer *packWriter
}

// stores returns the chunk and the tree store of one operation. They issue no
// request before their first use. A load of the chunk index that finds a lost
// data pack or an index object that does not open stores the lost-pack marker;
// a marker it fails to store is logged and the operation goes on, so a read,
// a check or a repair still runs on a repository that takes no write.
func (r *Repository) stores() (chunks, trees *objectStore) {
	open := func(blobs HandleType) *objectStore {
		space := newPackSpace(r.codec, r.key, blobs)
		return &objectStore{backend: r.backend, space: space, index: newPackIndex(r.backend, space, r.logger)}
	}
	chunks, trees = open(BlobFile), open(TreeFile)
	chunks.index.lost = func(ctx context.Context) error {
		if err := r.markLostPacks(ctx); err != nil {
			r.logger.WarnContext(ctx, "failed to store the lost-pack marker, a backup that takes files "+
				"unchanged from their parent does not see the loss until another index load stores it",
				"error", err)
		}
		return nil
	}
	trees.packs = newPackCache()
	return chunks, trees
}

// packCache is the LRU of whole tree packs of one operation. A pack pushed
// out is read by range for a while: fetching it whole for every blob would
// cost a pack per blob in a walk larger than the cache. Once those range reads
// have cost what the pack does, a request counted as packRangeGap bytes, it is
// fetched whole again, so the walks of many snapshots over the same trees do
// not end at one request per node either.
type packCache struct {
	*chunkCache
	mu sync.Mutex
	// credit is what the range reads of a pack may still cost, by pack. A
	// pack has none before it was fetched whole.
	credit map[string]int64
}

func newPackCache() *packCache {
	return &packCache{chunkCache: newChunkCache(treePackCacheBytes), credit: make(map[string]int64)}
}

// put holds whole, the pack named pack as it was just fetched.
func (c *packCache) put(pack string, whole []byte) {
	c.chunkCache.put(pack, whole)
	c.mu.Lock()
	c.credit[pack] = int64(len(whole))
	c.mu.Unlock()
}

// byRange reports whether a blob of length bytes in the pack named pack,
// which c does not hold, is read by range, and charges the pack for it.
func (c *packCache) byRange(pack string, length uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credit[pack] <= 0 {
		return false
	}
	c.credit[pack] -= int64(length) + packRangeGap
	return true
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
	pack, e, err := s.locate(ctx, name)
	if err != nil {
		return nil, err
	}
	var data []byte
	if s.packs == nil {
		data, err = s.space.readBlob(ctx, s.backend, pack, e)
	} else {
		data, err = s.fromWholePack(ctx, pack, e)
	}
	return s.checked(pack, e, data, err)
}

// locate returns the pack and the place in it of the object stored under name.
// An object no index lists in a stored pack wraps ErrNotFound and says
// whether a pack that is not stored lists it.
func (s *objectStore) locate(ctx context.Context, name string) (string, packEntry, error) {
	pack, e, ok, err := s.index.lookup(ctx, name)
	if err != nil || ok {
		return pack, e, err
	}
	listed, err := s.index.lists(ctx, name)
	if err != nil {
		return "", packEntry{}, err
	}
	why := errNotIndexed
	if listed {
		why = errPackLost
	}
	return "", packEntry{}, fmt.Errorf("ark: failed to read %s object %s: %w: %w",
		s.space.blobs, name, ErrNotFound, why)
}

// checked is what every read does with data and err, the outcome of reading
// and opening the blob at e of the pack named pack. A blob that did not open
// is noted when s collects such blobs, and a verifying store fails one that
// does not hash to its name.
func (s *objectStore) checked(pack string, e packEntry, data []byte, err error) ([]byte, error) {
	if err != nil {
		if s.suspects != nil && errors.Is(err, s.space.corruptErr) {
			s.suspects[pack] = append(s.suspects[pack], e)
		}
		return nil, err
	}
	if s.verify && s.name(data) != hex.EncodeToString(e.name[:]) {
		return nil, fmt.Errorf("%w: %s object %x: %w", s.space.corruptErr, s.space.blobs, e.name, errNameMismatch)
	}
	return data, nil
}

// getMany reads the blobs at entries, which are sorted by offset, from the
// pack named pack and calls fn for each with what a Get of it returns. Blobs
// at most packRangeGap apart share one range read, so every byte range is
// asked for once, and each blob is opened on its own: a changed byte fails
// the blob it lies in and no other. A pack that ends inside a range is asked
// for its size once, and the blobs behind its end fail without a request of
// their own. An error from fn ends the call and is returned.
//
// It reads by range whether or not s holds whole packs.
func (s *objectStore) getMany(
	ctx context.Context, pack string, entries []packEntry, fn func(e packEntry, data []byte, err error) error,
) error {
	return s.sealedMany(ctx, pack, entries, func(e packEntry, sealed []byte, err error) error {
		var data []byte
		if err == nil {
			data, err = s.space.openBlob(pack, e, sealed)
		}
		data, err = s.checked(pack, e, data, err)
		return fn(e, data, err)
	})
}

// sealedMany is getMany before a blob is opened: yield gets the sealed bytes of
// each blob as the pack holds them, which are only valid during the call, or
// the error of the read that should have returned them.
func (s *objectStore) sealedMany(
	ctx context.Context, pack string, entries []packEntry, yield func(e packEntry, sealed []byte, err error) error,
) error {
	h := Handle{Type: s.space.packs, Name: pack}
	// size is where the pack ends, once a read ran past it.
	size, sized := uint64(0), false
	for len(entries) > 0 {
		start := uint64(entries[0].offset)
		if sized && entries[0].end() > size {
			if err := yield(entries[0], nil, s.space.endsBefore(pack, entries[0], io.ErrUnexpectedEOF)); err != nil {
				return err
			}
			entries = entries[1:]
			continue
		}
		limit := uint64(math.MaxUint64)
		if sized {
			limit = size
		}
		n, stop := oneRange(entries, limit)
		whole, err := s.backend.GetRange(ctx, h, int64(start), int64(stop-start))
		if errors.Is(err, io.ErrUnexpectedEOF) && !sized {
			info, statErr := s.backend.Stat(ctx, h)
			if statErr == nil && uint64(info.Size) < stop {
				size, sized = uint64(info.Size), true
				continue
			}
			err = cmp.Or(statErr, err)
		}
		if err != nil {
			// A pack that does not answer fails every blob still wanted from
			// it: one request says so, not one per range.
			err = fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, pack, err)
			n = len(entries)
		}
		for _, e := range entries[:n] {
			var sealed []byte
			if err == nil {
				sealed = whole[uint64(e.offset)-start:][:e.length]
			}
			if err := yield(e, sealed, err); err != nil {
				return err
			}
		}
		entries = entries[n:]
	}
	return nil
}

// end is where the blob at e stops in its pack.
func (e packEntry) end() uint64 { return uint64(e.offset) + uint64(e.length) }

// oneRange returns how many of entries, which are sorted by offset, one range
// read takes and where it stops: the first, and those behind it that lie at
// most packRangeGap apart and end at limit or before.
func oneRange(entries []packEntry, limit uint64) (n int, stop uint64) {
	n, stop = 1, entries[0].end()
	for n < len(entries) && uint64(entries[n].offset) <= stop+packRangeGap && entries[n].end() <= limit {
		stop = max(stop, entries[n].end())
		n++
	}
	return n, stop
}

// fromWholePack returns the content of the blob at e in the pack named pack,
// fetching the pack whole unless s holds it or the walk pushed it out.
func (s *objectStore) fromWholePack(ctx context.Context, pack string, e packEntry) ([]byte, error) {
	whole, ok := s.packs.get(pack)
	if !ok && s.packs.byRange(pack, e.length) {
		return s.space.readBlob(ctx, s.backend, pack, e)
	}
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
		return nil, s.space.endsBefore(pack, e, io.ErrUnexpectedEOF)
	}
	return s.space.openBlob(pack, e, whole[e.offset:end])
}
