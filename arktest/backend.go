// Package arktest holds the conformance suites every ark implementation runs.
//
// The suites are ordinary Go tests driven by a factory, so an out-of-tree
// backend proves the same contract as the in-tree ones:
//
//	func TestConformance(t *testing.T) {
//		arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
//			return mybackend.New()
//		})
//	}
//
// The package depends on the standard library and ark itself, nothing else.
package arktest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
)

// conformanceTypes are the handle types every backend stores. They are listed
// by constant rather than derived, so a new type is a deliberate addition to
// the suite.
var conformanceTypes = []ark.HandleType{
	ark.ConfigFile,
	ark.BlobFile,
	ark.TreeFile,
	ark.SnapshotFile,
	ark.LockFile,
	ark.KeyFile,
}

// chunkNameLen is the length of a chunk name the suite generates: a content
// hash in hex, long enough for a backend that shards on the first characters.
const chunkNameLen = 64

// objectName returns a distinct name for type typ and suffix. Chunk names look
// like a content hash, 64 hex characters, so a backend that shards on the first
// few is exercised; the rest are opaque.
func objectName(typ ark.HandleType, suffix string) string {
	if typ == ark.BlobFile {
		h := fmt.Sprintf("%x", suffix)
		return strings.Repeat("f", chunkNameLen-len(h)) + h
	}
	return typ.String() + "-" + suffix
}

// RunBackendTests checks a backend against the contract documented on
// ark.Backend. factory is called once per sub-test and returns a backend with
// no objects in it; the suite closes what it creates.
//
// Every sub-test is independent, so a failure names the clause it broke.
func RunBackendTests(t *testing.T, factory func(t *testing.T) ark.Backend) {
	t.Helper()

	newBackend := func(t *testing.T) ark.Backend {
		t.Helper()
		b := factory(t)
		if b == nil {
			t.Fatal("factory returned a nil backend")
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}

	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, newBackend) })
	t.Run("Namespaces", func(t *testing.T) { testNamespaces(t, newBackend) })
	t.Run("PutIsIdempotent", func(t *testing.T) { testPutIsIdempotent(t, newBackend) })
	t.Run("PutIsAtomic", func(t *testing.T) { testPutIsAtomic(t, newBackend) })
	t.Run("PutEmptyObject", func(t *testing.T) { testPutEmptyObject(t, newBackend) })
	t.Run("MissingObject", func(t *testing.T) { testMissingObject(t, newBackend) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, newBackend) })
	t.Run("ListSizes", func(t *testing.T) { testListSizes(t, newBackend) })
	t.Run("ListStopsOnError", func(t *testing.T) { testListStopsOnError(t, newBackend) })
	t.Run("ConcurrentPuts", func(t *testing.T) { testConcurrentPuts(t, newBackend) })
	t.Run("ConcurrentMixed", func(t *testing.T) { testConcurrentMixed(t, newBackend) })
	t.Run("Close", func(t *testing.T) { testClose(t, factory) })
}

// get reads the object under h and fails the test if it cannot.
func get(t *testing.T, b ark.Backend, h ark.Handle) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("Get %v: %v", h, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %v: %v", h, err)
	}
	return data
}

// put stores payload under h and fails the test if it cannot.
func put(t *testing.T, b ark.Backend, h ark.Handle, payload []byte) {
	t.Helper()
	if err := b.Put(context.Background(), h, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put %v: %v", h, err)
	}
}

// list collects the namespace of typ into a name-keyed map.
func list(t *testing.T, b ark.Backend, typ ark.HandleType) map[string]int64 {
	t.Helper()
	got := make(map[string]int64)
	err := b.List(context.Background(), typ, func(info ark.BlobInfo) error {
		if _, dup := got[info.Name]; dup {
			t.Errorf("List %v yielded %q twice", typ, info.Name)
		}
		got[info.Name] = info.Size
		return nil
	})
	if err != nil {
		t.Fatalf("List %v: %v", typ, err)
	}
	return got
}

