package ark_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
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
		keep int // snapshots that are the newest of their day
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
			// the older snapshot is kept either way, as the newest of its
			// day or as the oldest of the series: the reason tells them apart
			days := 0
			for _, reasons := range res.KeepReasons {
				if slices.Contains(reasons, "daily snapshot") {
					days++
				}
			}
			if days != tc.keep || len(res.Keep)+len(res.Remove) != 2 {
				t.Errorf("%d snapshots are the newest of a day, want %d of 2: %v", days, tc.keep, res.KeepReasons)
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
	if len(res.Keep) != 4 || len(res.Remove) != 2 {
		t.Fatalf("keeps %d and removes %d, want the newest of 3 years and the oldest", len(res.Keep), len(res.Remove))
	}
	for _, s := range res.Remove {
		if s.CreatedAt.Month() != time.March || s.CreatedAt.Year() == 2023 {
			t.Errorf("removed %s, want the March snapshots of 2024 and 2025", s.CreatedAt)
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
	if snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(snaps) != 4 {
		t.Errorf("after the refused policies: %d snapshots, %v, want 4", len(snaps), err)
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
	for _, in := range []string{"Unlimited", "UNLIMITED"} {
		if got, err := ark.ParseKeepCount(in); err != nil || got != ark.Unlimited {
			t.Errorf("ParseKeepCount(%q) = %d, %v, want Unlimited", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "-3", "1.5", "all", "0x10", " 3"} {
		if got, err := ark.ParseKeepCount(in); err == nil {
			t.Errorf("ParseKeepCount(%q) = %d, want an error", in, got)
		}
	}
}

// A policy in JSON holds plain numbers, and the word for Unlimited.
func TestKeepCountJSON(t *testing.T) {
	policy := ark.ForgetPolicy{KeepLast: 3, KeepYearly: ark.Unlimited}
	data, err := json.Marshal(policy)
	if err != nil || !bytes.Contains(data, []byte(`"KeepLast":3,`)) ||
		!bytes.Contains(data, []byte(`"KeepYearly":"unlimited",`)) {
		t.Fatalf("Marshal = %s, %v, want KeepLast 3 and KeepYearly \"unlimited\"", data, err)
	}
	var back ark.ForgetPolicy
	if err := json.Unmarshal(data, &back); err != nil || back != policy {
		t.Errorf("Unmarshal(%s) = %+v, %v, want %+v", data, back, err, policy)
	}
	in := `{"KeepLast":3,"KeepDaily":"7","KeepWeekly":null,"KeepYearly":"Unlimited"}`
	policy.KeepDaily = 7
	if err := json.Unmarshal([]byte(in), &back); err != nil || back != policy {
		t.Errorf("Unmarshal(%s) = %+v, %v, want %+v", in, back, err, policy)
	}
	for _, bad := range []string{`{"KeepLast":-1}`, `{"KeepLast":1.5}`, `{"KeepLast":"all"}`, `{"KeepLast":true}`} {
		if err := json.Unmarshal([]byte(bad), &back); err == nil {
			t.Errorf("Unmarshal(%s) = %+v, want an error", bad, back)
		}
	}
}
