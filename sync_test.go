package ark_test

import (
	"context"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// syncRecordingBackend notes, for every Sync, how many objects of each type
// the repository held at that moment, which pins the barrier positions.
type syncRecordingBackend struct {
	ark.Backend

	mu    sync.Mutex
	syncs []map[ark.HandleType]int
}

func (b *syncRecordingBackend) Sync(ctx context.Context) error {
	counts := map[ark.HandleType]int{}
	types := []ark.HandleType{ark.KeyFile, ark.ConfigFile, ark.SnapshotFile, ark.BlobFile, ark.TreeFile, ark.MarkerFile}
	for _, t := range types {
		err := b.Backend.List(ctx, t, func(ark.BlobInfo) error { counts[t]++; return nil })
		if err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.syncs = append(b.syncs, counts)
	b.mu.Unlock()
	return b.Backend.Sync(ctx)
}

// take returns the Syncs since the last call and clears them.
func (b *syncRecordingBackend) take() []map[ark.HandleType]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.syncs
	b.syncs = nil
	return s
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
	if len(s) != 3 || s[0][ark.MarkerFile] != 1 || s[0][ark.BlobFile] != 0 ||
		s[1][ark.SnapshotFile] != 0 || s[1][ark.BlobFile] == 0 || s[2][ark.SnapshotFile] != 1 {
		t.Fatalf("Backup synced %v, want once after the marker before any chunk, "+
			"once with the chunks before the manifest and once after", s)
	}
	if n := countMarkers(t, b); n != 0 {
		t.Fatalf("Backup left %d markers, want none", n)
	}

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

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.SnapshotFile] != 0 || s[0][ark.BlobFile] == 0 {
		t.Fatalf("Forget synced %v, want once after the manifest went and before any sweep", s)
	}

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.BlobFile] != 0 || s[0][ark.TreeFile] != 0 {
		t.Fatalf("Prune synced %v, want once after the sweep", s)
	}

	if err := repo.ChangePassword(ctx, ark.WithPassword("another one")); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if s := b.take(); len(s) != 1 || s[0][ark.KeyFile] != 2 {
		t.Fatalf("ChangePassword synced %v, want once while both key files exist", s)
	}
}
