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
	"io"
	"log/slog"
	"slices"
	"sort"
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
// packs is sorted by name and holds each pack once; records is sorted by
// compareRecords and holds each record once.
type indexTable struct {
	packs   [][sha256.Size]byte
	records []indexRecord
	// unreadable names the index objects a load skipped.
	unreadable []string
}

// newIndexTable returns the table that lists the blobs of packs.
func newIndexTable(packs []indexedPack) (*indexTable, error) {
	t := &indexTable{packs: make([][sha256.Size]byte, len(packs))}
	for i, p := range packs {
		name, ok := parseName(p.name)
		if !ok {
			return nil, fmt.Errorf("ark: invalid pack name %q", p.name)
		}
		t.packs[i] = name
		for _, e := range p.entries {
			t.records = append(t.records, indexRecord{packEntry: e, pack: uint32(i)})
		}
	}
	return mergeIndex(t), nil
}

// mergeIndex returns one table of everything tables list. The order of its
// records does not depend on the order of tables or on how they number their
// packs: of several copies of a blob, the one in the pack with the smallest
// name comes first.
func mergeIndex(tables ...*indexTable) *indexTable {
	var packs, records int
	for _, t := range tables {
		packs += len(t.packs)
		records += len(t.records)
	}
	m := &indexTable{
		packs:   make([][sha256.Size]byte, 0, packs),
		records: make([]indexRecord, 0, records),
	}
	for _, t := range tables {
		m.packs = append(m.packs, t.packs...)
	}
	slices.SortFunc(m.packs, compareNames)
	m.packs = slices.Compact(m.packs)

	for _, t := range tables {
		number := make([]uint32, len(t.packs))
		for i, p := range t.packs {
			n, _ := slices.BinarySearchFunc(m.packs, p, compareNames)
			number[i] = uint32(n)
		}
		for _, r := range t.records {
			r.pack = number[r.pack]
			m.records = append(m.records, r)
		}
	}
	slices.SortFunc(m.records, compareRecords)
	m.records = slices.Compact(m.records)
	return m
}

// encode returns the content of the index object that holds t.
func (t *indexTable) encode() []byte {
	out := make([]byte, 0, indexHeaderSize+len(t.packs)*sha256.Size+len(t.records)*indexRecordSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(t.records)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(t.packs)))
	for _, p := range t.packs {
		out = append(out, p[:]...)
	}
	for _, r := range t.records {
		out = append(out, r.name[:]...)
		out = binary.LittleEndian.AppendUint32(out, r.pack)
		out = binary.LittleEndian.AppendUint32(out, r.offset)
		out = binary.LittleEndian.AppendUint32(out, r.length)
	}
	return out
}

// parseIndex reverses encode. Content that is not exactly its header, pack
// table and records, or that names a pack outside its table, is an error.
func parseIndex(data []byte) (*indexTable, error) {
	if len(data) < indexHeaderSize {
		return nil, fmt.Errorf("%d bytes are shorter than the header", len(data))
	}
	records := uint64(binary.LittleEndian.Uint32(data))
	packs := uint64(binary.LittleEndian.Uint32(data[4:]))
	if want := indexHeaderSize + packs*sha256.Size + records*indexRecordSize; want != uint64(len(data)) {
		return nil, fmt.Errorf("%d records of %d packs take %d bytes, the object has %d",
			records, packs, want, len(data))
	}
	t := &indexTable{
		packs:   make([][sha256.Size]byte, packs),
		records: make([]indexRecord, records),
	}
	data = data[indexHeaderSize:]
	for i := range t.packs {
		data = data[copy(t.packs[i][:], data):]
	}
	for i := range t.records {
		r := &t.records[i]
		rest := data[copy(r.name[:], data):]
		r.pack = binary.LittleEndian.Uint32(rest)
		r.offset = binary.LittleEndian.Uint32(rest[4:])
		r.length = binary.LittleEndian.Uint32(rest[8:])
		if uint64(r.pack) >= packs {
			return nil, fmt.Errorf("record %d names pack %d of %d", i, r.pack, packs)
		}
		data = data[indexRecordSize:]
	}
	return t, nil
}

// find returns the first record of the blob named name.
func (t *indexTable) find(name [sha256.Size]byte) (indexRecord, bool) {
	i := sort.Search(len(t.records), func(i int) bool {
		return bytes.Compare(t.records[i].name[:], name[:]) >= 0
	})
	if i == len(t.records) || t.records[i].name != name {
		return indexRecord{}, false
	}
	return t.records[i], true
}

