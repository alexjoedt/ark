package ark

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
)

// An index object says where the blobs of some packs of one type are stored:
//
//	record count | pack count | pack names, 32 bytes each | records, 44 bytes each
//
// A record is the blob name (32 bytes), the number of its pack in the pack
// table, the offset in that pack and the sealed length. Every number is 4
// bytes little endian. Records are sorted by blob name. The object is sealed
// like any single object and named by the keyed hash of its content.
const (
	// indexMaxRecords closes an index object: one that lists at least this
	// many blobs after a pack was added is stored, so the blobs of one pack
	// are never split across objects.
	indexMaxRecords = 50_000
	// indexHeaderSize is the record count and the pack count.
	indexHeaderSize = 8
	// indexRecordSize is one record, in the object and in memory.
	indexRecordSize = packEntrySize + 8
	// indexBucketBits is how many leading bits of a blob name pick the slice
	// of a table its record is kept in.
	indexBucketBits = 6
)

// indexRecord locates one blob: the entry of its pack's table with the number
// of the pack in the table of the index that holds the record.
type indexRecord struct {
	packEntry
	pack uint32
}

// compareRecords orders records by blob name, then by pack and offset, so the
// copies of one blob are in a fixed order.
func compareRecords(a, b indexRecord) int {
	if c := bytes.Compare(a.name[:], b.name[:]); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(a.pack, b.pack), cmp.Compare(a.offset, b.offset))
}

func compareNames(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) }

// indexedPack is a stored pack with its blob table: what a pack writer
// reports, and what a pack says about itself.
type indexedPack struct {
	name    string
	entries []packEntry
}

// indexTable is the content of index objects: of one, or of several merged.
// Once sorted, packs is sorted by name and holds each pack once, and records
// holds each record once in the order of compareRecords.
type indexTable struct {
	packs [][sha256.Size]byte
	// records keeps the records by the leading bits of their blob name, so
	// its slices one after another are the records in order. A table grows
	// them one by one as it is filled: it never holds all records twice.
	records [1 << indexBucketBits][]indexRecord
	// objects names the index objects a load listed, unreadable those of
	// them it skipped.
	objects, unreadable []string
	// gone marks, by their number in packs, the packs a load found listed and
	// not stored. find passes over their records; they stay in the table for
	// Check to report and for a prune or a rebuild to drop. Nil when every
	// pack is stored.
	gone []bool
}

// newIndexTable returns the sorted table that lists the blobs of packs.
func newIndexTable(packs []indexedPack) (*indexTable, error) {
	t := &indexTable{packs: make([][sha256.Size]byte, len(packs))}
	for i, p := range packs {
		name, ok := parseName(p.name)
		if !ok {
			return nil, fmt.Errorf("ark: invalid pack name %q", p.name)
		}
		t.packs[i] = name
		for _, e := range p.entries {
			t.add(indexRecord{packEntry: e, pack: uint32(i)})
		}
	}
	t.sort()
	return t, nil
}

// mergeIndex returns one sorted table of everything tables list.
func mergeIndex(tables ...*indexTable) *indexTable {
	m := &indexTable{}
	for _, t := range tables {
		base := uint32(len(m.packs))
		m.packs = append(m.packs, t.packs...)
		for _, records := range t.records {
			for _, r := range records {
				r.pack += base
				m.add(r)
			}
		}
	}
	m.sort()
	return m
}

// add appends r, whose pack is a number in t.packs. It leaves t unsorted.
func (t *indexTable) add(r indexRecord) {
	records := &t.records[r.name[0]>>(8-indexBucketBits)]
	*records = append(*records, r)
}

// count returns the number of records.
func (t *indexTable) count() int {
	n := 0
	for _, records := range t.records {
		n += len(records)
	}
	return n
}