// testRoundTrip stores one object per handle type and reads it back through
// Get, Exists and Stat.
func testRoundTrip(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)

	for _, typ := range conformanceTypes {
		h := ark.Handle{Type: typ, Name: objectName(typ, "01")}
		want := []byte("payload of " + h.Name)
		put(t, b, h, want)

		if got := get(t, b, h); !bytes.Equal(got, want) {
			t.Errorf("Get %v = %q, want %q", h, got, want)
		}

		ok, err := b.Exists(ctx, h)
		if err != nil {
			t.Errorf("Exists %v: %v", h, err)
		} else if !ok {
			t.Errorf("Exists %v = false, want true", h)
		}

		info, err := b.Stat(ctx, h)
		if err != nil {
			t.Errorf("Stat %v: %v", h, err)
			continue
		}
		if info.Name != h.Name {
			t.Errorf("Stat %v name = %q, want %q", h, info.Name, h.Name)
		}
		if info.Size != int64(len(want)) {
			t.Errorf("Stat %v size = %d, want %d", h, info.Size, len(want))
		}
	}
}

// testNamespaces checks that each handle type owns its own namespace: the same
// name under two types is two objects, and List never crosses the boundary.
func testNamespaces(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	b := newBackend(t)

	// One shared name, so a backend that ignores Handle.Type collides on it.
	const shared = "0011223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	for _, typ := range conformanceTypes {
		put(t, b, ark.Handle{Type: typ, Name: shared}, []byte(typ.String()))
	}

	for _, typ := range conformanceTypes {
		h := ark.Handle{Type: typ, Name: shared}
		if got := get(t, b, h); string(got) != typ.String() {
			t.Errorf("Get %v = %q, want %q", h, got, typ.String())
		}

		names := slices.Sorted(maps.Keys(list(t, b, typ)))
		if len(names) != 1 || names[0] != shared {
			t.Errorf("List %v = %v, want [%s]", typ, names, shared)
		}
	}

	// Deleting from one namespace leaves the others alone.
	if err := b.Delete(context.Background(), ark.Handle{Type: ark.BlobFile, Name: shared}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, typ := range conformanceTypes {
		want := typ != ark.BlobFile
		ok, err := b.Exists(context.Background(), ark.Handle{Type: typ, Name: shared})
		if err != nil {
			t.Errorf("Exists %v: %v", typ, err)
		} else if ok != want {
			t.Errorf("after deleting the chunk, Exists %v = %v, want %v", typ, ok, want)
		}
	}
}

// testPutIsIdempotent checks that a second Put replaces the object instead of
// adding one or failing.
func testPutIsIdempotent(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	b := newBackend(t)
	h := ark.Handle{Type: ark.SnapshotFile, Name: objectName(ark.SnapshotFile, "idem")}

	payloads := [][]byte{
		[]byte("first"),
		[]byte("second, longer than the first"),
		[]byte("third"), // shorter again, so a truncating backend is caught
	}
	for _, want := range payloads {
		put(t, b, h, want)
		if got := get(t, b, h); !bytes.Equal(got, want) {
			t.Fatalf("Get after Put %q = %q", want, got)
		}
		info, err := b.Stat(context.Background(), h)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(want)) {
			t.Errorf("Stat size = %d, want %d", info.Size, len(want))
		}
	}

	if got := list(t, b, ark.SnapshotFile); len(got) != 1 {
		t.Errorf("List after %d puts of one handle = %v, want one object", len(payloads), got)
	}
}

