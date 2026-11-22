package ark_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// shortRange returns one byte less than asked for from every range read of a
// pack while armed, with a nil error.
type shortRange struct {
	ark.Backend
	armed bool
}

func (b *shortRange) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	got, err := b.Backend.GetRange(ctx, h, offset, length)
	if err == nil && b.armed && h.Type == ark.DataPackFile && len(got) > 0 {
		return got[:len(got)-1], nil
	}
	return got, err
}

func TestShortRangeReadIsAnErrorNotAPanic(t *testing.T) {
	ctx := context.Background()
	b := &shortRange{Backend: arkmem.New()}
	repo := openRepo(t, b)
	first := mustBackup(t, repo, smallFiles("one", 8))
	mustBackup(t, repo, smallFiles("one", 4))
	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{first.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil || len(snaps) == 0 {
		t.Fatalf("Snapshots: %v, %d", err, len(snaps))
	}
	b.armed = true

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s = %v, want an error wrapping io.ErrUnexpectedEOF", name, err)
		} else if !strings.Contains(err.Error(), "object") {
			t.Errorf("%s = %v, want it to name the pack", name, err)
		}
	}
	_, err = repo.Restore(ctx, snaps[0].ID, &recordingSink{})
	check("Restore", err)
	_, err = repo.Verify(ctx, snaps[0].ID)
	check("Verify", err)
	res, err := repo.Check(ctx)
	if err != nil || !slices.ContainsFunc(res.Errors, func(f string) bool {
		return strings.Contains(f, io.ErrUnexpectedEOF.Error()) && strings.Contains(f, "object")
	}) {
		t.Errorf("Check = %v, %v, want a finding wrapping %v", res, err, io.ErrUnexpectedEOF)
	}
	_, err = repo.Prune(ctx)
	check("Prune", err)
}
