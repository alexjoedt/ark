package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// heldReadsBackend holds every range read until the test lets it go, and notes
// what each one asked for.
type heldReadsBackend struct {
	Backend
	entered chan struct{} // receives once per range read that arrived
	release chan struct{} // a send lets one range read go

	mu    sync.Mutex
	asked [][2]int64 // offset and length
}

func newHeldReadsBackend() *heldReadsBackend {
	return &heldReadsBackend{Backend: newStubBackend(), entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *heldReadsBackend) GetRange(ctx context.Context, h Handle, offset, length int64) ([]byte, error) {
	b.mu.Lock()
	b.asked = append(b.asked, [2]int64{offset, length})
	b.mu.Unlock()
	for _, gate := range []chan struct{}{b.entered, b.release} {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.Backend.GetRange(ctx, h, offset, length)
}

// arrive waits for count range reads to be held at the gate together.
func (b *heldReadsBackend) arrive(t *testing.T, count int) {
	t.Helper()
	for i := range count {
		select {
		case <-b.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d range reads arrived at the backend, want %d at once", i, count)
		}
	}
}

func (b *heldReadsBackend) let(count int) {
	for range count {
		<-b.release
	}
}

// storedChunks stores one chunk per size in a single pack on b and returns
// the store that reads them, their refs and their content.
func storedChunks(t *testing.T, b Backend, sizes ...int) (*objectStore, []chunkRef, [][]byte) {
	t.Helper()
	ctx := context.Background()
	codec := newObjectCodec()
	t.Cleanup(codec.Close)
	session := testSession(b, codec, newMasterKey("repo"))
	store := session.chunks

	var refs []chunkRef
	var content [][]byte
	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)
		name := store.name(data)
		if _, err := store.putNamed(ctx, name, data); err != nil {
			t.Fatalf("putNamed: %v", err)
		}
		refs = append(refs, chunkRef{ID: name, Length: int64(size)})
		content = append(content, data)
	}
	if err := session.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return store, refs, content
}

// settled waits until the fetcher holds pending chunks in no read yet.
func settled(t *testing.T, f *chunkFetcher, pending int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		n := 0
		for _, wants := range f.pending {
			n += len(wants)
		}
		f.mu.Unlock()
		if n == pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d chunks wait for a read, want %d", n, pending)
		}
	}
}

// TestFetcherReadsRangesThatLieApartSideBySide wants chunks of one pack that
// lie too far apart for one range read: the ranges must not queue behind each
// other while workers are free.
func TestFetcherReadsRangesThatLieApartSideBySide(t *testing.T) {
	const size = packRangeGap + 1
	b := newHeldReadsBackend()
	store, refs, content := storedChunks(t, b, size, size, size, size, size)

	f := newFetcher(context.Background(), store, nil, 4, 64<<20)
	defer f.close()
	s := f.reader([]chunkRef{refs[0], refs[2], refs[4]})
	defer s.Close()

	b.arrive(t, 1) // the first chunk wanted starts the pack's first read
	settled(t, f, 2)
	go b.let(1)
	b.arrive(t, 2)
	go b.let(2)

	got, err := io.ReadAll(s)
	if want := slices.Concat(content[0], content[2], content[4]); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, %v, want the %d stored", len(got), err, len(want))
	}
}

// TestClosedStreamLeavesItsChunksOutOfTheNextRead closes a stream whose
// chunks wait for a read: the read asks for none of them, except the one
// another stream waits for too, which that stream still gets.
func TestClosedStreamLeavesItsChunksOutOfTheNextRead(t *testing.T) {
	ctx := context.Background()
	b := newHeldReadsBackend()
	store, refs, content := storedChunks(t, b, 4, 4, 4)

	f := newFetcher(ctx, store, nil, 2, 1<<20)
	defer f.close()
	first := f.reader(refs[:1])
	defer first.Close()
	b.arrive(t, 1)
	closed := f.reader(refs[1:])
	shares := f.reader(refs[2:])
	defer shares.Close()
	settled(t, f, 2)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		waiting := f.flights[refs[2].ID].waiting
		f.mu.Unlock()
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d streams wait for the shared chunk, want 2", waiting)
		}
	}
	_ = closed.Close()

	go b.let(1)
	b.arrive(t, 1)
	go b.let(1)
	for i, s := range []*chunkStream{first, shares} {
		got, err := io.ReadAll(s)
		if want := content[2*i]; err != nil || !bytes.Equal(got, want) {
			t.Fatalf("stream %d read %x, %v, want %x", i, got, err, want)
		}
	}
	_, at, err := store.locate(ctx, refs[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if want := [2]int64{int64(at.offset), int64(at.length)}; len(b.asked) != 2 || b.asked[1] != want {
		t.Errorf("range reads %v, want two and the second %v: only the chunk still waited for", b.asked, want)
	}
}

// TestStreamClosedOutOfOrderKeepsTheTurnChain closes the middle of three
// streams while the first one still waits for budget. The third stream must
// not take the budget ahead of the first, or the restore deadlocks.
func TestStreamClosedOutOfOrderKeepsTheTurnChain(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	session := testSession(newStubBackend(), codec, newMasterKey("repo"))
	store := session.chunks

	var refs []chunkRef
	for i := range 4 {
		data := []byte{byte(i), 1, 2, 3}
		name := store.name(data)
		if _, err := store.putNamed(ctx, name, data); err != nil {
			t.Fatalf("putNamed: %v", err)
		}
		refs = append(refs, chunkRef{ID: name, Length: int64(len(data))})
	}
	if err := session.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	f := newFetcher(ctx, store, nil, 2, 4) // one chunk at a time
	defer f.close()

	a := f.reader(refs[:2])
	b := f.reader(refs[2:3])
	c := f.reader(refs[3:])
	time.Sleep(20 * time.Millisecond) // let a take the budget and block on its second chunk
	_ = b.Close()

	done := make(chan error, 1)
	go func() {
		for _, s := range []*chunkStream{a, c} {
			if _, err := io.ReadAll(s); err != nil {
				done <- err
				return
			}
			_ = s.Close()
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading the remaining streams in order deadlocked")
	}
}
