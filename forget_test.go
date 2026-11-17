package ark_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// importAt stores an empty snapshot of series "app" created at the given time.
func importAt(t *testing.T, repo *ark.Repository, at time.Time) *ark.Snapshot {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"version": 3, "created_at": at, "source_name": "app", "source_type": ark.SourceTypeFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "metadata.json", Mode: 0o644, Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(meta); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := repo.Import(context.Background(), &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return snap
}

// Half an hour either side of midnight in Berlin is two days there and one
// day in UTC, so the location given to Forget decides what KeepDaily keeps.
func TestForgetCountsPeriodsInTheGivenLocation(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	importAt(t, repo, time.Date(2026, 1, 14, 23, 30, 0, 0, berlin))
	importAt(t, repo, time.Date(2026, 1, 15, 0, 30, 0, 0, berlin))

	tests := []struct {
		name string
		loc  *time.Location
		keep int
	}{
		{"Berlin", berlin, 2},
		{"UTC", time.UTC, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := repo.Forget(ctx, ark.ForgetPolicy{KeepDaily: 2}, ark.SnapshotFilter{},
				ark.WithForgetDryRun(), ark.WithForgetLocation(tc.loc))
			if err != nil {
				t.Fatalf("Forget: %v", err)
			}
			if len(res.Keep) != tc.keep || len(res.Keep)+len(res.Remove) != 2 {
				t.Errorf("keeps %d and removes %d, want %d kept of 2", len(res.Keep), len(res.Remove), tc.keep)
			}
		})
	}
}

// An unlimited count is a rule of its own, any other negative count is
// refused before anything is read or removed.
func TestForgetTakesUnlimitedAndRefusesOtherNegativeCounts(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	for _, year := range []int{2023, 2024, 2025} {
		importAt(t, repo, time.Date(year, 3, 1, 12, 0, 0, 0, time.UTC))
		importAt(t, repo, time.Date(year, 6, 1, 12, 0, 0, 0, time.UTC))
	}

	res, err := repo.Forget(ctx, ark.ForgetPolicy{KeepYearly: ark.Unlimited}, ark.SnapshotFilter{},
		ark.WithForgetLocation(time.UTC))
	if err != nil {
		t.Fatalf("Forget with an unlimited count: %v", err)
	}
	if len(res.Keep) != 3 || len(res.Remove) != 3 {
		t.Fatalf("keeps %d and removes %d, want the newest of each of 3 years", len(res.Keep), len(res.Remove))
	}
	for _, s := range res.Keep {
		if s.CreatedAt.Month() != time.June {
			t.Errorf("kept %s, want the June snapshot of its year", s.CreatedAt)
		}
	}

	for _, policy := range []ark.ForgetPolicy{
		{KeepLast: -2},
		{KeepDaily: 7, KeepYearly: -5},
		{KeepLast: ark.Unlimited, KeepWeekly: -2},
	} {
		if _, err := repo.Forget(ctx, policy, ark.SnapshotFilter{}); !errors.Is(err, ark.ErrInvalidPolicy) {
			t.Errorf("Forget(%+v) = %v, want ErrInvalidPolicy", policy, err)
		}
	}
	if snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(snaps) != 3 {
		t.Errorf("after the refused policies: %d snapshots, %v, want 3", len(snaps), err)
	}
}

func TestParseKeepCount(t *testing.T) {
	for in, want := range map[string]ark.KeepCount{"0": 0, "7": 7, "unlimited": ark.Unlimited} {
		got, err := ark.ParseKeepCount(in)
		if err != nil || got != want {
			t.Errorf("ParseKeepCount(%q) = %d, %v, want %d", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("KeepCount(%d).String() = %q, want %q", got, got.String(), in)
		}
	}
	for _, in := range []string{"", "-1", "-3", "1.5", "all", "Unlimited", " 3"} {
		if got, err := ark.ParseKeepCount(in); err == nil {
			t.Errorf("ParseKeepCount(%q) = %d, want an error", in, got)
		}
	}
}