// testPutIsAtomic checks the contract clause that a reader never observes a
// partial object: a Put whose source fails mid-stream must report the error and
// leave the previous object in place.
func testPutIsAtomic(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)
	h := ark.Handle{Type: ark.ConfigFile, Name: objectName(ark.ConfigFile, "atomic")}

	want := []byte("the committed config")
	put(t, b, h, want)

	// Enough bytes before the error that a buffering backend has flushed some.
	boom := errors.New("arktest: source failed")
	src := io.MultiReader(strings.NewReader(strings.Repeat("x", 1<<16)), errReader{boom})
	if err := b.Put(ctx, h, src); err == nil {
		t.Fatal("Put with a failing source = nil, want an error")
	}

	if got := get(t, b, h); !bytes.Equal(got, want) {
		t.Errorf("after a failed Put, Get = %q (len %d), want %q", truncate(got), len(got), want)
	}

	// The failed attempt must not surface as an object of its own.
	if got := list(t, b, ark.ConfigFile); len(got) != 1 {
		t.Errorf("List after a failed Put = %v, want only %q", got, h.Name)
	}
}

// errReader fails every read with err.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// truncate shortens a payload for an error message.
func truncate(b []byte) string {
	if len(b) > 64 {
		return string(b[:64]) + "..."
	}
	return string(b)
}

// testPutEmptyObject checks that a zero-length object round trips, since an
// empty config or tree is legal.
func testPutEmptyObject(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	b := newBackend(t)
	h := ark.Handle{Type: ark.TreeFile, Name: objectName(ark.TreeFile, "empty")}
	put(t, b, h, nil)

	if got := get(t, b, h); len(got) != 0 {
		t.Errorf("Get = %q, want an empty object", got)
	}
	info, err := b.Stat(context.Background(), h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 0 {
		t.Errorf("Stat size = %d, want 0", info.Size)
	}
	if got := list(t, b, ark.TreeFile); got[h.Name] != 0 || len(got) != 1 {
		t.Errorf("List = %v, want {%q: 0}", got, h.Name)
	}
}

// testMissingObject pins the behaviour of every method on an absent object.
func testMissingObject(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)
	h := ark.Handle{Type: ark.TreeFile, Name: objectName(ark.TreeFile, "absent")}

	if _, err := b.Get(ctx, h); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Get of an absent object = %v, want ark.ErrNotFound", err)
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Stat of an absent object = %v, want ark.ErrNotFound", err)
	}
	if ok, err := b.Exists(ctx, h); err != nil {
		t.Errorf("Exists of an absent object: %v", err)
	} else if ok {
		t.Error("Exists of an absent object = true, want false")
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Errorf("Delete of an absent object = %v, want nil", err)
	}

	// An empty namespace lists as empty, it is not an error.
	for _, typ := range conformanceTypes {
		if got := list(t, b, typ); len(got) != 0 {
			t.Errorf("List %v on an empty backend = %v, want nothing", typ, got)
		}
	}
}

// testDelete checks removal and its idempotence.
func testDelete(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)
	h := ark.Handle{Type: ark.LockFile, Name: objectName(ark.LockFile, "held")}
	other := ark.Handle{Type: ark.LockFile, Name: objectName(ark.LockFile, "kept")}

	put(t, b, h, []byte("held"))
	put(t, b, other, []byte("kept"))

	for range 2 { // deleting twice is not an error
		if err := b.Delete(ctx, h); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	ok, err := b.Exists(ctx, h)
	if err != nil {
		t.Errorf("Exists: %v", err)
	} else if ok {
		t.Error("Exists after Delete = true, want false")
	}
	if got := list(t, b, ark.LockFile); len(got) != 1 || got[other.Name] != int64(len("kept")) {
		t.Errorf("List after Delete = %v, want only %q", got, other.Name)
	}

	// The name is free again.
	put(t, b, h, []byte("held again"))
	if got := get(t, b, h); string(got) != "held again" {
		t.Errorf("Get after re-Put = %q", got)
	}
}

// testListSizes checks that List reports every object of a type exactly once
// with its stored size.
func testListSizes(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	b := newBackend(t)

	want := make(map[string]int64)
	for i := range 12 {
		h := ark.Handle{Type: ark.BlobFile, Name: objectName(ark.BlobFile, fmt.Sprintf("%02d", i))}
		payload := bytes.Repeat([]byte{byte('a' + i)}, i*97)
		put(t, b, h, payload)
		want[h.Name] = int64(len(payload))
	}

	got := list(t, b, ark.BlobFile)
	if len(got) != len(want) {
		t.Fatalf("List returned %d objects, want %d", len(got), len(want))
	}
	for name, size := range want {
		gotSize, ok := got[name]
		if !ok {
			t.Errorf("List did not yield %q", name)
			continue
		}
		if gotSize != size {
			t.Errorf("List size of %q = %d, want %d", name, gotSize, size)
		}
	}
}

