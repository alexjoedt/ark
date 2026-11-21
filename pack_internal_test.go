package ark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// packLog records what a packWriter reports.
type packLog struct {
	mu    sync.Mutex
	packs map[string][]packEntry
	order []string
}

func (l *packLog) stored(_ context.Context, pack string, entries []packEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.packs == nil {
		l.packs = make(map[string][]packEntry)
	}
	l.packs[pack] = entries
	l.order = append(l.order, pack)
	return nil
}

type testBlob struct {
	name         string
	data, sealed []byte
}

// testBlobs returns one sealed blob of random content per size, named by a
// hash of its content like a stored chunk or tree node.
func testBlobs(s *packSpace, seed byte, sizes ...int) []testBlob {
	rng := rand.NewChaCha8([32]byte{seed})
	blobs := make([]testBlob, len(sizes))
	for i, size := range sizes {
		data := make([]byte, size)
		_, _ = rng.Read(data)
		sum := sha256.Sum256(data)
		name := hex.EncodeToString(sum[:])
		sealed := s.codec.seal(s.dataKey, objectAAD(Handle{Type: s.blobs, Name: name}), data)
		blobs[i] = testBlob{name: name, data: data, sealed: sealed}
	}
	return blobs
}

func testPackSpaces(t *testing.T) []*packSpace {
	t.Helper()
	codec := newObjectCodec()
	t.Cleanup(codec.Close)
	key := newMasterKey("repo")
	return []*packSpace{newPackSpace(codec, key, BlobFile), newPackSpace(codec, key, TreeFile)}
}

func otherCorruptErr(s *packSpace) error {
	if errors.Is(s.corruptErr, ErrCorruptChunk) {
		return ErrCorruptTree
	}
	return ErrCorruptChunk
}

func getPack(t *testing.T, b Backend, s *packSpace, name string) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), Handle{Type: s.packs, Name: name})
	if err != nil {
		t.Fatalf("Get pack %s: %v", name, err)
	}
	defer rc.Close()
	pack, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read pack %s: %v", name, err)
	}
	return pack
}

func blobSection(entries []packEntry) int {
	n := 0
	for _, e := range entries {
		n += int(e.length)
	}
	return n
}

