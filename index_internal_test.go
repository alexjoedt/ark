package ark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var quietLogger = slog.New(slog.DiscardHandler)

// indexBackend counts what an index asks of the backend, per namespace where
// the criteria name one.
type indexBackend struct {
	Backend

	mu          sync.Mutex
	lists, gets map[HandleType]int
	other       int
	// onList runs inside every List before it is answered, with the namespace
	// listed.
	onList func(HandleType)
	// getErr fails every Get while it is set.
	getErr error
	// cutGets is the number of coming Gets that are answered with the first
	// half of the object and no error.
	cutGets int
}

func newIndexBackend(b Backend) *indexBackend {
	return &indexBackend{Backend: b, lists: make(map[HandleType]int), gets: make(map[HandleType]int)}
}

func (b *indexBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.lists)
	clear(b.gets)
	b.other = 0
}

// calls returns the Lists and Gets of namespace typ and every request so far.
func (b *indexBackend) calls(typ HandleType) (lists, gets, total int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total = b.other
	for _, n := range b.lists {
		total += n
	}
	for _, n := range b.gets {
		total += n
	}
	return b.lists[typ], b.gets[typ], total
}

func (b *indexBackend) List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error {
	b.mu.Lock()
	b.lists[t]++
	onList := b.onList
	b.mu.Unlock()
	if onList != nil {
		onList(t)
	}
	return b.Backend.List(ctx, t, fn)
}

func (b *indexBackend) Get(ctx context.Context, h Handle) (io.ReadCloser, error) {
	b.mu.Lock()
	b.gets[h.Type]++
	err := b.getErr
	cut := b.cutGets > 0
	if cut {
		b.cutGets--
	}
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rc, err := b.Backend.Get(ctx, h)
	if err != nil || !cut {
		return rc, err
	}
	defer rc.Close()
	obj, err := io.ReadAll(rc)
	return io.NopCloser(bytes.NewReader(obj[:len(obj)/2])), err
}

func (b *indexBackend) count() {
	b.mu.Lock()
	b.other++
	b.mu.Unlock()
}

func (b *indexBackend) GetRange(ctx context.Context, h Handle, offset, length int64) ([]byte, error) {
	b.count()
	return b.Backend.GetRange(ctx, h, offset, length)
}

func (b *indexBackend) Exists(ctx context.Context, h Handle) (bool, error) {
	b.count()
	return b.Backend.Exists(ctx, h)
}

func (b *indexBackend) Stat(ctx context.Context, h Handle) (BlobInfo, error) {
	b.count()
	return b.Backend.Stat(ctx, h)
}

// storeTestPacks stores count blobs of s through a pack writer that closes a
// pack every few blobs, and returns the packs in the order they were stored.
func storeTestPacks(t *testing.T, b Backend, s *packSpace, seed byte, count int) ([]indexedPack, []testBlob) {
	t.Helper()
	ctx := context.Background()
	sizes := make([]int, count)
	for i := range sizes {
		sizes[i] = 300 + 37*i
	}
	blobs := testBlobs(s, seed, sizes...)

	var log packLog
	w := newPackWriter(b, s, newPackUploads(), log.stored)
	w.target = 2000
	for _, blob := range blobs {
		if err := w.add(ctx, blob.name, blob.sealed); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := w.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	packs := make([]indexedPack, len(log.order))
	for i, name := range log.order {
		packs[i] = indexedPack{name: name, entries: log.packs[name]}
	}
	if len(packs) < 3 {
		t.Fatalf("%d blobs went into %d packs, the test needs several", count, len(packs))
	}
	return packs, blobs
}

func writeTestIndex(t *testing.T, x *packIndex, packs []indexedPack) []string {
	t.Helper()
	names, err := x.write(context.Background(), packs)
	if err != nil {
		t.Fatalf("write index: %v", err)
	}
	return names
}

// syntheticPack returns a pack that was never stored: a name and count
// entries of random blob names. An index that loads it passes over its blobs
// until storePlaceholders has stored an object under its name.
func syntheticPack(rng *rand.ChaCha8, count int) indexedPack {
	var name [sha256.Size]byte
	_, _ = rng.Read(name[:])
	p := indexedPack{name: hex.EncodeToString(name[:]), entries: make([]packEntry, count)}
	for i := range p.entries {
		_, _ = rng.Read(p.entries[i].name[:])
		p.entries[i].length = 100 + uint32(i%50)
	}
	place(p.entries)
	return p
}

// storePlaceholders stores an empty object under the name of every pack, which
// is all a load asks of a pack: that the namespace lists it.
func storePlaceholders(t *testing.T, b Backend, s *packSpace, packs ...indexedPack) {
	t.Helper()
	for _, p := range packs {
		putObject(t, b, Handle{Type: s.packs, Name: p.name}, nil)
	}
}

// allRecords returns the records of table in the order it keeps them.
func allRecords(table *indexTable) []indexRecord { return slices.Concat(table.records[:]...) }

func mustLookup(t *testing.T, x *packIndex, name string) (string, packEntry, bool) {
	t.Helper()
	pack, e, ok, err := x.lookup(context.Background(), name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return pack, e, ok
}

func TestIndexAnswersLookupsForEveryBlob(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.index.String(), func(t *testing.T) {
			b := newStubBackend()
			writer := newPackIndex(b, s, quietLogger)
			var blobs []testBlob
			var objects []string
			for seed := range byte(3) {
				packs, stored := storeTestPacks(t, b, s, seed, 20)
				objects = append(objects, writeTestIndex(t, writer, packs)...)
				blobs = append(blobs, stored...)
			}
			if len(objects) != 3 || len(slices.Compact(slices.Sorted(slices.Values(objects)))) != 3 {
				t.Fatalf("index objects %v, want three distinct ones", objects)
			}

			x := newPackIndex(b, s, quietLogger)
			for _, blob := range blobs {
				pack, e, ok := mustLookup(t, x, blob.name)
				if !ok {
					t.Fatalf("blob %s is in no index object", blob.name)
				}
				data, err := s.readBlob(ctx, b, pack, e)
				if err != nil {
					t.Fatalf("readBlob %s: %v", blob.name, err)
				}
				if !bytes.Equal(data, blob.data) {
					t.Errorf("blob %s read through the index has other content", blob.name)
				}
			}
			for _, name := range []string{strings.Repeat("0", 64), strings.Repeat("f", 64)} {
				if pack, _, ok := mustLookup(t, x, name); ok {
					t.Errorf("lookup %q found pack %s, want no blob", name, pack)
				}
			}
		})
	}
}