// packIndex is the index of one blob type. Nothing is read before the first
// lookup: that one lists the index namespace once, loads every object and
// merges them into the table all later lookups search. It is safe for
// concurrent use.
type packIndex struct {
	space   *packSpace
	backend Backend
	logger  *slog.Logger

	// loading holds a token while a caller loads the table: a mutex whose
	// wait ends with the caller's context.
	loading chan struct{}
	table   atomic.Pointer[indexTable]
}

// newPackIndex returns the index of the blobs s packs. It issues no request.
func newPackIndex(b Backend, s *packSpace, logger *slog.Logger) *packIndex {
	return &packIndex{space: s, backend: b, logger: logger, loading: make(chan struct{}, 1)}
}

// lookup returns the pack that holds the blob named name and the blob's place
// in it. ok is false when no index object lists the blob. A blob in several
// packs resolves to the same one on every lookup, the first of the merged
// table.
func (x *packIndex) lookup(ctx context.Context, name string) (pack string, e packEntry, ok bool, err error) {
	t, err := x.load(ctx)
	if err != nil {
		return "", packEntry{}, false, err
	}
	blob, ok := parseName(name)
	if !ok {
		return "", packEntry{}, false, nil
	}
	r, ok := t.find(blob)
	if !ok {
		return "", packEntry{}, false, nil
	}
	return hex.EncodeToString(t.packs[r.pack][:]), r.packEntry, true, nil
}

// load returns the merged table, reading it on the first call. An index
// object that does not open is skipped with a warning: the blobs only it lists
// count as missing. Any other error fails the call and the next one loads
// again.
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

	names, err := x.names(ctx, x.space.index)
	if err != nil {
		return nil, err
	}
	// ponytail: one Get at a time, and mergeIndex sorts all records again
	// although every object is sorted. Load with the repository's concurrency
	// once a backend with slow round trips shows the wait, merge the sorted
	// objects once the load nears its budget (BenchmarkIndexLoad).
	tables := make([]*indexTable, 0, len(names))
	var unreadable []string
	for _, name := range names {
		t, err := x.read(ctx, name)
		switch {
		case errors.Is(err, ErrCorruptIndex):
			x.logger.WarnContext(ctx, "skipping an index object that does not open, its blobs count as missing",
				"namespace", x.space.index.String(), "object", name, "error", err)
			unreadable = append(unreadable, name)
		case err != nil:
			return nil, err
		default:
			tables = append(tables, t)
		}
	}
	t := mergeIndex(tables...)
	t.unreadable = unreadable
	x.table.Store(t)
	return t, nil
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

// read returns the table of the index object named name. An object that was
// changed, cut short or moved from another name or namespace fails with
// ErrCorruptIndex.
func (x *packIndex) read(ctx context.Context, name string) (*indexTable, error) {
	h := Handle{Type: x.space.index, Name: name}
	data, err := readSealed(ctx, x.backend, x.space.codec, x.space.dataKey, h)
	if errors.Is(err, errSealBroken) {
		return nil, fmt.Errorf("%w: %w", ErrCorruptIndex, err)
	}
	if err != nil {
		return nil, err
	}
	t, err := parseIndex(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s object %s: %w", ErrCorruptIndex, h.Type, name, err)
	}
	return t, nil
}

// write stores index objects that list packs and returns their names. Packs
// are taken in order and an object is closed once it lists indexMaxRecords
// blobs, so the last one may be short. The caller made every pack durable
// first: a stored index object is what makes its blobs count as present.
func (x *packIndex) write(ctx context.Context, packs []indexedPack) ([]string, error) {
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
	}
	return names, nil
}

// scanPacks returns every stored pack with the blob table the pack itself
// carries, which is what an index is rebuilt from, and the names of the
// objects it skipped with a warning: a pack that does not open describes no
// blob. Every pack is downloaded whole.
func (x *packIndex) scanPacks(ctx context.Context) (packs []indexedPack, unreadable []string, err error) {
	names, err := x.names(ctx, x.space.packs)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		entries, err := x.scanPack(ctx, name)
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
	if _, ok := parseName(name); !ok {
		return nil, fmt.Errorf("%w: %s object %q has no pack name", x.space.corruptErr, h.Type, name)
	}
	rc, err := x.backend.Get(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, name, err)
	}
	defer rc.Close()
	pack, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, name, err)
	}
	return x.space.parse(pack)
}