// sort puts packs and records in order and drops what is listed twice. The
// order of the records does not depend on the order they were added in or on
// how their packs were numbered: of several copies of a blob, the one in the
// pack with the smallest name comes first.
func (t *indexTable) sort() {
	packs := slices.Clone(t.packs)
	slices.SortFunc(packs, compareNames)
	packs = slices.Compact(packs)
	number := make([]uint32, len(t.packs))
	for i, p := range t.packs {
		n, _ := slices.BinarySearchFunc(packs, p, compareNames)
		number[i] = uint32(n)
	}
	t.packs = packs

	// Ranging over the array itself would copy it, and the copy would keep
	// every slice alive that is replaced here.
	for b, records := range &t.records {
		for i := range records {
			records[i].pack = number[records[i].pack]
		}
		slices.SortFunc(records, compareRecords)
		// Appending left spare capacity: keep a copy without it.
		t.records[b] = slices.Clone(slices.Compact(records))
	}
}

// listed returns the packs of t that keep marks, by their number in t.packs,
// each with every blob t lists in it. That is a second copy of their records.
func (t *indexTable) listed(keep []bool) []indexedPack {
	blobs := make([]int, len(t.packs))
	for _, records := range &t.records {
		for _, rec := range records {
			blobs[rec.pack]++
		}
	}
	entries := make([][]packEntry, len(t.packs))
	for i, n := range blobs {
		if keep[i] {
			entries[i] = make([]packEntry, 0, n)
		}
	}
	for _, records := range &t.records {
		for _, rec := range records {
			if keep[rec.pack] {
				entries[rec.pack] = append(entries[rec.pack], rec.packEntry)
			}
		}
	}
	var packs []indexedPack
	for i, pack := range t.packs {
		if keep[i] {
			packs = append(packs, indexedPack{name: hex.EncodeToString(pack[:]), entries: entries[i]})
		}
	}
	return packs
}

// pack returns the number in t.packs of the pack stored under name. ok is
// false for a pack t does not list and for a name that is no pack name. t is
// sorted.
func (t *indexTable) pack(name string) (i int, ok bool) {
	sum, ok := parseName(name)
	if !ok {
		return 0, false
	}
	return slices.BinarySearchFunc(t.packs, sum, compareNames)
}

// encode returns the content of the index object that holds t, which is
// sorted.
func (t *indexTable) encode() []byte {
	n := t.count()
	out := make([]byte, 0, indexHeaderSize+len(t.packs)*sha256.Size+n*indexRecordSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(n))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(t.packs)))
	for _, p := range t.packs {
		out = append(out, p[:]...)
	}
	for _, records := range t.records {
		for _, r := range records {
			out = append(out, r.name[:]...)
			out = binary.LittleEndian.AppendUint32(out, r.pack)
			out = binary.LittleEndian.AppendUint32(out, r.offset)
			out = binary.LittleEndian.AppendUint32(out, r.length)
		}
	}
	return out
}

// parse reverses encode: it adds what the content of an index object lists
// to t and leaves t unsorted. Content that is not exactly its header, pack
// table and records, or that names a pack outside its table, is an error and
// adds nothing.
func (t *indexTable) parse(data []byte) error {
	if len(data) < indexHeaderSize {
		return fmt.Errorf("%d bytes are shorter than the header", len(data))
	}
	records := uint64(binary.LittleEndian.Uint32(data))
	packs := uint64(binary.LittleEndian.Uint32(data[4:]))
	if want := indexHeaderSize + packs*sha256.Size + records*indexRecordSize; want != uint64(len(data)) {
		return fmt.Errorf("%d records of %d packs take %d bytes, the object has %d",
			records, packs, want, len(data))
	}
	data = data[indexHeaderSize:]
	names, data := data[:packs*sha256.Size], data[packs*sha256.Size:]
	for i := range records {
		if p := binary.LittleEndian.Uint32(data[i*indexRecordSize+sha256.Size:]); uint64(p) >= packs {
			return fmt.Errorf("record %d names pack %d of %d", i, p, packs)
		}
	}

	base := uint32(len(t.packs))
	for ; len(names) > 0; names = names[sha256.Size:] {
		t.packs = append(t.packs, [sha256.Size]byte(names))
	}
	for ; len(data) > 0; data = data[indexRecordSize:] {
		r := indexRecord{pack: base + binary.LittleEndian.Uint32(data[sha256.Size:])}
		copy(r.name[:], data)
		r.offset = binary.LittleEndian.Uint32(data[sha256.Size+4:])
		r.length = binary.LittleEndian.Uint32(data[sha256.Size+8:])
		t.add(r)
	}
	return nil
}