func TestIndexLookupRejectsWhatIsNoName(t *testing.T) {
	s := testPackSpaces(t)[0]
	b := newIndexBackend(newStubBackend())
	x := newPackIndex(b, s, quietLogger)
	packs, blobs := storeTestPacks(t, b, s, 1, 12)
	writeTestIndex(t, x, packs)
	b.reset()

	for _, name := range []string{strings.ToUpper(blobs[0].name), blobs[0].name[:63], blobs[0].name + "0", "short", ""} {
		_, _, ok, err := x.lookup(context.Background(), name)
		if ok || err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("lookup %q = %v, %v, want an invalid name error", name, ok, err)
		}
	}
	if _, _, total := b.calls(s.index); total != 0 {
		t.Errorf("%d backend requests for names that are none, want 0", total)
	}
}

// TestIndexFindsWhatItWroteAfterLoading pins that the order of the first
// lookup and a write does not matter: an index that has loaded its table
// answers for the packs it lists afterwards, without listing again, as an
// index that loads later does.
func TestIndexFindsWhatItWroteAfterLoading(t *testing.T) {
	s := testPackSpaces(t)[0]
	rng := rand.NewChaCha8([32]byte{3})
	one, two, three := syntheticPack(rng, 40), syntheticPack(rng, 40), syntheticPack(rng, 40)
	// The same blob sits in the first two packs, at different places.
	shared := one.entries[7].name
	two.entries[31].name = shared
	first, firstEntry := one, one.entries[7]
	if two.name < one.name {
		first, firstEntry = two, two.entries[31]
	}

	for name, c := range map[string]struct{ before, after [][]indexedPack }{
		"all after":               {nil, [][]indexedPack{{one, two}, {three}}},
		"all after, one by one":   {nil, [][]indexedPack{{three}, {two}, {one}}},
		"a copy before":           {[][]indexedPack{{one}}, [][]indexedPack{{two}, {three}}},
		"the other copy before":   {[][]indexedPack{{two, three}}, [][]indexedPack{{one}}},
		"a pack listed both ways": {[][]indexedPack{{one, two}}, [][]indexedPack{{two, three}, {one}}},
	} {
		t.Run(name, func(t *testing.T) {
			b := newIndexBackend(newStubBackend())
			storePlaceholders(t, b, s, one, two, three)
			x := newPackIndex(b, s, quietLogger)
			for _, packs := range c.before {
				writeTestIndex(t, x, packs)
			}
			if x.written.Load() != nil {
				t.Error("an index that has loaded nothing keeps what it wrote, which its load will list")
			}
			if _, _, ok := mustLookup(t, x, strings.Repeat("0", 64)); ok {
				t.Fatal("found a blob no pack holds")
			}
			for _, packs := range c.after {
				writeTestIndex(t, x, packs)
			}
			b.reset()

			for _, index := range []*packIndex{x, newPackIndex(b, s, quietLogger)} {
				for _, p := range []indexedPack{one, two, three} {
					for _, e := range p.entries {
						want, wantEntry := p.name, e
						if e.name == shared {
							want, wantEntry = first.name, firstEntry
						}
						pack, got, ok := mustLookup(t, index, hex.EncodeToString(e.name[:]))
						if !ok || pack != want || got != wantEntry {
							t.Fatalf("lookup = %s %+v %v, want %s %+v", pack, got, ok, want, wantEntry)
						}
					}
				}
				if index == x {
					if _, _, total := b.calls(s.index); total != 0 {
						t.Errorf("%d backend requests to find what the index wrote itself, want 0", total)
					}
				}
			}
		})
	}
}