// checkPacks reads everything the writer reported back from b: the stored
// packs are the reported ones, each is named by the SHA-256 of its bytes and
// describes itself with the reported table, and every blob was reported once
// and opens to its content through a range read.
func checkPacks(t *testing.T, s *packSpace, b Backend, log *packLog, blobs []testBlob) {
	t.Helper()
	ctx := context.Background()

	var listed []string
	err := b.List(ctx, s.packs, func(info BlobInfo) error {
		listed = append(listed, info.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(listed)
	reported := slices.Sorted(slices.Values(log.order))
	if !slices.Equal(listed, reported) {
		t.Fatalf("stored packs %v, reported %v", listed, reported)
	}

	byName := make(map[string]testBlob, len(blobs))
	for _, blob := range blobs {
		byName[blob.name] = blob
	}
	seen := make(map[string]int, len(blobs))
	for pack, entries := range log.packs {
		obj := getPack(t, b, s, pack)
		if sum := sha256.Sum256(obj); hex.EncodeToString(sum[:]) != pack {
			t.Errorf("pack %s is not named by the SHA-256 of its bytes", pack)
		}
		parsed, err := s.parse(obj)
		if err != nil {
			t.Fatalf("parse pack %s: %v", pack, err)
		}
		if !slices.Equal(parsed, entries) {
			t.Errorf("pack %s describes %v, the writer reported %v", pack, parsed, entries)
		}
		for _, e := range entries {
			name := hex.EncodeToString(e.name[:])
			seen[name]++
			data, err := s.readBlob(ctx, b, pack, e)
			if err != nil {
				t.Fatalf("readBlob %s: %v", name, err)
			}
			if !bytes.Equal(data, byName[name].data) {
				t.Errorf("blob %s in pack %s does not read back as written", name, pack)
			}
		}
	}
	for _, blob := range blobs {
		if seen[blob.name] != 1 {
			t.Errorf("blob %s was reported %d times, want once", blob.name, seen[blob.name])
		}
	}
}

func TestPackWriterClosesPacksAtTheTargetSize(t *testing.T) {
	ctx := context.Background()
	s := testPackSpaces(t)[0]
	b := newStubBackend()
	var log packLog
	w := newPackWriter(b, s, newPackUploads(), log.stored)

	const mib = 1 << 20
	sizes := []int{3 * mib, 3 * mib, 3 * mib, 3 * mib, 3 * mib, 3 * mib, 3 * mib, 17 * mib}
	blobs := testBlobs(s, 1, sizes...)

	for i, blob := range blobs[:5] {
		if err := w.add(ctx, blob.name, blob.sealed); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if len(log.order) != 0 {
		t.Fatalf("%d packs stored below the target size", len(log.order))
	}

	// The sixth blob crosses 16 MiB: the pack closes with it inside.
	if err := w.add(ctx, blobs[5].name, blobs[5].sealed); err != nil {
		t.Fatal(err)
	}
	if len(log.order) != 1 {
		t.Fatalf("%d packs stored after crossing the target size, want 1", len(log.order))
	}
	full := log.packs[log.order[0]]
	size := blobSection(full)
	if len(full) != 6 || size < packTargetSize || size-int(full[5].length) >= packTargetSize {
		t.Errorf("full pack holds %d blobs and %d bytes, want 6 blobs, one over %d", len(full), size, packTargetSize)
	}

	// A blob above the target gets its own pack and leaves the open one alone.
	if err := w.add(ctx, blobs[6].name, blobs[6].sealed); err != nil {
		t.Fatal(err)
	}
	if err := w.add(ctx, blobs[7].name, blobs[7].sealed); err != nil {
		t.Fatal(err)
	}
	if len(log.order) != 2 {
		t.Fatalf("%d packs stored after the oversized blob, want 2", len(log.order))
	}
	if own := log.packs[log.order[1]]; len(own) != 1 || hex.EncodeToString(own[0].name[:]) != blobs[7].name {
		t.Errorf("the oversized blob shares its pack: %d entries", len(own))
	}

	// The last pack of a run is written as short as it is.
	if err := w.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(log.order) != 3 {
		t.Fatalf("%d packs stored after flush, want 3", len(log.order))
	}
	if short := log.packs[log.order[2]]; len(short) != 1 || hex.EncodeToString(short[0].name[:]) != blobs[6].name {
		t.Errorf("the last pack holds %d entries, want the one open blob", len(short))
	}
	if err := w.flush(ctx); err != nil || len(log.order) != 3 {
		t.Errorf("a second flush stored a pack or failed: %v", err)
	}
	checkPacks(t, s, b, &log, blobs)
}

func TestPackBlobsReadBackThroughOneRangeRead(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			b := Instrument(newStubBackend())
			var log packLog
			w := newPackWriter(b, s, newPackUploads(), log.stored)
			w.target = 4 << 10

			sizes := make([]int, 60)
			for i := range sizes {
				sizes[i] = 1 + (i*211)%1500
			}
			blobs := testBlobs(s, 2, sizes...)
			for _, blob := range blobs {
				if err := w.add(ctx, blob.name, blob.sealed); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.flush(ctx); err != nil {
				t.Fatal(err)
			}
			if len(log.order) < 5 {
				t.Fatalf("fixture wrote %d packs, want several", len(log.order))
			}

			before := b.Stats()
			for pack, entries := range log.packs {
				for _, e := range entries {
					if _, err := s.readBlob(ctx, b, pack, e); err != nil {
						t.Fatal(err)
					}
				}
			}
			after := b.Stats()
			if got := after.GetRange.Calls - before.GetRange.Calls; got != int64(len(blobs)) {
				t.Errorf("%d range reads for %d blobs", got, len(blobs))
			}
			if after.Get.Calls != before.Get.Calls {
				t.Errorf("reading blobs issued %d whole-object Gets", after.Get.Calls-before.Get.Calls)
			}
			checkPacks(t, s, b, &log, blobs)
		})
	}
}

func TestPackWriterRejectsNamesThatAreNotObjectNames(t *testing.T) {
	s := testPackSpaces(t)[0]
	var log packLog
	w := newPackWriter(newStubBackend(), s, newPackUploads(), log.stored)
	good := testBlobs(s, 3, 10)[0]
	upper := strings.ToUpper(good.name)
	if upper == good.name {
		t.Fatal("fixture name has no letter")
	}
	for _, name := range []string{"", good.name[:62], good.name + "00", "zz" + good.name[2:], upper} {
		if err := w.add(context.Background(), name, good.sealed); err == nil {
			t.Errorf("add accepted the name %q", name)
		}
	}
}

// A blob shorter than a seal never opens: it is refused when written, not
// found at restore.
func TestPackWriterRejectsBlobsTooShortToBeSealed(t *testing.T) {
	ctx := context.Background()
	s := testPackSpaces(t)[0]
	var log packLog
	w := newPackWriter(newStubBackend(), s, newPackUploads(), log.stored)
	good := testBlobs(s, 3, 10)[0]
	for _, sealed := range [][]byte{nil, good.sealed[:sealOverhead-1]} {
		if err := w.add(ctx, good.name, sealed); err == nil {
			t.Errorf("add accepted a sealed blob of %d bytes", len(sealed))
		}
	}
	if err := w.flush(ctx); err != nil || len(log.order) != 0 {
		t.Errorf("rejected blobs left %d packs behind, flush: %v", len(log.order), err)
	}
}

// writeOnePack stores blobs as a single pack and returns its name, bytes and
// table.
func writeOnePack(t *testing.T, s *packSpace, b Backend, blobs []testBlob) (string, []byte, []packEntry) {
	t.Helper()
	var log packLog
	w := newPackWriter(b, s, newPackUploads(), log.stored)
	for _, blob := range blobs {
		if err := w.add(context.Background(), blob.name, blob.sealed); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(log.order) != 1 {
		t.Fatalf("fixture wrote %d packs, want 1", len(log.order))
	}
	name := log.order[0]
	return name, getPack(t, b, s, name), log.packs[name]
}

func TestPackTamperedBytesAreRejected(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			b := newStubBackend()
			blobs := testBlobs(s, 4, 40, 1, 90)
			name, pack, entries := writeOnePack(t, s, b, blobs)
			h := Handle{Type: s.packs, Name: name}
			section := blobSection(entries)
			rejected := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, s.corruptErr) || errors.Is(err, otherCorruptErr(s)) {
					t.Errorf("%s: got %v, want %v", what, err, s.corruptErr)
				}
			}

			// Every byte: blobs, sealed table, trailer.
			for i := range pack {
				bad := bytes.Clone(pack)
				bad[i] ^= 0x01
				_, err := s.parse(bad)
				rejected("parse with flipped byte", err)
				if i >= section {
					continue
				}
				if err := b.Put(ctx, h, bytes.NewReader(bad)); err != nil {
					t.Fatal(err)
				}
				for k, e := range entries {
					data, err := s.readBlob(ctx, b, name, e)
					hit := i >= int(e.offset) && i < int(e.offset+e.length)
					switch {
					case hit:
						rejected("readBlob of the flipped blob", err)
					case err != nil || !bytes.Equal(data, blobs[k].data):
						t.Errorf("flipped byte %d broke blob %d outside it: %v", i, k, err)
					}
				}
			}

			// Every truncation.
			last := entries[len(entries)-1]
			for n := range pack {
				_, err := s.parse(pack[:n])
				rejected("parse of a truncated pack", err)
				if n >= section {
					continue
				}
				if err := b.Put(ctx, h, bytes.NewReader(pack[:n])); err != nil {
					t.Fatal(err)
				}
				_, err = s.readBlob(ctx, b, name, last)
				rejected("readBlob past the end of a truncated pack", err)
			}

			// A pack of the other namespace does not parse here.
			for _, other := range testPackSpaces(t) {
				if other.packs == s.packs {
					continue
				}
				other.dataKey = s.dataKey
				_, err := other.parse(pack)
				if !errors.Is(err, other.corruptErr) {
					t.Errorf("a %s object parsed as %s: %v", s.packs, other.packs, err)
				}
			}
		})
	}
}

