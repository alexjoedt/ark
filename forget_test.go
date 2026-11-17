package ark_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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
