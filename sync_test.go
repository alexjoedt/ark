package ark_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// syncRecordingBackend notes, for every Sync, how many objects of each type
// the repository held at that moment, which pins the barrier positions, and
// every Put, Delete and Sync in the order they returned.
type syncRecordingBackend struct {
	ark.Backend

	mu     sync.Mutex
	syncs  []map[ark.HandleType]int
	events []backendEvent
}

// backendEvent is one Put or Delete of h, or a Sync.
type backendEvent struct {
	op string
	h  ark.Handle
}

func (b *syncRecordingBackend) record(op string, h ark.Handle) {
	b.mu.Lock()
	b.events = append(b.events, backendEvent{op, h})
	b.mu.Unlock()
}

func (b *syncRecordingBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	err := b.Backend.Put(ctx, h, r)
	if err == nil {
		b.record("put", h)
	}
	return err
}

func (b *syncRecordingBackend) Delete(ctx context.Context, h ark.Handle) error {
	err := b.Backend.Delete(ctx, h)
	if err == nil {
		b.record("delete", h)
	}
	return err
}

func (b *syncRecordingBackend) Sync(ctx context.Context) error {
	counts := map[ark.HandleType]int{}
	types := []ark.HandleType{
		ark.KeyFile, ark.ConfigFile, ark.SnapshotFile,
		ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
	}
	for _, t := range types {
		err := b.Backend.List(ctx, t, func(ark.BlobInfo) error { counts[t]++; return nil })
		if err != nil {
			return err
		}
	}
	if err := b.Backend.Sync(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	b.syncs = append(b.syncs, counts)
	b.events = append(b.events, backendEvent{op: "sync"})
	b.mu.Unlock()
	return nil
}

// take returns the Syncs since the last call and clears them.
func (b *syncRecordingBackend) take() []map[ark.HandleType]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.syncs
	b.syncs = nil
	return s
}

// takeEvents returns the Puts, Deletes and Syncs since the last call.
func (b *syncRecordingBackend) takeEvents() []backendEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.events
	b.events = nil
	return e
}

func packs(c map[ark.HandleType]int) int { return c[ark.DataPackFile] + c[ark.TreePackFile] }
func index(c map[ark.HandleType]int) int { return c[ark.DataIndexFile] + c[ark.TreeIndexFile] }

// assertWriteOrder checks the order a backup or import makes its objects
// present in: packs, Sync, the index objects that list them, Sync, the
// manifest, Sync. It fails for an index object stored before a Sync that
// followed the Put of every pack it lists, for a manifest stored while a pack
// or index object was not synced, and for a run that ends without a Sync
// after its manifest. It returns the number of index objects and manifests.
func assertWriteOrder(t *testing.T, repo *ark.Repository, events []backendEvent) (indexes, manifests int) {
	t.Helper()
	synced := map[string]bool{}
	var unsynced []ark.Handle
	for _, e := range events {
		switch {
		case e.op == "sync":
			for _, h := range unsynced {
				synced[h.Name] = true
			}
			unsynced = nil
			continue
		case e.op != "put":
			continue
		}
		switch e.h.Type { //nolint:exhaustive // only what a run writes in order
		case ark.DataIndexFile, ark.TreeIndexFile:
			indexes++
			listed, _, err := ark.IndexedPacks(context.Background(), repo, e.h)
			if err != nil || len(listed) == 0 {
				t.Fatalf("packs of %s/%s: %v, %v", e.h.Type, e.h.Name, listed, err)
			}
			for _, pack := range listed {
				if !synced[pack] {
					t.Errorf("%s/%s was stored before a Sync covered its pack %s", e.h.Type, e.h.Name, pack)
				}
			}
		case ark.SnapshotFile:
			manifests++
			if len(unsynced) != 0 {
				t.Errorf("manifest %s was stored before a Sync covered %v", e.h.Name, unsynced)
			}
		case ark.DataPackFile, ark.TreePackFile:
		default:
			continue
		}
		unsynced = append(unsynced, e.h)
	}
	if len(unsynced) != 0 {
		t.Errorf("the run ended without a Sync after %v", unsynced)
	}
	return indexes, manifests
}

func TestDurabilityBarriers(t *testing.T) {
	ctx := context.Background()
	b := &syncRecordingBackend{Backend: arkmem.New()}

	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.KeyFile] != 1 || s[0][ark.ConfigFile] != 1 {
		t.Fatalf("Init synced %v, want once after key and config", s)
	}

	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()
	if s := b.take(); len(s) != 0 {
		t.Fatalf("Open synced %v, want never", s)
	}

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	s := b.take()
	if len(s) != 3 || packs(s[0]) != 2 || index(s[0]) != 0 ||
		index(s[1]) != 2 || s[1][ark.SnapshotFile] != 0 || s[2][ark.SnapshotFile] != 1 {
		t.Fatalf("Backup synced %v, want once with the packs before any index object, "+
			"once with the index before the manifest and once after", s)
	}
	if indexes, manifests := assertWriteOrder(t, repo, b.takeEvents()); indexes != 2 || manifests != 1 {
		t.Fatalf("Backup stored %d index objects and %d manifests, want 2 and 1", indexes, manifests)
	}

	// An unchanged backup stores no pack, so only the manifest needs barriers.
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if s := b.take(); len(s) != 2 || s[0][ark.SnapshotFile] != 1 || s[1][ark.SnapshotFile] != 2 {
		t.Fatalf("unchanged Backup synced %v, want once before the manifest and once after", s)
	}
	b.takeEvents()

	if _, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if s := b.take(); len(s) != 0 {
		t.Fatalf("Snapshots synced %v, want never (locks are not synced)", s)
	}

	if _, err := repo.Prune(ctx, ark.WithPruneDryRun()); err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if s := b.take(); len(s) != 0 {
		t.Fatalf("Prune dry run synced %v, want never", s)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil || len(snaps) != 2 {
		t.Fatalf("Snapshots = %v, %v", snaps, err)
	}
	b.take()
	_, err = repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID, snaps[0].ID}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.SnapshotFile] != 0 || packs(s[0]) != 2 {
		t.Fatalf("Forget synced %v, want once after the manifests went and before any sweep", s)
	}

	// Nothing is referenced any more: the index goes first and is durably
	// gone before the first pack is deleted.
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if s := b.take(); len(s) != 3 || index(s[0]) != 2 || index(s[1]) != 0 || packs(s[1]) != 2 || packs(s[2]) != 0 {
		t.Fatalf("Prune synced %v, want once for the new index, once after the old index objects went "+
			"with every pack still stored, and once after the packs went", s)
	}

	// A prune that finds nothing to delete still ends with its barrier.
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if s := b.take(); len(s) != 1 {
		t.Fatalf("idle Prune synced %v, want once", s)
	}

	if err := repo.ChangePassword(ctx, ark.WithPassword("another one")); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.KeyFile] != 2 {
		t.Fatalf("ChangePassword synced %v, want once while both key files exist", s)
	}
}