// splicedPack returns the blobs of one pack followed by the table of another
// of the same shape, so only the binding tells them apart, with the entries
// of that table.
func splicedPack(t *testing.T, s *packSpace) ([]byte, []packEntry) {
	t.Helper()
	_, one, oneEntries := writeOnePack(t, s, newStubBackend(), testBlobs(s, 5, 64, 200, 7))
	_, two, twoEntries := writeOnePack(t, s, newStubBackend(), testBlobs(s, 6, 64, 200, 7))
	section := blobSection(oneEntries)
	if len(one) != len(two) || section != blobSection(twoEntries) {
		t.Fatalf("fixture packs differ in shape: %d and %d bytes", len(one), len(two))
	}
	for i := range oneEntries {
		if oneEntries[i].length != twoEntries[i].length {
			t.Fatalf("fixture blob %d differs in sealed length", i)
		}
	}
	return append(bytes.Clone(two[:section]), one[section:]...), oneEntries
}

// A reader that holds the whole pack compares the sum the table carries with
// its blobs. The seal of the table is intact: only the sum refuses it.
func TestPackTableBehindOtherBlobsIsRefusedByItsSum(t *testing.T) {
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			spliced, _ := splicedPack(t, s)
			_, err := s.parse(spliced)
			if !errors.Is(err, s.corruptErr) || errors.Is(err, errSealBroken) {
				t.Errorf("a table behind other blobs: got %v, want %v by its sum", err, s.corruptErr)
			}
		})
	}
}