// TestIndexReadsAnObjectAgainBeforeSkippingIt pins that one read that came
// back short does not make the blobs of an index object missing.
func TestIndexReadsAnObjectAgainBeforeSkippingIt(t *testing.T) {
	s := testPackSpaces(t)[0]
	b := newIndexBackend(newStubBackend())
	writer := newPackIndex(b, s, quietLogger)
	var blobs []testBlob
	for seed := range byte(3) {
		packs, stored := storeTestPacks(t, b, s, seed, 12)
		writeTestIndex(t, writer, packs)
		blobs = append(blobs, stored...)
	}
	b.reset()

	var logged bytes.Buffer
	x := newPackIndex(b, s, slog.New(slog.NewTextHandler(&logged, nil)))
	b.cutGets = 1
	for _, blob := range blobs {
		if _, _, ok := mustLookup(t, x, blob.name); !ok {
			t.Fatalf("blob %s is missing after one short read", blob.name)
		}
	}
	if lists, gets, _ := b.calls(s.index); lists != 1 || gets != 4 {
		t.Errorf("%d Lists and %d Gets, want 1 and 4: each of 3 objects once, the short one again", lists, gets)
	}
	if got := x.table.Load().unreadable; len(got) != 0 || logged.Len() != 0 {
		t.Errorf("unreadable index objects %v, logged %q, want none", got, logged.String())
	}
}

// TestRebuildScanReadsAPackAgainBeforeSkippingIt pins that one read that came
// back short does not leave a stored pack out of a rebuilt index.
func TestRebuildScanReadsAPackAgainBeforeSkippingIt(t *testing.T) {
	s := testPackSpaces(t)[0]
	b := newIndexBackend(newStubBackend())
	stored, _ := storeTestPacks(t, b, s, 1, 20)
	b.reset()

	var logged bytes.Buffer
	b.cutGets = 1
	scanner := newPackIndex(b, s, slog.New(slog.NewTextHandler(&logged, nil)))
	packs, unreadable, err := scanner.scanPacks(context.Background())
	if err != nil {
		t.Fatalf("scanPacks: %v", err)
	}
	if len(packs) != len(stored) || len(unreadable) != 0 || logged.Len() != 0 {
		t.Errorf("scan found %d of %d packs, unreadable %v, logged %q; want every pack and no warning",
			len(packs), len(stored), unreadable, logged.String())
	}
	if _, gets, _ := b.calls(s.packs); gets != len(stored)+1 {
		t.Errorf("%d Gets, want %d: each pack once, the short one again", gets, len(stored)+1)
	}
}