// copies returns every record of the blob named name, in the order of their
// pack names, whether those packs are stored or not. t is sorted.
func (t *indexTable) copies(name [sha256.Size]byte) []indexRecord {
	records := t.records[name[0]>>(8-indexBucketBits)]
	i, _ := slices.BinarySearchFunc(records, name, func(r indexRecord, name [sha256.Size]byte) int {
		return compareNames(r.name, name)
	})
	n := i
	for n < len(records) && records[n].name == name {
		n++
	}
	return records[i:n]
}

// find returns the pack of the first record of the blob named name whose
// pack is stored, and the blob's place in it. t is sorted.
func (t *indexTable) find(name [sha256.Size]byte) (pack [sha256.Size]byte, e packEntry, ok bool) {
	for _, r := range t.copies(name) {
		if t.gone == nil || !t.gone[r.pack] {
			return t.packs[r.pack], r.packEntry, true
		}
	}
	return pack, e, false
}

// packIndex is the index of one blob type. Nothing is read before the first
// lookup: that one lists the index namespace once, loads every object, merges
// them into the table all later lookups search and then lists the pack
// namespace once, to pass over what the table lists in a pack that is not
// stored. What it stores after
// that load it keeps beside the table, so a lookup finds a blob whether its
// index object was written before the load or after it. It is safe for
// concurrent use.
type packIndex struct {
	space   *packSpace
	backend Backend
	logger  *slog.Logger

	// loading holds a token while a caller loads the table or writes index
	// objects: a mutex whose wait ends with the caller's context.
	loading chan struct{}
	table   atomic.Pointer[indexTable]
	// pending is the table of a load whose pack listing failed, held under
	// the loading token: the next load lists the packs again and reads
	// nothing else.
	pending *indexTable
	// written lists the packs of the index objects stored after the table
	// was loaded, which is never listed again.
	written atomic.Pointer[indexTable]
	// lost, when set, runs when a load finds a pack the table lists that is
	// not stored or an index object that does not open, before any lookup
	// passes over what they held. Its error fails the load.
	lost func(context.Context) error
}

// newPackIndex returns the index of the blobs s packs. It issues no request.
func newPackIndex(b Backend, s *packSpace, logger *slog.Logger) *packIndex {
	return &packIndex{space: s, backend: b, logger: logger, loading: make(chan struct{}, 1)}
}

// lookup returns the pack that holds the blob named name and the blob's place
// in it. ok is false when no index object lists the blob in a pack that was
// stored when the index was loaded; a name that is none is an error. A blob
// in several packs resolves to the same one on every lookup: of those stored,
// the one with the smallest name.
func (x *packIndex) lookup(ctx context.Context, name string) (pack string, e packEntry, ok bool, err error) {
	blob, ok := parseName(name)
	if !ok {
		return "", packEntry{}, false, fmt.Errorf("ark: invalid %s name %q", x.space.blobs, name)
	}
	in, e, ok, err := x.find(ctx, blob)
	if err != nil || !ok {
		return "", packEntry{}, false, err
	}
	return hex.EncodeToString(in[:]), e, true, nil
}

// find is lookup by the bytes a blob name and a pack name spell.
func (x *packIndex) find(
	ctx context.Context, blob [sha256.Size]byte,
) (pack [sha256.Size]byte, e packEntry, ok bool, err error) {
	t, err := x.load(ctx)
	if err != nil {
		return pack, packEntry{}, false, err
	}
	pack, e, ok = t.find(blob)
	if w := x.written.Load(); w != nil {
		if p, we, found := w.find(blob); found && (!ok || compareNames(p, pack) < 0) {
			pack, e, ok = p, we, true
		}
	}
	return pack, e, ok, nil
}

// lists reports whether an index object lists the blob named name, in a pack
// that is stored or in one that is not. A prune asks this: what a lost pack
// held stays listed for Check to report.
func (x *packIndex) lists(ctx context.Context, name string) (bool, error) {
	blob, ok := parseName(name)
	if !ok {
		return false, fmt.Errorf("ark: invalid %s name %q", x.space.blobs, name)
	}
	t, err := x.load(ctx)
	if err != nil {
		return false, err
	}
	if len(t.copies(blob)) > 0 {
		return true, nil
	}
	w := x.written.Load()
	return w != nil && len(w.copies(blob)) > 0, nil
}