// What a reader of the tail gives up: without the blob section it cannot
// compare the sum, so the table of another pack opens. The fixture packs hold
// no blob in common, so every blob it then reads is one the table does not
// name, and none opens.
func TestPackTableOfAnotherPackOpensFromATailReadAndNoOtherBlobDoes(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			spliced, want := splicedPack(t, s)
			b := newStubBackend()
			h := Handle{Type: s.packs, Name: "spliced"}
			if err := b.Put(ctx, h, bytes.NewReader(spliced)); err != nil {
				t.Fatal(err)
			}
			entries := tailTable(t, s, b, h, int64(len(spliced)))
			if !slices.Equal(entries, want) {
				t.Fatalf("the swapped table lists %v, want the entries of its own pack %v", entries, want)
			}
			for _, e := range entries {
				if _, err := s.readBlob(ctx, b, h.Name, e); !errors.Is(err, s.corruptErr) {
					t.Errorf("blob %x read through a swapped table: got %v, want %v", e.name, err, s.corruptErr)
				}
			}
		})
	}
}

// tailTable opens the blob table of the pack of size bytes stored under h the
// way a reader does that never fetches the blobs: one range read of the
// trailer, one of the sealed table.
func tailTable(t *testing.T, s *packSpace, b Backend, h Handle, size int64) []packEntry {
	t.Helper()
	ctx := context.Background()
	trailer, err := b.GetRange(ctx, h, size-packTrailerSize, packTrailerSize)
	if err != nil {
		t.Fatalf("read the trailer of %s: %v", h.Name, err)
	}
	n := int64(binary.LittleEndian.Uint32(trailer))
	if n > size-packTrailerSize {
		t.Fatalf("the trailer of %s gives a table of %d bytes, the pack has %d", h.Name, n, size)
	}
	sealed, err := b.GetRange(ctx, h, size-packTrailerSize-n, n)
	if err != nil {
		t.Fatalf("read the table of %s: %v", h.Name, err)
	}
	entries, _, err := s.openTable(sealed)
	if err != nil {
		t.Fatalf("open the table of %s from its tail: %v", h.Name, err)
	}
	return entries
}

// rangeLog records the range reads that reach a backend.
type rangeLog struct {
	Backend
	ranges [][2]int64
}

func (l *rangeLog) GetRange(ctx context.Context, h Handle, offset, length int64) ([]byte, error) {
	l.ranges = append(l.ranges, [2]int64{offset, length})
	return l.Backend.GetRange(ctx, h, offset, length)
}

func (l *rangeLog) Get(context.Context, Handle) (io.ReadCloser, error) {
	return nil, errors.New("a tail read fetched the whole pack")
}