func TestIndexLoadsLazilyAndOncePerType(t *testing.T) {
	ctx := context.Background()
	spaces := testPackSpaces(t)
	data, trees := spaces[0], spaces[1]
	b := newIndexBackend(newStubBackend())

	stored := make(map[*packSpace][]testBlob)
	objects := make(map[*packSpace]int)
	for i, s := range spaces {
		writer := newPackIndex(b, s, quietLogger)
		for seed := range byte(2 + i) {
			packs, blobs := storeTestPacks(t, b, s, seed, 12)
			objects[s] += len(writeTestIndex(t, writer, packs))
			stored[s] = append(stored[s], blobs...)
		}
	}
	if objects[data] == objects[trees] {
		t.Fatalf("both namespaces hold %d index objects, the counts below could not tell them apart", objects[data])
	}
	b.reset()

	dataIndex, treeIndex := newPackIndex(b, data, quietLogger), newPackIndex(b, trees, quietLogger)
	if _, _, total := b.calls(TreeIndexFile); total != 0 {
		t.Fatalf("opening the indexes issued %d backend requests, want none", total)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := treeIndex.lookup(cancelled, stored[trees][0].name); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup under a cancelled context: %v, want context.Canceled", err)
	}
	if _, _, total := b.calls(TreeIndexFile); total != 0 {
		t.Fatalf("a lookup under a cancelled context issued %d backend requests", total)
	}

	check := func(when string, typ HandleType, wantLists, wantGets, wantTotal int) {
		t.Helper()
		lists, gets, total := b.calls(typ)
		if lists != wantLists || gets != wantGets || total != wantTotal {
			t.Fatalf("%s: %s lists %d gets %d, all requests %d; want %d, %d, %d",
				when, typ, lists, gets, total, wantLists, wantGets, wantTotal)
		}
	}
	lookupAll := func(x *packIndex, blobs []testBlob) {
		t.Helper()
		for _, blob := range blobs {
			if _, _, ok := mustLookup(t, x, blob.name); !ok {
				t.Fatalf("blob %s is in no index object", blob.name)
			}
		}
	}

	if _, _, ok := mustLookup(t, treeIndex, stored[trees][0].name); !ok {
		t.Fatal("the first tree lookup found nothing")
	}
	// One List of the index objects, one Get of each, one List of the packs.
	treeLoad := 2 + objects[trees]
	check("first tree lookup", TreeIndexFile, 1, objects[trees], treeLoad)
	check("first tree lookup", TreePackFile, 1, 0, treeLoad)
	check("first tree lookup", DataIndexFile, 0, 0, treeLoad)
	check("first tree lookup", DataPackFile, 0, 0, treeLoad)

	lookupAll(treeIndex, stored[trees])
	mustLookup(t, treeIndex, strings.Repeat("0", 64))
	check("later tree lookups", TreeIndexFile, 1, objects[trees], treeLoad)
	check("later tree lookups", TreePackFile, 1, 0, treeLoad)

	lookupAll(dataIndex, stored[data])
	check("data lookups", DataIndexFile, 1, objects[data], treeLoad+2+objects[data])
	check("data lookups", DataPackFile, 1, 0, treeLoad+2+objects[data])
	check("data lookups", TreeIndexFile, 1, objects[trees], treeLoad+2+objects[data])

	t.Run("concurrent first lookups", func(t *testing.T) {
		b.reset()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		b.mu.Lock()
		b.onList = func(HandleType) {
			once.Do(func() { close(entered) })
			<-release
		}
		b.mu.Unlock()

		x := newPackIndex(b, data, quietLogger)
		var wg sync.WaitGroup
		errs := make(chan error, len(stored[data]))
		for _, blob := range stored[data] {
			wg.Go(func() {
				_, _, ok, err := x.lookup(ctx, blob.name)
				if err == nil && !ok {
					err = errors.New("blob " + blob.name + " is in no index object")
				}
				errs <- err
			})
		}
		<-entered
		// Give the other lookups time to queue up behind the one that loads:
		// the test passes without it, it only makes the waiters real.
		time.Sleep(20 * time.Millisecond)
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
		check("concurrent first lookups", DataIndexFile, 1, objects[data], 2+objects[data])
		check("concurrent first lookups", DataPackFile, 1, 0, 2+objects[data])
	})
}

func TestIndexObjectClosesAtTheRecordCap(t *testing.T) {
	ctx := context.Background()
	s := testPackSpaces(t)[0]
	b := newStubBackend()
	x := newPackIndex(b, s, quietLogger)

	rng := rand.NewChaCha8([32]byte{1})
	sizes := []int{30_000, 30_000, 10, indexMaxRecords - 10, 5, 60_000, 7}
	want := [][]int{{0, 1}, {2, 3}, {4, 5}, {6}}
	packs := make([]indexedPack, len(sizes))
	for i, size := range sizes {
		packs[i] = syntheticPack(rng, size)
	}

	storePlaceholders(t, b, s, packs...)

	names := writeTestIndex(t, x, packs)
	if len(names) != len(want) {
		t.Fatalf("%d index objects, want %d", len(names), len(want))
	}
	for i, name := range names {
		table := &indexTable{}
		if err := x.read(ctx, name, table); err != nil {
			t.Fatalf("read index object %d: %v", i, err)
		}
		var wantPacks []string
		wantRecords := 0
		for _, p := range want[i] {
			wantPacks = append(wantPacks, packs[p].name)
			wantRecords += sizes[p]
		}
		slices.Sort(wantPacks)
		var gotPacks []string
		for _, p := range table.packs {
			gotPacks = append(gotPacks, hex.EncodeToString(p[:]))
		}
		if !slices.Equal(gotPacks, wantPacks) || table.count() != wantRecords {
			t.Errorf("index object %d lists %d records of packs %v, want %d of %v",
				i, table.count(), gotPacks, wantRecords, wantPacks)
		}
		if !slices.IsSortedFunc(allRecords(table), compareRecords) {
			t.Errorf("index object %d is not sorted by blob name", i)
		}
	}

	// Every blob of every pack answers, whichever object lists it.
	loaded := newPackIndex(b, s, quietLogger)
	for _, p := range packs {
		for _, e := range []packEntry{p.entries[0], p.entries[len(p.entries)-1]} {
			pack, got, ok := mustLookup(t, loaded, hex.EncodeToString(e.name[:]))
			if !ok || pack != p.name || got != e {
				t.Fatalf("lookup = %s %+v %v, want %s %+v", pack, got, ok, p.name, e)
			}
		}
	}
}

