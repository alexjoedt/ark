package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// commonPrefix returns the longest prefix shared by a and b.
func commonPrefix(a, b ark.SnapshotID) ark.SnapshotID {
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
	for _, id := range []ark.SnapshotID{first.ID, second.ID} {
		if !strings.Contains(err.Error(), string(id)) {
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

// A keep policy with an empty filter judges every series on its own: two hosts
// backing up one path, and a file and a docker source sharing a name, each
// keep their newest snapshot.
func TestForgetKeepsTheNewestOfEverySeries(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	series := []struct {
		src  stubSource
		host string
	}{
		{stubSource{name: "/srv/app"}, "a"},
		{stubSource{name: "/srv/app"}, "b"},
		{stubSource{name: "db"}, "a"},
		{stubSource{name: "db", typ: "docker-volume"}, "a"},
		{stubSource{name: "db", typ: "docker-volume"}, ""},
	}
	// Two rounds; the last series of a round is the newest in the repository.
	var newest []ark.SnapshotID
	for round := range 2 {
		newest = newest[:0]
		for _, s := range series {
			s.src.content = strconv.Itoa(round)
			snap, err := repo.Backup(ctx, s.src, ark.WithHost(s.host))
			if err != nil {
				t.Fatalf("Backup %+v: %v", s, err)
			}
			newest = append(newest, snap.ID)
		}
	}

	res, err := repo.Forget(ctx, ark.ForgetPolicy{KeepLast: 1}, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(res.Remove) != len(series) {
		t.Errorf("removed %d snapshots, want the %d older ones", len(res.Remove), len(series))
	}
	left, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	var got []ark.SnapshotID
	for _, s := range left {
		got = append(got, s.ID)
	}
	slices.Sort(got)
	slices.Sort(newest)
	if !slices.Equal(got, newest) {
		t.Errorf("left %v, want the newest of every series %v", got, newest)
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
	result, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{prefix}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(result.Remove) != 1 || result.Remove[0].ID != first.ID {
		t.Fatalf("Forget removed %v, want just %s", result.Remove, first.ID)
	}
	if result.KeepReasons != nil {
		t.Errorf("removing named ids gave keep reasons %v", result.KeepReasons)
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

	now := time.Now()
	for _, age := range []time.Duration{72 * time.Hour, 30 * time.Minute, 0} {
		src := stubSource{name: "app", content: age.String()}
		if _, err := repo.Backup(ctx, src, ark.WithTime(now.Add(-age))); err != nil {
			t.Fatalf("Backup %v ago: %v", age, err)
		}
	}
	// One dated in the future, as a machine with a wrong clock leaves it:
	// the window is still measured from the newest of the others.
	importAt(t, repo, now.AddDate(1, 0, 0))

	for _, tc := range []struct {
		within ark.Within
		keep   int
	}{
		{ark.Within{Hours: 1}, 3},
		{ark.Within{Days: 7}, 4},
	} {
		result, err := repo.Forget(ctx, ark.ForgetPolicy{KeepWithin: tc.within}, ark.SnapshotFilter{},
			ark.WithForgetDryRun())
		if err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if len(result.Keep) != tc.keep || len(result.Keep)+len(result.Remove) != 4 {
			t.Errorf("KeepWithin %v keeps %d and removes %d, want %d of 4",
				tc.within, len(result.Keep), len(result.Remove), tc.keep)
		}
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
	raw, err := ark.OpenObject(ctx, repo, ark.SnapshotFile, string(snap.ID))
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
