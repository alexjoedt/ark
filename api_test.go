package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// commonPrefix returns the longest prefix shared by a and b.
func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

func TestUniquePrefixResolvesEverywhere(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// The random suffix makes the last characters unique on their own.
	prefix := snap.ID[:len(snap.ID)-4]

	got, err := repo.Snapshot(ctx, prefix)
	if err != nil {
		t.Fatalf("Snapshot by prefix: %v", err)
	}
	if got.ID != snap.ID {
		t.Errorf("Snapshot resolved %s to %s, want %s", prefix, got.ID, snap.ID)
	}

	if _, err := repo.Verify(ctx, prefix); err != nil {
		t.Errorf("Verify by prefix: %v", err)
	}
	if err := repo.Export(ctx, prefix, io.Discard); err != nil {
		t.Errorf("Export by prefix: %v", err)
	}
	if err := repo.Walk(ctx, prefix, func(ark.FileInfo) error { return nil }); err != nil {
		t.Errorf("Walk by prefix: %v", err)
	}
	if _, err := repo.Restore(ctx, prefix, discardSink{}); err != nil {
		t.Errorf("Restore by prefix: %v", err)
	}
}

func TestAmbiguousPrefixIsRejected(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	first, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	second, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}

	shared := commonPrefix(first.ID, second.ID)
	if shared == "" {
		t.Skip("the two ids share no prefix")
	}

	_, err = repo.Snapshot(ctx, shared)
	if !errors.Is(err, ark.ErrAmbiguousID) {
		t.Fatalf("Snapshot(%q) returned %v, want ErrAmbiguousID", shared, err)
	}
	// The message has to name the candidates, otherwise the user cannot pick.
	for _, id := range []string{first.ID, second.ID} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error does not name candidate %s: %v", id, err)
		}
	}
}

func TestUnknownIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	if _, err := repo.Snapshot(ctx, "nosuchsnapshot"); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Snapshot of an unknown id returned %v, want ErrNotFound", err)
	}
	if _, err := repo.Snapshot(ctx, ""); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Snapshot of an empty id returned %v, want ErrNotFound", err)
	}
}

func TestForgetRefusesAZeroPolicyWithoutIDs(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{})
	if !errors.Is(err, ark.ErrEmptyPolicy) {
		t.Fatalf("Forget with a zero policy returned %v, want ErrEmptyPolicy", err)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("the refused Forget removed snapshots: %d left, want 1", len(snaps))
	}
}

func TestForgetByIDsRemovesExactlyThose(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	first, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	second, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}

	// A unique prefix names the snapshot just as well as the full id.
	prefix := first.ID[:len(first.ID)-4]
	result, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{prefix}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(result.Remove) != 1 || result.Remove[0].ID != first.ID {
		t.Fatalf("Forget removed %v, want just %s", result.Remove, first.ID)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].ID != second.ID {
		t.Errorf("after Forget the repository holds %v, want just %s", snaps, second.ID)
	}
}

func TestKeepWithinKeepsRecentSnapshots(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	for _, content := range []string{"one", "two", "three"} {
		if _, err := repo.Backup(ctx, stubSource{name: "app", content: content}); err != nil {
			t.Fatalf("Backup %s: %v", content, err)
		}
	}

	// Everything was written seconds ago, so an hour keeps all three.
	result, err := repo.Forget(ctx, ark.ForgetPolicy{KeepWithin: time.Hour}, ark.SnapshotFilter{},
		ark.WithForgetDryRun())
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(result.Keep) != 3 || len(result.Remove) != 0 {
		t.Errorf("KeepWithin=1h keeps %d and removes %d, want 3 and 0",
			len(result.Keep), len(result.Remove))
	}

	// A window of one nanosecond keeps only the newest.
	result, err = repo.Forget(ctx, ark.ForgetPolicy{KeepWithin: time.Nanosecond}, ark.SnapshotFilter{},
		ark.WithForgetDryRun())
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(result.Keep) != 1 {
		t.Errorf("KeepWithin=1ns keeps %d snapshots, want 1", len(result.Keep))
	}
}

func TestWithParentOverridesTheDerivedParent(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	first, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	second, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	if second.ParentID != first.ID {
		t.Fatalf("the derived parent is %s, want %s", second.ParentID, first.ID)
	}

	// A prefix works here too.
	third, err := repo.Backup(ctx, stubSource{name: "app", content: "three"},
		ark.WithParent(first.ID[:len(first.ID)-4]))
	if err != nil {
		t.Fatalf("third Backup: %v", err)
	}
	if third.ParentID != first.ID {
		t.Errorf("WithParent set %s, want %s", third.ParentID, first.ID)
	}

	if _, err := repo.Backup(ctx, stubSource{name: "app"}, ark.WithParent("nosuch")); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("WithParent of an unknown id returned %v, want ErrNotFound", err)
	}
}

func TestManifestKeepsDurationAsAString(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var m struct {
		Stats map[string]json.RawMessage `json:"stats"`
	}
	raw, err := ark.OpenObject(ctx, repo, ark.SnapshotFile, snap.ID)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	duration, ok := m.Stats["duration"]
	if !ok {
		t.Fatal("manifest stats carry no duration")
	}
	if !bytes.HasPrefix(duration, []byte(`"`)) {
		t.Errorf("duration is %s, want a JSON string", duration)
	}

	// The public type exposes it as a time.Duration, parsed back from that string.
	if snap.Stats.Duration <= 0 {
		t.Errorf("Snapshot.Stats.Duration is %v, want a positive duration", snap.Stats.Duration)
	}
	reopened, err := repo.Snapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if reopened.Stats.Duration <= 0 {
		t.Errorf("the reloaded duration is %v, want a positive duration", reopened.Stats.Duration)
	}
}

func TestWithConcurrencyIsHonoured(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}

	repo, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: strings.Repeat("payload ", 4096)})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	result, err := repo.Restore(ctx, snap.ID, discardSink{})
	if err != nil {
		t.Fatalf("Restore with one worker: %v", err)
	}
	if result.FilesRestored != 1 {
		t.Errorf("restored %d files, want 1", result.FilesRestored)
	}

	// A non-positive value falls back to the default rather than stalling.
	quiet, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(0))
	if err != nil {
		t.Fatalf("Open with zero concurrency: %v", err)
	}
	defer quiet.Close()
	if _, err := quiet.Restore(ctx, snap.ID, discardSink{}); err != nil {
		t.Errorf("Restore with the default concurrency: %v", err)
	}
}