func TestIndexDuplicateBlobResolvesToTheFirstCopy(t *testing.T) {
	s := testPackSpaces(t)[0]
	rng := rand.NewChaCha8([32]byte{2})
	one, two := syntheticPack(rng, 40), syntheticPack(rng, 40)
	// The same blob sits in both packs, at different places.
	shared := one.entries[7].name
	two.entries[31].name = shared
	first, firstEntry, otherEntry := one, one.entries[7], two.entries[31]
	if two.name < one.name {
		first, firstEntry, otherEntry = two, two.entries[31], one.entries[7]
	}
	if firstEntry.offset == otherEntry.offset {
		t.Fatal("both copies sit at the same offset, the test could not tell them apart")
	}

	for name, objects := range map[string][][]indexedPack{
		"one object":             {{one, two}},
		"one object, other way":  {{two, one}},
		"two objects":            {{one}, {two}},
		"two objects, other way": {{two}, {one}},
		"a pack listed twice":    {{two}, {one, two}, {one}},
	} {
		t.Run(name, func(t *testing.T) {
			b := newStubBackend()
			storePlaceholders(t, b, s, one, two)
			x := newPackIndex(b, s, quietLogger)
			for _, packs := range objects {
				writeTestIndex(t, x, packs)
			}
			pack, e, ok := mustLookup(t, x, hex.EncodeToString(shared[:]))
			if !ok || pack != first.name || e != firstEntry {
				t.Errorf("lookup = %s %+v %v, want the copy in the pack with the smaller name: %s %+v",
					pack, e, ok, first.name, firstEntry)
			}
			table := x.table.Load()
			if len(table.packs) != 2 || table.count() != len(one.entries)+len(two.entries) {
				t.Errorf("merged table has %d packs and %d records, want 2 and %d: nothing listed twice",
					len(table.packs), table.count(), len(one.entries)+len(two.entries))
			}
		})
	}
}

