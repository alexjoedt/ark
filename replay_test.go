package ark_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// The replay tests pin what a backend attacker gets by putting an old, valid
// object back under its old name. Chunks and trees are content-addressed, so
// a replayed one is the object itself; these cover the named namespaces.
// There is no freshness anchor: a rollback of the repository state is
// accepted, the guarantee is that a replay never restores different bytes.
// Locks and the config are in replay_internal_test.go.

func putObject(t *testing.T, b ark.Backend, typ ark.HandleType, name string, data []byte) {
	t.Helper()
	if err := b.Put(context.Background(), ark.Handle{Type: typ, Name: name}, bytes.NewReader(data)); err != nil {
		t.Fatalf("put %s/%s: %v", typ, name, err)
	}
}

func restoreData(t *testing.T, repo *ark.Repository, id string) (string, error) {
	t.Helper()
	sink := &recordingSink{}
	_, err := repo.Restore(context.Background(), id, sink)
	return sink.written["data.txt"], err
}

// TestReplayedManifestRestoresOnlyItsOwnBytes replays a forgotten manifest.
// Before prune its trees and chunks survive and it restores what it held.
// After prune it fails Restore, Verify and Check, also once the source has
// newer contents, and restores its own bytes again once they are backed up
// anew: names are keyed hashes of the plaintext, so no other bytes fit them.
func TestReplayedManifestRestoresOnlyItsOwnBytes(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*arkmem.Backend, *ark.Repository, string, []byte) {
		t.Helper()
		b := arkmem.New()
		repo := openRepo(t, b)
		snap, err := repo.Backup(ctx, stubSource{name: "app", content: victimContent})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		manifest := readObject(t, b, ark.SnapshotFile, snap.ID)
		if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID}}); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		return b, repo, snap.ID, manifest
	}

	t.Run("before prune", func(t *testing.T) {
		b, repo, id, manifest := setup(t)
		putObject(t, b, ark.SnapshotFile, id, manifest)

		snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
		if err != nil || len(snaps) != 1 || snaps[0].ID != id {
			t.Fatalf("Snapshots = %v, %v; want the replayed %s", snaps, err, id)
		}
		if got, err := restoreData(t, repo, id); err != nil || got != victimContent {
			t.Fatalf("Restore = %q, %v; want %q", got, err, victimContent)
		}
		if res, err := repo.Verify(ctx, id); err != nil || !res.OK {
			t.Errorf("Verify = %+v, %v; want OK", res, err)
		}
	})

	t.Run("after prune", func(t *testing.T) {
		b, repo, id, manifest := setup(t)
		if _, err := repo.Prune(ctx); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		putObject(t, b, ark.SnapshotFile, id, manifest)

		for _, typ := range []ark.HandleType{ark.TreeFile, ark.BlobFile} {
			if n := len(objectNames(t, b, typ)); n != 0 {
				t.Fatalf("prune left %d %s objects", n, typ)
			}
		}

		// Prune took the trees with the chunks, so every consumer stops at
		// the missing root tree.
		assertGone := func(t *testing.T) {
			t.Helper()
			if got, err := restoreData(t, repo, id); !errors.Is(err, ark.ErrNotFound) || got != "" {
				t.Errorf("Restore = %q, %v; want ErrNotFound and no bytes", got, err)
			}
			if res, err := repo.Verify(ctx, id); !errors.Is(err, ark.ErrNotFound) || (res != nil && res.OK) {
				t.Errorf("Verify = %+v, %v; want ErrNotFound and OK false", res, err)
			}
			check, err := repo.Check(ctx)
			if err != nil || len(check.Errors) != 1 || !strings.Contains(check.Errors[0], id) {
				t.Errorf("Check = %+v, %v; want the replayed snapshot under Errors", check, err)
			}
		}
		assertGone(t)

		// Newer contents under the same source name never fill the gap.
		if _, err := repo.Backup(ctx, stubSource{name: "app", content: otherContent}); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		assertGone(t)

		// The same contents recreate the same chunks and trees.
		if _, err := repo.Backup(ctx, stubSource{name: "app", content: victimContent}); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if got, err := restoreData(t, repo, id); err != nil || got != victimContent {
			t.Fatalf("Restore = %q, %v; want %q", got, err, victimContent)
		}
	})
}