// load returns the merged table, reading it on the first call. An index
// object that does not open is read a second time and then skipped with a
// warning: the blobs only it lists count as missing. Any other error fails the
// call and the next one loads again; after one from listing the packs it only
// lists them again.
func (x *packIndex) load(ctx context.Context) (*indexTable, error) {
	if t := x.table.Load(); t != nil {
		return t, nil
	}
	if err := take(ctx, x.loading); err != nil {
		return nil, err
	}
	defer func() { <-x.loading }()
	if t := x.table.Load(); t != nil {
		return t, nil
	}
	t := x.pending
	if t == nil {
		var err error
		if t, err = x.readAll(ctx); err != nil {
			return nil, err
		}
	}
	gone, err := x.markGone(ctx, t)
	if err == nil && x.lost != nil && (gone != nil || len(t.unreadable) > 0) {
		err = x.lost(ctx)
	}
	if err != nil {
		x.pending = t
		return nil, err
	}
	t.gone = gone
	x.pending = nil
	x.table.Store(t)
	return t, nil
}

// sawLoss reports whether the table is loaded and its load found a listed
// pack that is not stored or an index object that does not open. It issues no
// request.
func (x *packIndex) sawLoss() bool {
	t := x.table.Load()
	return t != nil && (t.gone != nil || len(t.unreadable) > 0)
}

// readAll lists the index namespace and reads every object into one sorted
// table.
func (x *packIndex) readAll(ctx context.Context) (*indexTable, error) {
	names, err := x.names(ctx, x.space.index)
	if err != nil {
		return nil, err
	}
	// ponytail: one Get at a time, and sort orders all records again although
	// every object is sorted. Load with the repository's concurrency once a
	// backend with slow round trips shows the wait, merge the sorted objects
	// once the load nears its budget (BenchmarkIndexLoad).
	t := &indexTable{objects: names}
	for _, name := range names {
		err := x.read(ctx, name, t)
		if errors.Is(err, ErrCorruptIndex) {
			// A read that came back short or garbled looks like a damaged
			// object, and what is skipped here stays missing for as long as x
			// is used: ask once more.
			err = x.read(ctx, name, t)
		}
		switch {
		case errors.Is(err, ErrCorruptIndex):
			x.logger.WarnContext(ctx, "skipping an index object that does not open, its blobs count as missing",
				"namespace", x.space.index.String(), "object", name, "error", err)
			t.unreadable = append(t.unreadable, name)
		case err != nil:
			return nil, err
		}
	}
	t.sort()
	return t, nil
}