// TestIndexPassesOverAPackThatIsNotStored deletes a pack its index object
// still lists. A lookup of a blob in it finds nothing, and the copy of a blob
// in a stored pack instead, while the table keeps every record for Check, a
// prune and a rebuild. The load pays one List of the pack namespace for it.
// What the index writes after its load is not held against that listing.
func TestIndexPassesOverAPackThatIsNotStored(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			b := newIndexBackend(newStubBackend())
			if _, _, ok := mustLookup(t, newPackIndex(b, s, quietLogger), strings.Repeat("0", 64)); ok {
				t.Fatal("found a blob in an empty repository")
			}
			if lists, _, total := b.calls(s.packs); lists != 0 || total != 1 {
				t.Errorf("an index that lists no pack: %d Lists of the packs, %d requests; want 0 and 1", lists, total)
			}
			packs, _ := storeTestPacks(t, b, s, 1, 20)
			// The lost pack has the smallest name, the one a lookup prefers. A
			// second copy of one of its blobs is listed in the pack that sorts
			// last.
			lost := slices.MinFunc(packs, func(a, b indexedPack) int { return strings.Compare(a.name, b.name) })
			last := slices.MaxFunc(packs, func(a, b indexedPack) int { return strings.Compare(a.name, b.name) })
			twice := lost.entries[0].name
			copyOf := packEntry{name: twice, offset: 7, length: 9}
			listed := slices.Clone(packs)
			for i, p := range listed {
				if p.name == last.name {
					listed[i].entries = append(slices.Clone(p.entries), copyOf)
				}
			}
			objects := len(writeTestIndex(t, newPackIndex(b, s, quietLogger), listed))
			if err := b.Delete(ctx, Handle{Type: s.packs, Name: lost.name}); err != nil {
				t.Fatalf("delete pack: %v", err)
			}
			b.reset()

			x := newPackIndex(b, s, quietLogger)
			records := 0
			for _, p := range listed {
				for _, e := range p.entries {
					records++
					name := hex.EncodeToString(e.name[:])
					pack, got, ok := mustLookup(t, x, name)
					switch {
					case e.name == twice:
						if !ok || pack != last.name || got != copyOf {
							t.Errorf("lookup of the blob in two packs = %s %+v %v, want the stored copy %s %+v",
								pack, got, ok, last.name, copyOf)
						}
					case p.name == lost.name:
						if ok {
							t.Errorf("lookup found %s in %s, which is not stored", name, pack)
						}
					case !ok || pack != p.name || got != e:
						t.Errorf("lookup = %s %+v %v, want %s %+v", pack, got, ok, p.name, e)
					}
					if is, err := x.lists(ctx, name); err != nil || !is {
						t.Errorf("lists(%s) = %v, %v; want true: the record stays", name, is, err)
					}
				}
			}
			table := x.table.Load()
			gone, _ := table.pack(lost.name)
			if table.count() != records || len(table.packs) != len(packs) ||
				len(table.gone) != len(packs) || !table.gone[gone] || countTrue(table.gone) != 1 {
				t.Errorf("table: %d records of %d packs, gone %v; want %d of %d and only pack %d gone",
					table.count(), len(table.packs), table.gone, records, len(packs), gone)
			}
			if lists, gets, total := b.calls(s.packs); lists != 1 || gets != 0 || total != 2+objects {
				t.Errorf("pack namespace: %d Lists, %d Gets, %d requests in all; want 1, 0 and %d",
					lists, gets, total, 2+objects)
			}

			rng := rand.NewChaCha8([32]byte{7})
			fresh := syntheticPack(rng, 5)
			writeTestIndex(t, x, []indexedPack{fresh})
			for _, e := range fresh.entries {
				if pack, _, ok := mustLookup(t, x, hex.EncodeToString(e.name[:])); !ok || pack != fresh.name {
					t.Errorf("lookup of a blob written after the load = %s %v, want %s", pack, ok, fresh.name)
				}
				if is, err := x.lists(ctx, hex.EncodeToString(e.name[:])); err != nil || !is {
					t.Errorf("lists of a blob written after the load = %v, %v; want true", is, err)
				}
			}
			if lists, _, _ := b.calls(s.packs); lists != 1 {
				t.Errorf("%d Lists of the pack namespace after a write, want still 1", lists)
			}
		})
	}
}

func countTrue(marks []bool) int {
	n := 0
	for _, m := range marks {
		if m {
			n++
		}
	}
	return n
}

// TestIndexListsThePacksAfterItReadTheIndex has another writer store a pack
// and the index object that lists it while a load lists the index namespace.
// The load reads that object, so its pack listing has to be younger than it:
// taken first, it would lack the pack and the load would pass over blobs that
// are stored.
func TestIndexListsThePacksAfterItReadTheIndex(t *testing.T) {
	s := testPackSpaces(t)[0]
	b := newIndexBackend(newStubBackend())
	packs, _ := storeTestPacks(t, b, s, 1, 12)
	writeTestIndex(t, newPackIndex(b, s, quietLogger), packs)

	late := syntheticPack(rand.NewChaCha8([32]byte{9}), 5)
	var once sync.Once
	b.onList = func(typ HandleType) {
		if typ != s.index {
			return
		}
		once.Do(func() {
			storePlaceholders(t, b, s, late)
			writeTestIndex(t, newPackIndex(b.Backend, s, quietLogger), []indexedPack{late})
		})
	}
	x := newPackIndex(b, s, quietLogger)
	for _, p := range append(packs, late) {
		for _, e := range p.entries {
			if pack, _, ok := mustLookup(t, x, hex.EncodeToString(e.name[:])); !ok || pack != p.name {
				t.Fatalf("lookup = %s %v, want %s: the pack was stored before its index object", pack, ok, p.name)
			}
		}
	}
	if x.table.Load().gone != nil {
		t.Error("a load marked packs as gone that are all stored")
	}
}

// listErrBackend fails the List of one namespace while err is set.
type listErrBackend struct {
	Backend
	typ HandleType
	err error
}

func (b *listErrBackend) List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error {
	if t == b.typ && b.err != nil {
		return b.err
	}
	return b.Backend.List(ctx, t, fn)
}