// The table opens from the tail of its pack: the size from a listing, the
// last four bytes, then the table. No byte of the blob section is read.
func TestPackTableOpensFromATailRead(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			b := newStubBackend()
			blobs := testBlobs(s, 12, 3000, 1, 64, 500, 9000)
			name, pack, want := writeOnePack(t, s, b, blobs)

			size := int64(-1)
			err := b.List(ctx, s.packs, func(info BlobInfo) error {
				if info.Name == name {
					size = info.Size
				}
				return nil
			})
			if err != nil || size != int64(len(pack)) {
				t.Fatalf("List gives %d bytes for the pack of %d: %v", size, len(pack), err)
			}

			log := &rangeLog{Backend: b}
			got := tailTable(t, s, log, Handle{Type: s.packs, Name: name}, size)
			if !slices.Equal(got, want) {
				t.Errorf("a tail read lists %v, the writer reported %v", got, want)
			}
			for i, blob := range blobs {
				if hex.EncodeToString(got[i].name[:]) != blob.name || int(got[i].length) != len(blob.sealed) {
					t.Errorf("entry %d is %x with %d bytes, want %s with %d",
						i, got[i].name, got[i].length, blob.name, len(blob.sealed))
				}
			}
			section := int64(blobSection(want))
			if len(log.ranges) != 2 {
				t.Fatalf("a tail read took %d range reads, want 2", len(log.ranges))
			}
			for _, r := range log.ranges {
				if r[0] < section {
					t.Errorf("a tail read started at %d, inside the %d bytes of blobs", r[0], section)
				}
			}

			// The blobs then open by range, each under its own name.
			for i, e := range got {
				data, err := s.readBlob(ctx, b, name, e)
				if err != nil || !bytes.Equal(data, blobs[i].data) {
					t.Errorf("blob %d read through the tail's table: %v", i, err)
				}
			}
		})
	}
}

// gatedPackWriters returns a data and a tree pack writer over one backend
// whose Puts wait for release. Both share the upload slots, as the writers of
// one repository do. Every blob pair fills a pack.
func gatedPackWriters(t *testing.T, release <-chan struct{}) (*gatedBackend, []*packWriter, [][]testBlob, *packLog) {
	t.Helper()
	b := &gatedBackend{stubBackend: newStubBackend()}
	b.hook = func(ctx context.Context, _ int32) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	uploads := newPackUploads()
	log := &packLog{}
	var writers []*packWriter
	var blobs [][]testBlob
	for i, s := range testPackSpaces(t) {
		sizes := make([]int, 32)
		for k := range sizes {
			sizes[k] = 100
		}
		set := testBlobs(s, byte(10+i), sizes...)
		w := newPackWriter(b, s, uploads, log.stored)
		w.target = len(set[0].sealed) + 1
		writers = append(writers, w)
		blobs = append(blobs, set)
	}
	return b, writers, blobs, log
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPackWriterBoundsUploadsInFlight(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	b, writers, blobs, log := gatedPackWriters(t, release)

	// 8 goroutines per writer, 4 blobs each: 16 packs per writer.
	var returned atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for wi, w := range writers {
		for g := range 8 {
			wg.Go(func() {
				for _, blob := range blobs[wi][g*4 : g*4+4] {
					if err := w.add(ctx, blob.name, blob.sealed); err != nil {
						errs <- err
						return
					}
					returned.Add(1)
				}
			})
		}
	}

	waitFor(t, "4 uploads in flight", func() bool { return b.inFlight.Load() == packUploads })
	time.Sleep(100 * time.Millisecond)
	if got := b.calls.Load(); got != packUploads {
		t.Errorf("%d pack Puts started while %d are held, want no more", got, packUploads)
	}
	// Per held upload one add returned, the one that half filled the pack.
	// Beyond that each writer takes one more blob into its open pack, then
	// the add that fills it waits for a slot and every other add behind it.
	if got := returned.Load(); got > packUploads+int32(len(writers)) {
		t.Errorf("%d adds returned while every upload slot is busy, want at most %d", got, packUploads+len(writers))
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("add: %v", err)
	}
	for _, w := range writers {
		if err := w.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	peak := b.peak
	b.mu.Unlock()
	if peak != packUploads {
		t.Errorf("peak of %d pack uploads in flight, want %d", peak, packUploads)
	}
	if len(log.order) != 32 {
		t.Errorf("%d packs stored, want 32", len(log.order))
	}
}

func TestPackWriterAddStopsOnCancelWhileUploadsAreHeld(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, writers, blobs, _ := gatedPackWriters(t, nil)

	done := make(chan struct{})
	var failed atomic.Int32
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for wi, w := range writers {
			for g := range 8 {
				wg.Go(func() {
					for _, blob := range blobs[wi][g*4 : g*4+4] {
						if err := w.add(ctx, blob.name, blob.sealed); err != nil {
							if !errors.Is(err, context.Canceled) {
								t.Errorf("add: %v, want the context error", err)
							}
							failed.Add(1)
							return
						}
					}
				})
			}
		}
		wg.Wait()
	}()

	waitFor(t, "4 uploads in flight", func() bool { return b.inFlight.Load() == packUploads })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("adds still blocked after the context was cancelled")
	}
	// The four adds held in a Put and the ones waiting for a slot fail; a
	// goroutine whose adds never filled a pack may have finished before.
	if failed.Load() < packUploads {
		t.Errorf("%d goroutines stopped with the context error, want at least %d", failed.Load(), packUploads)
	}
}

