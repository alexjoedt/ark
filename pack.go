package ark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

// A pack object holds many sealed blobs of one type, chunks or tree nodes:
//
//	sealed blobs in arrival order | sealed blob table | table length, 4 bytes little endian
//
// Each blob is sealed on its own with the name it would have as a single
// object, so one moved inside a pack or into another does not open. The table
// has one entry per blob, the 32-byte name and the sealed length; offsets
// follow from the order. It is sealed with the SHA-256 of the blob section in
// its associated data, so a table put behind other blobs does not open. The
// pack is named by the hex SHA-256 of all its bytes.
const (
	// packTargetSize closes a pack: one that holds at least this many blob
	// bytes after an append is uploaded, so it overshoots by one blob at most.
	packTargetSize = 16 << 20
	// packUploads bounds the pack uploads in flight of the writers that
	// share one set of upload slots.
	packUploads = 4
	// packEntrySize is one table entry: blob name and sealed length.
	packEntrySize = sha256.Size + 4
	// packTrailerSize is the length of the sealed table, at the pack's end.
	packTrailerSize = 4
)

// packEntry locates one sealed blob in a pack.
type packEntry struct {
	// name is the blob's object name, the 32 bytes its hex form encodes.
	name   [sha256.Size]byte
	offset uint32
	// length is the sealed length, what a range read asks for.
	length uint32
}

// packSpace is the pack namespace of one blob type with what sealing and
// opening its packs needs.
type packSpace struct {
	codec   *objectCodec
	dataKey *[32]byte
	// blobs is the namespace the blobs are sealed under, packs the one the
	// pack objects are stored in.
	blobs, packs HandleType
	// corruptErr is the sentinel a pack or blob that does not open wraps.
	corruptErr error
}

// newPackSpace returns the pack namespace of chunks (BlobFile) or tree nodes
// (TreeFile). Any other type panics: it is not stored in packs.
func newPackSpace(c *objectCodec, key *masterKey, blobs HandleType) *packSpace {
	s := &packSpace{codec: c, dataKey: (*[32]byte)(&key.DataKey), blobs: blobs}
	switch blobs { //nolint:exhaustive // only chunks and trees are packed
	case BlobFile:
		s.packs, s.corruptErr = DataPackFile, ErrCorruptChunk
	case TreeFile:
		s.packs, s.corruptErr = TreePackFile, ErrCorruptTree
	default:
		panic(fmt.Sprintf("ark: no packs for %s", blobs))
	}
	return s
}

// tableAAD binds a blob table to the blob section with the hash sum.
func (s *packSpace) tableAAD(sum []byte) string {
	return objectAAD(Handle{Type: s.packs, Name: hex.EncodeToString(sum)})
}

// finish appends the sealed table of entries and the trailer to the blob
// section and returns the pack with its name. The section is hashed once: the
// sum so far goes into the table's associated data, the final sum is the name.
func (s *packSpace) finish(blobs []byte, entries []packEntry) ([]byte, string) {
	table := make([]byte, 0, len(entries)*packEntrySize)
	for _, e := range entries {
		table = append(table, e.name[:]...)
		table = binary.LittleEndian.AppendUint32(table, e.length)
	}
	h := sha256.New()
	h.Write(blobs)
	tail := s.codec.seal(s.dataKey, s.tableAAD(h.Sum(nil)), table)
	tail = binary.LittleEndian.AppendUint32(tail, uint32(len(tail)))
	h.Write(tail)
	return append(blobs, tail...), hex.EncodeToString(h.Sum(nil))
}

// parse returns the blob table of a whole pack object. A pack that was
// changed, cut short or put together from parts of others fails with the
// corrupt error of its namespace.
func (s *packSpace) parse(pack []byte) ([]packEntry, error) {
	if len(pack) < packTrailerSize {
		return nil, fmt.Errorf("%w: %s object is shorter than its trailer", s.corruptErr, s.packs)
	}
	body := pack[:len(pack)-packTrailerSize]
	n := binary.LittleEndian.Uint32(pack[len(body):])
	if uint64(n) > uint64(len(body)) {
		return nil, fmt.Errorf("%w: %s object is shorter than its table", s.corruptErr, s.packs)
	}
	blobs, sealed := body[:len(body)-int(n)], body[len(body)-int(n):]

	sum := sha256.Sum256(blobs)
	table, err := s.codec.open(s.dataKey, s.tableAAD(sum[:]), sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to open the table of a %s object: %w", s.corruptErr, s.packs, err)
	}
	if len(table)%packEntrySize != 0 {
		return nil, fmt.Errorf("%w: table of a %s object has %d bytes", s.corruptErr, s.packs, len(table))
	}
	entries := make([]packEntry, len(table)/packEntrySize)
	var offset uint64
	for i := range entries {
		rec := table[i*packEntrySize:]
		e := &entries[i]
		copy(e.name[:], rec)
		e.length = binary.LittleEndian.Uint32(rec[len(e.name):])
		e.offset = uint32(offset) // an overflow fails the length check below
		offset += uint64(e.length)
	}
	if offset != uint64(len(blobs)) || offset > math.MaxUint32 {
		return nil, fmt.Errorf("%w: table of a %s object lists %d bytes, the pack holds %d",
			s.corruptErr, s.packs, offset, len(blobs))
	}
	return entries, nil
}

