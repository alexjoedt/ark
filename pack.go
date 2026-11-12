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
	"sync/atomic"
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
	name [sha256.Size]byte
	// offset follows from the lengths before it: place sets it, nothing else.
	offset uint32
	// length is the sealed length, what a range read asks for.
	length uint32
}

// packSpace is the pack namespace of one blob type with what sealing and
// opening its packs needs.
type packSpace struct {
	codec   *objectCodec
	dataKey *[32]byte
	idKey   []byte
	// blobs is the namespace the blobs are sealed under, packs the one the
	// pack objects are stored in, index the one of the objects that say which
	// pack holds a blob.
	blobs, packs, index HandleType
	// corruptErr is the sentinel a pack or blob that does not open wraps.
	corruptErr error
}

// newPackSpace returns the pack namespace of chunks (BlobFile) or tree nodes
// (TreeFile). Any other type panics: it is not stored in packs.
func newPackSpace(c *objectCodec, key *masterKey, blobs HandleType) *packSpace {
	s := &packSpace{codec: c, dataKey: (*[32]byte)(&key.DataKey), idKey: key.IDKey[:], blobs: blobs}
	switch blobs { //nolint:exhaustive // only chunks and trees are packed
	case BlobFile:
		s.packs, s.index, s.corruptErr = DataPackFile, DataIndexFile, ErrCorruptChunk
	case TreeFile:
		s.packs, s.index, s.corruptErr = TreePackFile, TreeIndexFile, ErrCorruptTree
	default:
		panic(fmt.Sprintf("ark: no packs for %s", blobs))
	}
	return s
}

// parseName returns the 32 bytes a blob or pack name spells. Only the bytes are
// kept and a reader spells them in lower case, so a name in any other spelling
// is none: it would seal a blob that never opens.
func parseName(name string) (b [sha256.Size]byte, ok bool) {
	if len(name) != hex.EncodedLen(len(b)) {
		return b, false
	}
	_, err := hex.Decode(b[:], []byte(name))
	return b, err == nil && hex.EncodeToString(b[:]) == name
}

// tableAAD binds a blob table to the blob section with the hash sum.
func (s *packSpace) tableAAD(sum []byte) string {
	return objectAAD(Handle{Type: s.packs, Name: hex.EncodeToString(sum)})
}

// place sets the offset of every entry from the lengths before it and returns
// the length of the blob section the entries describe. An offset that does not
// fit wraps: the caller checks the returned length.
func place(entries []packEntry) uint64 {
	var offset uint64
	for i := range entries {
		entries[i].offset = uint32(offset)
		offset += uint64(entries[i].length)
	}
	return offset
}

