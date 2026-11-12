package ark

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
)

// orderBackend notes every Put by namespace and every Sync, in order.
type orderBackend struct {
	*stubBackend
	mu  sync.Mutex
	log []string
}

func (b *orderBackend) note(event string) {
	b.mu.Lock()
	b.log = append(b.log, event)
	b.mu.Unlock()
}

func (b *orderBackend) Put(ctx context.Context, h Handle, r io.Reader) error {
	err := b.stubBackend.Put(ctx, h, r)
	b.note("put " + h.Type.String())
	return err
}

func (b *orderBackend) Sync(ctx context.Context) error {
	err := b.stubBackend.Sync(ctx)
	b.note("sync")
	return err
}

func (b *orderBackend) events() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.log)
}

// TestSessionCheckpointsAtTheRecordCap fills packs until the stored ones hold
// indexMaxRecords blobs no index lists: the pack that crosses the cap is
// followed by a Sync and one index object, while the run goes on.
func TestSessionCheckpointsAtTheRecordCap(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	b := &orderBackend{stubBackend: newStubBackend()}
	session := testSession(b, codec, newMasterKey("repo"))
	store := session.chunks
	store.writer.target = 4 << 10 // a few dozen blobs to a pack

	put := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			data := fmt.Appendf(nil, "blob %d", i)
			if _, err := store.putNamed(ctx, store.name(data), data); err != nil {
				t.Fatalf("putNamed: %v", err)
			}
		}
	}
	indexPuts := func() int {
		n := 0
		for _, e := range b.events() {
			if e == "put "+DataIndexFile.String() {
				n++
			}
		}
		return n
	}

	put(0, indexMaxRecords-1)
	if n := indexPuts(); n != 0 {
		t.Fatalf("%d index objects below the cap, want none", n)
	}
	const total = indexMaxRecords + 500
	put(indexMaxRecords-1, total)
	events := b.events()
	at := slices.Index(events, "put "+DataIndexFile.String())
	if indexPuts() != 1 || at < 2 || events[at-1] != "sync" || events[at-2] != "put "+DataPackFile.String() {
		t.Fatalf("%d index objects after crossing the cap, want one right after a Sync that follows a pack",
			indexPuts())
	}
	if slices.Index(events, "sync") != at-1 {
		t.Error("a Sync ran before the checkpoint was due")
	}
	x := newPackIndex(b, store.space, session.chunks.index.logger)
	table, err := x.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := table.count(); n < indexMaxRecords || n >= total {
		t.Errorf("the checkpoint lists %d blobs, want at least %d and not the %d of the run", n, indexMaxRecords, total)
	}

	if err := session.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	table, err = newPackIndex(b, store.space, session.chunks.index.logger).load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := table.count(); n != total || indexPuts() != 2 {
		t.Errorf("after finish %d index objects list %d blobs, want 2 and %d", indexPuts(), n, total)
	}
}