// TestReplayedKeyFileOpensUnderTheOldPassword pins that ChangePassword does
// not revoke the old passphrase against a backend attacker who kept the old
// key file: put back, it opens the repository again, next to the new one.
func TestReplayedKeyFileOpensUnderTheOldPassword(t *testing.T) {
	ctx := context.Background()
	oldPW, newPW := ark.WithPassword("old"), ark.WithPassword("new")
	b := initRepo(t, oldPW)
	oldName, oldKey := onlyKeyFile(t, b)

	repo, err := ark.Open(ctx, b, oldPW)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap, err := repo.Backup(ctx, stubSource{name: "app", content: victimContent})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := repo.ChangePassword(ctx, newPW); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := ark.Open(ctx, b, oldPW); !errors.Is(err, ark.ErrWrongPassword) {
		t.Fatalf("Open with the old password before the replay = %v, want ErrWrongPassword", err)
	}

	putObject(t, b, ark.KeyFile, oldName, oldKey)
	for _, pw := range []ark.Passphrase{oldPW, newPW} {
		got, err := ark.Open(ctx, b, pw)
		if err != nil {
			t.Fatalf("Open after the replay: %v", err)
		}
		// No Close: it closes the shared backend.
		data, err := restoreData(t, got, snap.ID)
		if err != nil || data != victimContent {
			t.Errorf("Restore = %q, %v; want %q", data, err, victimContent)
		}
	}
}

// markerBackend keeps a copy of every marker written and counts chunk Gets:
// a backup reads chunks it reuses only in repair mode.
type markerBackend struct {
	ark.Backend

	mu        sync.Mutex
	markers   map[string][]byte
	chunkGets int
}

func (b *markerBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if h.Type != ark.MarkerFile {
		return b.Backend.Put(ctx, h, r)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.markers[h.Name] = data
	b.mu.Unlock()
	return b.Backend.Put(ctx, h, bytes.NewReader(data))
}

func (b *markerBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type == ark.BlobFile {
		b.mu.Lock()
		b.chunkGets++
		b.mu.Unlock()
	}
	return b.Backend.Get(ctx, h)
}

func (b *markerBackend) takeMarkers() map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.markers
	b.markers = map[string][]byte{}
	return m
}

func (b *markerBackend) takeChunkGets() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.chunkGets
	b.chunkGets = 0
	return n
}

// TestReplayedMarkerForcesRepairMode replays the marker of a finished
// backup. It costs the next backup the reads of every chunk it reuses and
// shows in Check as a warning, until Prune removes it.
func TestReplayedMarkerForcesRepairMode(t *testing.T) {
	ctx := context.Background()
	b := &markerBackend{Backend: arkmem.New(), markers: map[string][]byte{}}
	repo := openRepo(t, b)
	src := stubSource{name: "app", content: victimContent}
	if _, err := repo.Backup(ctx, src); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	replayed := b.takeMarkers()
	if len(replayed) != 1 || countMarkers(t, b) != 0 {
		t.Fatalf("backup wrote %d markers and left %d, want 1 and 0", len(replayed), countMarkers(t, b))
	}
	if _, err := repo.Backup(ctx, src); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if n := b.takeChunkGets(); n != 0 {
		t.Fatalf("backup without a marker read %d chunks, want 0", n)
	}

	for name, data := range replayed {
		putObject(t, b.Backend, ark.MarkerFile, name, data)
	}
	if res, err := repo.Check(ctx); err != nil || len(res.Errors) != 0 || len(res.Warnings) != 1 {
		t.Fatalf("Check = %+v, %v; want one warning", res, err)
	}
	b.takeChunkGets() // Check verifies every chunk
	if _, err := repo.Backup(ctx, src); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if n := b.takeChunkGets(); n == 0 {
		t.Fatal("backup beside the replayed marker read no chunk, want repair mode")
	}

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n := countMarkers(t, b); n != 0 {
		t.Fatalf("Prune left %d markers", n)
	}
	if _, err := repo.Backup(ctx, src); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if n := b.takeChunkGets(); n != 0 {
		t.Errorf("backup after Prune read %d chunks, want 0", n)
	}
}
