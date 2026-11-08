package ark

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestStreamClosedOutOfOrderKeepsTheTurnChain closes the middle of three
// streams while the first one still waits for budget. The third stream must
// not take the budget ahead of the first, or the restore deadlocks.
func TestStreamClosedOutOfOrderKeepsTheTurnChain(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	store := newObjectStore(newStubBackend(), BlobFile, codec, newMasterKey("repo"))

	var refs []chunkRef
	for i := range 4 {
		data := []byte{byte(i), 1, 2, 3}
		name, _, err := store.Put(ctx, data)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		refs = append(refs, chunkRef{ID: name, Length: int64(len(data))})
	}

	f := &chunkFetcher{
		store:   store,
		workers: 2,
		sem:     make(chan struct{}, 2),
		budget:  newByteBudget(4), // one chunk at a time
		turn:    make(chan struct{}),
		flights: make(map[string]*flight),
	}
	close(f.turn)

	a := f.reader(ctx, refs[:2])
	b := f.reader(ctx, refs[2:3])
	c := f.reader(ctx, refs[3:])
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