// readBlob returns the content of the blob at e in the pack named pack, with
// one range read. A blob that does not open, and a pack that ends before the
// blob does, fail with the corrupt error of the namespace.
func (s *packSpace) readBlob(ctx context.Context, b Backend, pack string, e packEntry) ([]byte, error) {
	name := hex.EncodeToString(e.name[:])
	obj, err := b.GetRange(ctx, Handle{Type: s.packs, Name: pack}, int64(e.offset), int64(e.length))
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("%w: %s object %s ends before %s: %w", s.corruptErr, s.packs, pack, name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", s.packs, pack, err)
	}
	data, err := s.codec.open(s.dataKey, objectAAD(Handle{Type: s.blobs, Name: name}), obj)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to open %s in %s object %s: %w", s.corruptErr, name, s.packs, pack, err)
	}
	return data, nil
}

// packWriter collects sealed blobs of one type into pack objects. It is safe
// for concurrent use.
type packWriter struct {
	space   *packSpace
	backend Backend
	// target is packTargetSize; tests lower it.
	target int
	// uploads holds one token per pack upload in flight.
	uploads chan struct{}
	// stored is called once per pack after its Put returned, from the
	// goroutine that uploaded it, so several calls may run at once.
	stored func(pack string, entries []packEntry)

	// mu guards the open pack.
	mu      sync.Mutex
	blobs   []byte
	entries []packEntry
}

// newPackUploads returns the upload slots the pack writers of one repository
// share.
func newPackUploads() chan struct{} { return make(chan struct{}, packUploads) }

// newPackWriter returns a writer into the packs of s. stored receives the
// name and blob table of every pack once it is stored; nothing else reports
// where a blob went.
func newPackWriter(
	b Backend, s *packSpace, uploads chan struct{}, stored func(pack string, entries []packEntry),
) *packWriter {
	return &packWriter{space: s, backend: b, target: packTargetSize, uploads: uploads, stored: stored}
}

// add appends the sealed blob stored under name to the open pack and uploads
// the pack once it reaches the target size. A blob of at least the target
// size gets a pack of its own. sealed is not kept.
func (w *packWriter) add(ctx context.Context, name string, sealed []byte) error {
	var e packEntry
	if len(name) != hex.EncodedLen(len(e.name)) {
		return fmt.Errorf("ark: invalid %s name %q", w.space.blobs, name)
	}
	if _, err := hex.Decode(e.name[:], []byte(name)); err != nil {
		return fmt.Errorf("ark: invalid %s name %q", w.space.blobs, name)
	}
	if uint64(len(sealed)) > math.MaxUint32 {
		return fmt.Errorf("ark: %s object %s is too large for a pack: %d bytes", w.space.blobs, name, len(sealed))
	}
	e.length = uint32(len(sealed))
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(sealed) >= w.target {
		if err := w.acquire(ctx); err != nil {
			return err
		}
		return w.store(ctx, bytes.Clone(sealed), []packEntry{e})
	}

	w.mu.Lock()
	e.offset = uint32(len(w.blobs)) // below target, the pack closes there
	w.entries = append(w.entries, e)
	w.blobs = append(w.blobs, sealed...)
	if len(w.blobs) < w.target {
		w.mu.Unlock()
		return nil
	}
	return w.storeOpen(ctx)
}

// flush uploads the open pack, however short. A run calls it once all its
// blobs are added.
func (w *packWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	if len(w.entries) == 0 {
		w.mu.Unlock()
		return nil
	}
	return w.storeOpen(ctx)
}

// storeOpen uploads the open pack and starts an empty one. The caller holds
// w.mu, storeOpen releases it. The upload slot is taken under the lock: while
// every slot is busy, adds wait here instead of filling packs that would
// queue up in memory.
func (w *packWriter) storeOpen(ctx context.Context) error {
	err := w.acquire(ctx)
	blobs, entries := w.blobs, w.entries
	if err == nil {
		w.blobs, w.entries = nil, nil
	}
	w.mu.Unlock()
	if err != nil {
		return err
	}
	return w.store(ctx, blobs, entries)
}

// acquire takes an upload slot; store gives it back.
func (w *packWriter) acquire(ctx context.Context) error {
	select {
	case w.uploads <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// store finishes the pack of blobs and entries and puts it. The caller holds
// an upload slot, which store releases once the Put returned.
func (w *packWriter) store(ctx context.Context, blobs []byte, entries []packEntry) error {
	pack, name := w.space.finish(blobs, entries)
	err := w.backend.Put(ctx, Handle{Type: w.space.packs, Name: name}, bytes.NewReader(pack))
	<-w.uploads
	if err != nil {
		return fmt.Errorf("ark: failed to store %s object %s: %w", w.space.packs, name, err)
	}
	w.stored(name, entries)
	return nil
}