// A pack that is not stored held blobs whose add had returned, so the writer
// reports it on every later call: no run ends well without them.
func TestPackWriterFailsForGoodAfterALostPack(t *testing.T) {
	ctx := context.Background()
	s := testPackSpaces(t)[0]
	errPut := errors.New("put failed")
	b := &gatedBackend{stubBackend: newStubBackend()}
	b.hook = func(_ context.Context, call int32) error {
		if call == 1 {
			return errPut
		}
		return nil
	}
	var log packLog
	w := newPackWriter(b, s, newPackUploads(), log.stored)
	blobs := testBlobs(s, 9, 100, 100, 100, 300)
	w.target = len(blobs[0].sealed) + 1

	if err := w.add(ctx, blobs[0].name, blobs[0].sealed); err != nil {
		t.Fatal(err)
	}
	if err := w.add(ctx, blobs[1].name, blobs[1].sealed); !errors.Is(err, errPut) {
		t.Fatalf("the add that fills the pack: %v, want %v", err, errPut)
	}
	// The first add had returned nil and its blob went with the pack.
	for _, blob := range blobs[2:] {
		if err := w.add(ctx, blob.name, blob.sealed); !errors.Is(err, errPut) {
			t.Errorf("add of %d bytes after a lost pack: %v, want %v", len(blob.sealed), err, errPut)
		}
	}
	if err := w.flush(ctx); !errors.Is(err, errPut) {
		t.Errorf("flush after a lost pack: %v, want %v", err, errPut)
	}
	if len(log.order) != 0 || b.calls.Load() != 1 {
		t.Errorf("%d packs reported and %d Puts after a lost pack, want 0 and 1", len(log.order), b.calls.Load())
	}
}

// No add or flush waits longer than its context lives, for the open pack or
// for an upload slot, and a blob whose add failed is in no pack.
func TestPackWriterWaitsEndWithTheContext(t *testing.T) {
	live := context.Background()
	s := testPackSpaces(t)[0]
	b := newStubBackend()
	var log packLog
	uploads := newPackUploads()
	w := newPackWriter(b, s, uploads, log.stored)
	blobs := testBlobs(s, 11, 100, 100, 100)
	w.target = len(blobs[0].sealed) + 1
	for range packUploads {
		uploads <- struct{}{}
	}

	if err := w.add(live, blobs[0].name, blobs[0].sealed); err != nil {
		t.Fatal(err)
	}
	// The add that fills the pack holds it while it waits for a slot.
	ctx, cancel := context.WithCancel(live)
	defer cancel()
	filled := make(chan error, 1)
	go func() { filled <- w.add(ctx, blobs[1].name, blobs[1].sealed) }()
	waitFor(t, "the add that waits for a slot", func() bool { return len(w.open) == 1 })

	short, stop := context.WithTimeout(live, 20*time.Millisecond)
	defer stop()
	if err := w.add(short, blobs[2].name, blobs[2].sealed); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("add behind a held pack: %v, want the deadline", err)
	}
	if err := w.flush(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("flush behind a held pack: %v, want the deadline", err)
	}
	cancel()
	if err := <-filled; !errors.Is(err, context.Canceled) {
		t.Errorf("add waiting for a slot: %v, want the context error", err)
	}

	for range packUploads {
		<-uploads
	}
	if err := w.add(ctx, blobs[2].name, blobs[2].sealed); !errors.Is(err, context.Canceled) {
		t.Errorf("add with a done context: %v, want the context error", err)
	}
	if err := w.flush(live); err != nil {
		t.Fatal(err)
	}
	checkPacks(t, s, b, &log, blobs[:1])

	// The rejected blobs can be added again.
	for _, blob := range blobs[1:] {
		if err := w.add(live, blob.name, blob.sealed); err != nil {
			t.Fatal(err)
		}
	}
	checkPacks(t, s, b, &log, blobs)
}