// finish places entries and returns what ends the pack whose blob section is
// the parts of section in order, the sealed table and the trailer, with the
// pack's name. The section is hashed once: the sum so far goes into the
// table's associated data, the final sum is the name.
func (s *packSpace) finish(entries []packEntry, section ...[]byte) ([]byte, string) {
	place(entries)
	table := make([]byte, 0, len(entries)*packEntrySize)
	for _, e := range entries {
		table = append(table, e.name[:]...)
		table = binary.LittleEndian.AppendUint32(table, e.length)
	}
	h := sha256.New()
	for _, part := range section {
		h.Write(part)
	}
	tail := s.codec.seal(s.dataKey, s.tableAAD(h.Sum(nil)), table)
	tail = binary.LittleEndian.AppendUint32(tail, uint32(len(tail)))
	h.Write(tail)
	return tail, hex.EncodeToString(h.Sum(nil))
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
	for i := range entries {
		rec := table[i*packEntrySize:]
		e := &entries[i]
		copy(e.name[:], rec)
		e.length = binary.LittleEndian.Uint32(rec[len(e.name):])
	}
	if listed := place(entries); listed != uint64(len(blobs)) || listed > math.MaxUint32 {
		return nil, fmt.Errorf("%w: table of a %s object lists %d bytes, the pack holds %d",
			s.corruptErr, s.packs, listed, len(blobs))
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
// for concurrent use. A pack that fails to upload fails the writer: the pack
// held blobs whose add had returned, so every later add and flush returns
// that first error and a run cannot end well without them.
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

	// open holds a token while a caller works on the open pack, blobs and
	// entries: a mutex whose wait ends with the caller's context. The open
	// pack stays below target, the blob that would fill it is uploaded with
	// it and never copied in.
	open    chan struct{}
	blobs   []byte
	entries []packEntry

	failed atomic.Pointer[error]
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
	return &packWriter{
		space: s, backend: b, target: packTargetSize, uploads: uploads, stored: stored,
		open: make(chan struct{}, 1),
	}
}

// take puts a token into sem and fails without one once ctx ends, also when
// ctx had ended before the call.
func take(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-sem
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// failure returns the error of the first pack that was not stored.
func (w *packWriter) failure() error {
	if err := w.failed.Load(); err != nil {
		return *err
	}
	return nil
}

// add takes the sealed blob stored under name into the open pack, or uploads
// the open pack with it once both reach the target size. A blob of at least
// the target size gets a pack of its own. sealed is not kept. After an error
// the blob is in no pack, and adding it again is safe.
func (w *packWriter) add(ctx context.Context, name string, sealed []byte) error {
	blob, ok := parseName(name)
	if !ok {
		return fmt.Errorf("ark: invalid %s name %q", w.space.blobs, name)
	}
	if len(sealed) < sealOverhead || uint64(len(sealed)) > math.MaxUint32 {
		return fmt.Errorf("ark: %s object %s is no sealed blob a pack holds: %d bytes",
			w.space.blobs, name, len(sealed))
	}
	e := packEntry{name: blob, length: uint32(len(sealed))}
	if err := w.failure(); err != nil {
		return err
	}

	if len(sealed) >= w.target {
		if err := take(ctx, w.uploads); err != nil {
			return err
		}
		return w.store(ctx, []packEntry{e}, sealed)
	}

	if err := take(ctx, w.open); err != nil {
		return err
	}
	if len(w.blobs)+len(sealed) < w.target {
		if w.blobs == nil {
			w.blobs = make([]byte, 0, w.target)
		}
		w.blobs = append(w.blobs, sealed...)
		w.entries = append(w.entries, e)
		<-w.open
		return nil
	}
	blobs, entries, err := w.closeOpen(ctx)
	if err != nil {
		return err
	}
	return w.store(ctx, append(entries, e), blobs, sealed)
}

// flush uploads the open pack, however short. A run calls it once every add
// has returned: only then does a nil say that every blob is stored.
func (w *packWriter) flush(ctx context.Context) error {
	if err := w.failure(); err != nil {
		return err
	}
	if err := take(ctx, w.open); err != nil {
		return err
	}
	if len(w.entries) == 0 {
		<-w.open
		return nil
	}
	blobs, entries, err := w.closeOpen(ctx)
	if err != nil {
		return err
	}
	return w.store(ctx, entries, blobs)
}

// closeOpen takes an upload slot and hands out the open pack, leaving an
// empty one. The caller holds w.open, closeOpen releases it. The slot is
// taken while holding it: while every slot is busy, adds wait here instead of
// filling packs that would queue up in memory. Without a slot the open pack
// stays as it is.
func (w *packWriter) closeOpen(ctx context.Context) ([]byte, []packEntry, error) {
	defer func() { <-w.open }()
	if err := take(ctx, w.uploads); err != nil {
		return nil, nil, err
	}
	blobs, entries := w.blobs, w.entries
	w.blobs, w.entries = nil, nil
	return blobs, entries, nil
}

// store puts the pack of entries, whose blob section is the parts of section
// in order, and reports it. The caller holds an upload slot, which store
// releases once the Put returned.
func (w *packWriter) store(ctx context.Context, entries []packEntry, section ...[]byte) error {
	tail, name := w.space.finish(entries, section...)
	parts := make([]io.Reader, 0, len(section)+1)
	for _, part := range section {
		parts = append(parts, bytes.NewReader(part))
	}
	parts = append(parts, bytes.NewReader(tail))
	err := w.backend.Put(ctx, Handle{Type: w.space.packs, Name: name}, io.MultiReader(parts...))
	<-w.uploads
	if err != nil {
		err = fmt.Errorf("ark: failed to store %s object %s: %w", w.space.packs, name, err)
		w.failed.CompareAndSwap(nil, &err)
		return err
	}
	w.stored(name, entries)
	return nil
}