// TestIndexLoadFailsWhenThePacksCannotBeListed: without the listing nothing
// says which packs are stored. The lookup fails instead of answering either
// way, and the next one lists the packs again without reading the index
// objects a second time.
func TestIndexLoadFailsWhenThePacksCannotBeListed(t *testing.T) {
	ctx := context.Background()
	s := testPackSpaces(t)[0]
	errList := errors.New("list failed")
	b := &listErrBackend{Backend: newStubBackend(), typ: s.packs, err: errList}
	packs, blobs := storeTestPacks(t, b, s, 1, 12)
	writeTestIndex(t, newPackIndex(b, s, quietLogger), packs)

	counted := newIndexBackend(b)
	x := newPackIndex(counted, s, quietLogger)
	if _, _, ok, err := x.lookup(ctx, blobs[0].name); !errors.Is(err, errList) || ok {
		t.Fatalf("lookup = %v, %v; want %v", ok, err, errList)
	}
	if x.table.Load() != nil {
		t.Fatal("a load whose pack listing failed left a table")
	}
	_, read, _ := counted.calls(s.index)
	b.err = nil
	for _, blob := range blobs {
		if _, _, ok := mustLookup(t, x, blob.name); !ok {
			t.Fatalf("blob %s is missing after the listing answered", blob.name)
		}
	}
	if lists, gets, _ := counted.calls(s.index); lists != 1 || gets != read {
		t.Errorf("index namespace after the retry: %d Lists, %d Gets; want 1 and %d", lists, gets, read)
	}
}

func TestIndexSkipsAnObjectThatDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.index.String(), func(t *testing.T) {
			b := newIndexBackend(newStubBackend())
			writer := newPackIndex(b, s, quietLogger)
			var objects []string
			var blobs [][]testBlob
			for seed := range byte(3) {
				packs, stored := storeTestPacks(t, b, s, seed, 12)
				objects = append(objects, writeTestIndex(t, writer, packs)...)
				blobs = append(blobs, stored)
			}
			broken := Handle{Type: s.index, Name: objects[1]}
			obj := getObject(t, b, broken)
			obj[len(obj)/2] ^= 0x01
			putObject(t, b, broken, obj)

			var logged bytes.Buffer
			x := newPackIndex(b, s, slog.New(slog.NewTextHandler(&logged, nil)))

			// A backend that fails is no index object that does not open:
			// the lookup fails and the next one loads again.
			b.getErr = errors.New("backend is down")
			if _, _, _, err := x.lookup(ctx, blobs[0][0].name); !errors.Is(err, b.getErr) {
				t.Fatalf("lookup on a failing backend: %v, want its error", err)
			}
			if logged.Len() != 0 {
				t.Fatalf("a failing backend was logged as a skipped object: %s", logged.String())
			}
			b.getErr = nil

			for i, stored := range blobs {
				for _, blob := range stored {
					if _, _, ok := mustLookup(t, x, blob.name); ok != (i != 1) {
						t.Fatalf("blob %s of index object %d: found %v", blob.name, i, ok)
					}
				}
			}
			if n := strings.Count(logged.String(), "level=WARN"); n != 1 {
				t.Errorf("%d warnings, want one: %s", n, logged.String())
			}
			if !strings.Contains(logged.String(), objects[1]) {
				t.Errorf("the warning does not name index object %s: %s", objects[1], logged.String())
			}
			if got := x.table.Load().unreadable; !slices.Equal(got, []string{objects[1]}) {
				t.Errorf("unreadable index objects %v, want %v", got, []string{objects[1]})
			}
		})
	}
}

func TestIndexRebuiltFromPacksEqualsTheWrittenOne(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.index.String(), func(t *testing.T) {
			b := newStubBackend()
			x := newPackIndex(b, s, quietLogger)
			for seed := range byte(3) {
				packs, _ := storeTestPacks(t, b, s, seed, 20)
				writeTestIndex(t, x, packs)
			}
			written, err := x.load(ctx)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			// What is no pack is left out of a rebuild and reported: a pack
			// cut short, one copied to another name, and a name that is none.
			cut := Handle{Type: s.packs, Name: strings.Repeat("ab", 32)}
			whole := getObject(t, b, Handle{Type: s.packs, Name: hex.EncodeToString(written.packs[0][:])})
			putObject(t, b, cut, whole[:len(whole)-9])
			copied := Handle{Type: s.packs, Name: strings.Repeat("cd", 32)}
			putObject(t, b, copied, whole)
			stray := Handle{Type: s.packs, Name: "not-a-pack"}
			putObject(t, b, stray, whole)

			var logged bytes.Buffer
			scanner := newPackIndex(b, s, slog.New(slog.NewTextHandler(&logged, nil)))
			packs, unreadable, err := scanner.scanPacks(ctx)
			if err != nil {
				t.Fatalf("scanPacks: %v", err)
			}
			if want := []string{cut.Name, copied.Name, stray.Name}; !slices.Equal(unreadable, want) {
				t.Errorf("unreadable packs %v, want %v", unreadable, want)
			}
			if n := strings.Count(logged.String(), "level=WARN"); n != 3 {
				t.Errorf("%d warnings, want three: %s", n, logged.String())
			}
			rebuilt, err := newIndexTable(packs)
			if err != nil {
				t.Fatalf("newIndexTable: %v", err)
			}
			if rebuilt.count() == 0 || !slices.Equal(rebuilt.packs, written.packs) ||
				!slices.Equal(allRecords(rebuilt), allRecords(written)) {
				t.Errorf("rebuilt index has %d packs and %d records, the written one %d and %d, or they differ",
					len(rebuilt.packs), rebuilt.count(), len(written.packs), written.count())
			}
		})
	}
}