func TestPackWriterConcurrentAdds(t *testing.T) {
	ctx := context.Background()
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			b := newStubBackend()
			var log packLog
			w := newPackWriter(b, s, newPackUploads(), log.stored)
			w.target = 16 << 10

			const workers, perWorker = 8, 100
			sizes := make([]int, workers*perWorker)
			for i := range sizes {
				sizes[i] = 1 + (i*977)%3000
				if i%50 == 49 {
					sizes[i] = 20 << 10 // above the target: a pack of its own
				}
			}
			blobs := testBlobs(s, 7, sizes...)

			var wg sync.WaitGroup
			for g := range workers {
				wg.Go(func() {
					for _, blob := range blobs[g*perWorker : (g+1)*perWorker] {
						if err := w.add(ctx, blob.name, blob.sealed); err != nil {
							t.Errorf("add: %v", err)
							return
						}
					}
				})
			}
			wg.Wait()
			if err := w.flush(ctx); err != nil {
				t.Fatal(err)
			}

			for pack, entries := range log.packs {
				size := blobSection(entries)
				if len(entries) > 1 && size-int(entries[len(entries)-1].length) >= w.target {
					t.Errorf("pack %s kept filling beyond the target: %d bytes", pack, size)
				}
			}
			checkPacks(t, s, b, &log, blobs)
		})
	}
}

// A table that authenticates but does not describe its blob section is a
// writer bug no reader should build on.
func TestPackTableThatDisagreesWithItsBlobsIsRejected(t *testing.T) {
	for _, s := range testPackSpaces(t) {
		t.Run(s.packs.String(), func(t *testing.T) {
			blobs := testBlobs(s, 8, 30, 50)
			section := slices.Concat(blobs[0].sealed, blobs[1].sealed)
			entries := []packEntry{
				{length: uint32(len(blobs[0].sealed))},
				{offset: uint32(len(blobs[0].sealed)), length: uint32(len(blobs[1].sealed))},
			}
			tail, _ := s.finish(slices.Clone(entries), section)
			if got, err := s.parse(slices.Concat(section, tail)); err != nil || !slices.Equal(got, entries) {
				t.Fatalf("control pack does not parse: %v", err)
			}

			short := slices.Clone(entries)
			short[1].length--
			tail, _ = s.finish(short, section)
			if _, err := s.parse(slices.Concat(section, tail)); !errors.Is(err, s.corruptErr) {
				t.Errorf("a table listing fewer bytes than the pack holds: got %v, want %v", err, s.corruptErr)
			}

			// One entry covering the whole section, then a stray byte.
			table := binary.LittleEndian.AppendUint32(make([]byte, sha256.Size), uint32(len(section)))
			sum := sha256.Sum256(section)
			tail = s.codec.seal(s.dataKey, s.tableAAD(), append(slices.Concat(sum[:], table), 0))
			tail = append(tail, byte(len(tail)), 0, 0, 0)
			if _, err := s.parse(slices.Concat(section, tail)); !errors.Is(err, s.corruptErr) {
				t.Errorf("a table with a cut entry: got %v, want %v", err, s.corruptErr)
			}
		})
	}
}
