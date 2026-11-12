package ark_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// The replay tests pin what a backend attacker gets by putting an old, valid
// object back under its old name. Chunks, trees, packs and index objects are
// content-addressed, so a replayed one is the object itself; these cover the
// named namespaces, and the one replay of a content-addressed object that
// changes what the repository claims to hold: an old index object.
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

		for _, typ := range []ark.HandleType{
			ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
		} {
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

// TestReplayedIndexObjectIsPassedOver replays the index objects of a pruned
// backup. They list packs that are gone, and a load of the index passes over
// what is listed in a pack the backend does not hold: a backup of the same
// contents stores them again and its snapshot restores. Check warns about the
// two packs the replayed objects list until a prune drops the entries: a
// stored pack holds what they listed, so nothing is lost.
func TestReplayedIndexObjectIsPassedOver(t *testing.T) {
	ctx := context.Background()
	b := &putCountingBackend{Backend: arkmem.New(), puts: map[ark.HandleType]int{}}
	repo := openRepo(t, b)
	src := stubSource{name: "app", content: victimContent}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	index := map[ark.Handle][]byte{}
	for _, typ := range []ark.HandleType{ark.DataIndexFile, ark.TreeIndexFile} {
		for _, name := range objectNames(t, b, typ) {
			index[ark.Handle{Type: typ, Name: name}] = readObject(t, b, typ, name)
		}
	}
	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(index) != 2 || len(objectNames(t, b, ark.DataPackFile)) != 0 {
		t.Fatalf("%d index objects kept, %d data packs after the prune, want 2 and 0",
			len(index), len(objectNames(t, b, ark.DataPackFile)))
	}
	for h, obj := range index {
		putObject(t, b, h.Type, h.Name, obj)
	}

	b.take()
	again, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := b.take(); got[ark.DataPackFile] != 1 || got[ark.TreePackFile] != 1 {
		t.Fatalf("backup beside the replayed index stored %v, want one data pack and one tree pack", got)
	}
	if got, err := restoreData(t, repo, again.ID); err != nil || got != victimContent {
		t.Errorf("Restore = %q, %v; want %q", got, err, victimContent)
	}
	if res, err := repo.Verify(ctx, again.ID); err != nil || !res.OK {
		t.Errorf("Verify = %+v, %v; want OK", res, err)
	}
	// Check names the two packs the replayed index lists that are gone, and
	// no snapshot: none reads from them.
	check, err := repo.Check(ctx)
	if err != nil || len(check.Errors) != 0 || len(check.Warnings) != 2 {
		t.Fatalf("Check = %+v, %v; want both lost packs under Warnings and nothing else", check, err)
	}
	for _, want := range []string{"datapacks object", "treepacks object"} {
		if !slices.ContainsFunc(check.Warnings, func(e string) bool { return strings.Contains(e, want) }) {
			t.Errorf("Check warnings %q name no %s", check.Warnings, want)
		}
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if check, err := repo.Check(ctx); err != nil || len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check after the prune = %+v, %v; want no findings", check, err)
	}
	if got, err := restoreData(t, repo, again.ID); err != nil || got != victimContent {
		t.Errorf("Restore after the prune = %q, %v; want %q", got, err, victimContent)
	}
}