// testListStopsOnError checks that List returns the callback's error and stops
// calling it.
func testListStopsOnError(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	b := newBackend(t)
	for i := range 8 {
		h := ark.Handle{Type: ark.TreeFile, Name: objectName(ark.TreeFile, fmt.Sprintf("%02d", i))}
		put(t, b, h, []byte("node"))
	}

	stop := errors.New("arktest: stop listing")
	calls := 0
	err := b.List(context.Background(), ark.TreeFile, func(ark.BlobInfo) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("List = %v, want the callback's error", err)
	}
	if calls != 1 {
		t.Errorf("List called fn %d times after it returned an error, want 1", calls)
	}
}

// testConcurrentPuts writes distinct objects from many goroutines. Run under
// -race, it is the check that the backend is safe for concurrent use.
func testConcurrentPuts(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)
	const workers = 16

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			h := ark.Handle{Type: ark.BlobFile, Name: objectName(ark.BlobFile, fmt.Sprintf("%02d", i))}
			if err := b.Put(ctx, h, bytes.NewReader(bytes.Repeat([]byte("x"), i+1))); err != nil {
				t.Errorf("Put %v: %v", h, err)
			}
		})
	}
	wg.Wait()

	got := list(t, b, ark.BlobFile)
	if len(got) != workers {
		t.Fatalf("List after %d concurrent puts returned %d objects", workers, len(got))
	}
	for i := range workers {
		name := objectName(ark.BlobFile, fmt.Sprintf("%02d", i))
		if got[name] != int64(i+1) {
			t.Errorf("size of %q = %d, want %d", name, got[name], i+1)
		}
	}
}

// testConcurrentMixed hammers one handle with writers while readers and
// listers run against the same namespace. A reader either misses the object or
// sees one whole payload, never a mix of the two.
func testConcurrentMixed(t *testing.T, newBackend func(*testing.T) ark.Backend) {
	ctx := context.Background()
	b := newBackend(t)
	h := ark.Handle{Type: ark.ConfigFile, Name: objectName(ark.ConfigFile, "hot")}

	small := bytes.Repeat([]byte("a"), 4096)
	large := bytes.Repeat([]byte("z"), 8192)

	var wg sync.WaitGroup
	for i := range 8 {
		payload := small
		if i%2 == 1 {
			payload = large
		}
		wg.Go(func() {
			if err := b.Put(ctx, h, bytes.NewReader(payload)); err != nil {
				t.Errorf("Put: %v", err)
			}
		})
		wg.Go(func() {
			rc, err := b.Get(ctx, h)
			if errors.Is(err, ark.ErrNotFound) {
				return // not written yet
			}
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if !bytes.Equal(data, small) && !bytes.Equal(data, large) {
				t.Errorf("Get returned a torn object of len %d", len(data))
			}
		})
		wg.Go(func() {
			if err := b.List(ctx, ark.ConfigFile, func(ark.BlobInfo) error { return nil }); err != nil {
				t.Errorf("List: %v", err)
			}
		})
	}
	wg.Wait()

	if got := get(t, b, h); !bytes.Equal(got, small) && !bytes.Equal(got, large) {
		t.Errorf("final object has len %d, want one of the two payloads", len(got))
	}
}

// testClose checks that Close reports no error on a backend that was used.
// It owns its backend, so the shared cleanup does not close it twice.
func testClose(t *testing.T, factory func(t *testing.T) ark.Backend) {
	b := factory(t)
	put(t, b, ark.Handle{Type: ark.ConfigFile, Name: objectName(ark.ConfigFile, "closing")}, []byte("x"))
	if err := b.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