func TestIndexTamperedObjectsAreRejected(t *testing.T) {
	ctx := context.Background()
	spaces := testPackSpaces(t)
	for i, s := range spaces {
		t.Run(s.index.String(), func(t *testing.T) {
			b := newStubBackend()
			x := newPackIndex(b, s, quietLogger)
			packs, _ := storeTestPacks(t, b, s, 1, 12)
			name := writeTestIndex(t, x, packs)[0]
			h := Handle{Type: s.index, Name: name}
			obj := getObject(t, b, h)
			content, err := s.codec.open(s.dataKey, objectAAD(h), obj)
			if err != nil {
				t.Fatalf("open the index object: %v", err)
			}

			if err := x.read(ctx, name, &indexTable{}); err != nil {
				t.Fatalf("read the unchanged index object: %v", err)
			}
			if err := x.read(ctx, strings.Repeat("0", 64), &indexTable{}); !errors.Is(err, ErrNotFound) ||
				errors.Is(err, ErrCorruptIndex) {
				t.Errorf("read a missing index object: %v, want ErrNotFound", err)
			}

			// sealed returns content as an index object that authenticates
			// under the name it is read from.
			sealed := func(content []byte) []byte { return s.codec.seal(s.dataKey, objectAAD(h), content) }
			flipped := func(at int) []byte {
				changed := bytes.Clone(obj)
				changed[at] ^= 0x80
				return changed
			}
			outOfTable := bytes.Clone(content)
			binary.LittleEndian.PutUint32(outOfTable[len(outOfTable)-12:], uint32(len(packs)))

			other := spaces[1-i]
			moved := Handle{Type: s.index, Name: strings.Repeat("1", 64)}
			foreign := Handle{Type: other.index, Name: name}
			for cell, c := range map[string]struct {
				at  Handle
				obj []byte
			}{
				"format byte flipped":       {h, flipped(0)},
				"nonce flipped":             {h, flipped(5)},
				"ciphertext flipped":        {h, flipped(len(obj) / 2)},
				"tag flipped":               {h, flipped(len(obj) - 1)},
				"truncated":                 {h, obj[:len(obj)/2]},
				"empty":                     {h, nil},
				"moved to another name":     {moved, obj},
				"moved from the other type": {foreign, obj},
				"content cut short":         {h, sealed(content[:len(content)-1])},
				"content with a byte more":  {h, sealed(append(bytes.Clone(content), 0))},
				"content below the header":  {h, sealed(content[:indexHeaderSize-1])},
				"record outside the table":  {h, sealed(outOfTable)},
			} {
				putObject(t, b, c.at, c.obj)
				reader := x
				if c.at.Type != s.index {
					reader = newPackIndex(b, other, quietLogger)
				}
				table := &indexTable{}
				if err := reader.read(ctx, c.at.Name, table); !errors.Is(err, ErrCorruptIndex) {
					t.Errorf("%s: read = %v, want ErrCorruptIndex", cell, err)
				}
				if len(table.packs) != 0 || table.count() != 0 {
					t.Errorf("%s: the rejected object left %d packs and %d records in the table",
						cell, len(table.packs), table.count())
				}
			}
		})
	}
}

func getObject(t *testing.T, b Backend, h Handle) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("Get %s object %s: %v", h.Type, h.Name, err)
	}
	defer rc.Close()
	obj, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s object %s: %v", h.Type, h.Name, err)
	}
	return obj
}

func putObject(t *testing.T, b Backend, h Handle, obj []byte) {
	t.Helper()
	if err := b.Put(context.Background(), h, bytes.NewReader(obj)); err != nil {
		t.Fatalf("Put %s object %s: %v", h.Type, h.Name, err)
	}
}