// markGone lists the pack namespace once and returns which packs of t the
// listing lacks, by pack number, nil when it lacks none. It runs after the
// index objects were read, never before: an index object is stored once its
// packs are durable, so a pack missing from a listing taken later is gone,
// while one taken earlier would miss the packs of an index object another
// writer stored in between. A listing that fails
// marks nothing and fails the load: a record is passed over only for a pack
// that a complete listing lacks.
func (x *packIndex) markGone(ctx context.Context, t *indexTable) ([]bool, error) {
	if len(t.packs) == 0 {
		return nil, nil
	}
	gone := slices.Repeat([]bool{true}, len(t.packs))
	n := len(gone)
	err := x.backend.List(ctx, x.space.packs, func(info BlobInfo) error {
		if i, listed := t.pack(info.Name); listed && gone[i] {
			gone[i] = false
			n--
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to list %s objects: %w", x.space.packs, err)
	}
	if n == 0 {
		return nil, nil
	}
	x.logger.WarnContext(ctx, "the index lists packs that are not stored, what only they hold counts as missing",
		"namespace", x.space.packs.String(), "packs", n)
	return gone, nil
}

// names lists the objects of namespace typ, sorted.
func (x *packIndex) names(ctx context.Context, typ HandleType) ([]string, error) {
	var names []string
	err := x.backend.List(ctx, typ, func(info BlobInfo) error {
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to list %s objects: %w", typ, err)
	}
	slices.Sort(names)
	return names, nil
}

// read adds what the index object named name lists to t, which it leaves
// unsorted. An object that was changed, cut short or moved from another name
// or namespace fails with ErrCorruptIndex and adds nothing.
func (x *packIndex) read(ctx context.Context, name string, t *indexTable) error {
	h := Handle{Type: x.space.index, Name: name}
	data, err := readSealed(ctx, x.backend, x.space.codec, x.space.dataKey, h)
	if errors.Is(err, errSealBroken) {
		return fmt.Errorf("%w: %w", ErrCorruptIndex, err)
	}
	if err != nil {
		return err
	}
	if err := t.parse(data); err != nil {
		return fmt.Errorf("%w: %s object %s: %w", ErrCorruptIndex, h.Type, name, err)
	}
	return nil
}

// write stores index objects that list packs and returns their names. Packs
// are taken in order and an object is closed once it lists indexMaxRecords
// blobs, so the last one may be short. The caller made every pack durable
// first: a stored index object is what makes its blobs count as present.
func (x *packIndex) write(ctx context.Context, packs []indexedPack) ([]string, error) {
	// A load lists once. Holding it off makes every object stored here either
	// one a later load lists or one whose packs go into written.
	if err := take(ctx, x.loading); err != nil {
		return nil, err
	}
	defer func() { <-x.loading }()

	var names []string
	first, records := 0, 0
	for i, p := range packs {
		records += len(p.entries)
		if records < indexMaxRecords && i < len(packs)-1 {
			continue
		}
		t, err := newIndexTable(packs[first : i+1])
		if err != nil {
			return names, err
		}
		data := t.encode()
		h := Handle{Type: x.space.index, Name: keyedName(x.space.idKey, data)}
		obj := x.space.codec.seal(x.space.dataKey, objectAAD(h), data)
		if err := x.backend.Put(ctx, h, bytes.NewReader(obj)); err != nil {
			return names, fmt.Errorf("ark: failed to store %s object %s: %w", h.Type, h.Name, err)
		}
		names = append(names, h.Name)
		first, records = i+1, 0
		if x.table.Load() == nil {
			continue
		}
		// ponytail: every object sorts all that was written since the load
		// again. Merge the two sorted tables if a long run shows it.
		if w := x.written.Load(); w != nil {
			t = mergeIndex(w, t)
		}
		x.written.Store(t)
	}
	return names, nil
}

// scanPacks returns every stored pack with the blob table the pack itself
// carries, which is what an index is rebuilt from, and the names of the
// objects it skipped with a warning: a pack that does not open describes no
// blob. Every pack is downloaded whole, and one that does not open a second
// time before it is skipped.
func (x *packIndex) scanPacks(ctx context.Context) (packs []indexedPack, unreadable []string, err error) {
	names, err := x.names(ctx, x.space.packs)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		entries, err := x.scanPack(ctx, name)
		if errors.Is(err, x.space.corruptErr) {
			// A read that came back short or garbled looks like a damaged
			// pack, and a rebuild that skips a pack lists none of its blobs:
			// ask once more.
			entries, err = x.scanPack(ctx, name)
		}
		if errors.Is(err, x.space.corruptErr) {
			x.logger.WarnContext(ctx, "skipping a pack that does not open, a rebuilt index lists none of its blobs",
				"namespace", x.space.packs.String(), "object", name, "error", err)
			unreadable = append(unreadable, name)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		packs = append(packs, indexedPack{name: name, entries: entries})
	}
	return packs, unreadable, nil
}

// scanPack returns the blob table of the pack named name.
func (x *packIndex) scanPack(ctx context.Context, name string) ([]packEntry, error) {
	h := Handle{Type: x.space.packs, Name: name}
	sum, ok := parseName(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s object %q has no pack name", x.space.corruptErr, h.Type, name)
	}
	pack, err := readWhole(ctx, x.backend, h)
	if err != nil {
		return nil, err
	}
	// The table of a pack stored under another name still opens: it is bound
	// to the blobs, not to the name.
	if sha256.Sum256(pack) != sum {
		return nil, fmt.Errorf("%w: %s object %s is not named by its hash", x.space.corruptErr, h.Type, name)
	}
	return x.space.parse(pack)
}
