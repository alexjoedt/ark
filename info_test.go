package ark_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
)

func TestInfoReportsSnapshotsAndStoredSizes(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var exclusive int
	repo, err := ark.Open(ctx, b, testPassword, ark.WithLockEvent(func(ev ark.LockEvent) {
		if ev.Kind == ark.LockAcquired && ev.Exclusive {
			exclusive++
		}
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// The forgotten snapshot leaves one dead chunk in a pack of live ones,
	// too little for a prune to repack, so it stays unused.
	a := contents("a", 100)
	gone := mustBackup(t, repo, treeOf("gone", dirOf{"a", a}))
	kept := mustBackup(t, repo, treeOf("kept", dirOf{"a", a[:99]}))
	again := mustBackup(t, repo, smallFiles("again", 10))
	mustForget(t, repo, gone)
	exclusive = 0 // the forget's; only Info's own count below

	want := map[ark.HandleType]int64{}
	for _, typ := range []ark.HandleType{
		ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
		ark.SnapshotFile, ark.KeyFile, ark.ConfigFile, ark.LockFile, ark.LostPackFile,
	} {
		if err := b.List(ctx, typ, func(bi ark.BlobInfo) error {
			want[typ] += bi.Size
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	b.take()

	info, err := repo.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Snapshots != 2 || info.TotalSize != kept.Stats.TotalSize+again.Stats.TotalSize {
		t.Errorf("Snapshots %d, TotalSize %d; want 2, %d",
			info.Snapshots, info.TotalSize, kept.Stats.TotalSize+again.Stats.TotalSize)
	}
	wantStored := ark.StoredSizes{
		DataPacks: want[ark.DataPackFile],
		TreePacks: want[ark.TreePackFile],
		Indexes:   want[ark.DataIndexFile] + want[ark.TreeIndexFile],
		Snapshots: want[ark.SnapshotFile],
		Keys:      want[ark.KeyFile],
		Other:     want[ark.ConfigFile] + want[ark.LockFile] + want[ark.LostPackFile],
	}
	if info.Stored != wantStored {
		t.Errorf("Stored = %+v, want %+v", info.Stored, wantStored)
	}
	var total int64
	for _, n := range want {
		total += n
	}
	if info.Stored.Total() != total {
		t.Errorf("Total = %d, want %d", info.Stored.Total(), total)
	}
	if info.Exact || info.BytesUnused != 0 || exclusive != 0 {
		t.Errorf(
			"without WithInfoExact: Exact %v, BytesUnused %d, exclusive locks %d",
			info.Exact,
			info.BytesUnused,
			exclusive,
		)
	}
	for req := range b.take() {
		if !strings.HasPrefix(req, "list ") && req != "get snapshots" {
			t.Errorf("Info made a %q request, want only lists and manifest reads", req)
		}
	}

	dry, err := repo.Prune(ctx, ark.WithPruneDryRun())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	exclusive = 0
	exact, err := repo.Info(ctx, ark.WithInfoExact())
	if err != nil {
		t.Fatalf("Info exact: %v", err)
	}
	if !exact.Exact || exact.BytesUnused == 0 || exact.BytesUnused != dry.BytesUnused ||
		exact.BytesFreed != dry.BytesFreed {
		t.Errorf("exact: Exact %v, BytesUnused %d, BytesFreed %d; want the dry run's %d (non-zero), %d",
			exact.Exact, exact.BytesUnused, exact.BytesFreed, dry.BytesUnused, dry.BytesFreed)
	}
	if exact.Stored != info.Stored {
		t.Errorf("exact Stored = %+v, want the plain call's %+v", exact.Stored, info.Stored)
	}
	if exclusive != 1 {
		t.Errorf("exact Info took %d exclusive locks, want 1", exclusive)
	}
}

// ghostSnapshot lists a snapshot whose manifest is gone, as a forget that
// deletes it between the listing and the read leaves it.
type ghostSnapshot struct{ ark.Backend }

func (b ghostSnapshot) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if t == ark.SnapshotFile {
		if err := fn(ark.BlobInfo{Name: strings.Repeat("0", 64), Size: 1}); err != nil {
			return err
		}
	}
	return b.Backend.List(ctx, t, fn)
}

func TestInfoSkipsManifestDeletedWhileListing(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, ghostSnapshot{b}, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	mustBackup(t, repo, smallFiles("kept", 3))

	info, err := repo.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Snapshots != 1 {
		t.Errorf("Snapshots = %d, want 1", info.Snapshots)
	}
}
