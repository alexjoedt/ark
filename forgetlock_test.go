package ark_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// manifestDeleteGate holds every manifest Delete until a second one arrives
// or a second has passed, so two Forgets that run at once both decide before
// either removes anything.
type manifestDeleteGate struct {
	ark.Backend
	n    atomic.Int32
	once sync.Once
	both chan struct{}
}

func (b *manifestDeleteGate) Delete(ctx context.Context, h ark.Handle) error {
	if h.Type == ark.SnapshotFile {
		if b.n.Add(1) >= 2 {
			b.once.Do(func() { close(b.both) })
		}
		select {
		case <-b.both:
		case <-time.After(time.Second):
		}
	}
	return b.Backend.Delete(ctx, h)
}

// TestConcurrentForgetsKeepTheSeries: KeepTags keeps only the tagged
// snapshot, KeepLast only the newer untagged one. Run from one listing, the
// two remove both. Each Forget holds the exclusive lock, so the second fails
// to lock or decides from what the first left.
func TestConcurrentForgetsKeepTheSeries(t *testing.T) {
	ctx := context.Background()
	b := &manifestDeleteGate{Backend: arkmem.New(), both: make(chan struct{})}
	repo := openRepo(t, b)
	now := time.Now()
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"},
		ark.WithTags("pinned"), ark.WithTime(now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "two"},
		ark.WithTime(now.Add(-time.Hour))); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	policies := []ark.ForgetPolicy{{KeepTags: [][]string{{"pinned"}}}, {KeepLast: 1}}
	errs := make([]error, len(policies))
	var wg sync.WaitGroup
	for i, policy := range policies {
		handle := openOn(t, b)
		wg.Go(func() {
			_, errs[i] = handle.Forget(ctx, policy, ark.SnapshotFilter{})
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ark.ErrRepoLocked) && !errors.Is(err, ark.ErrNoSnapshotKept) {
			t.Errorf("Forget %d = %v, want nil, ErrRepoLocked or ErrNoSnapshotKept", i, err)
		}
	}
	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) == 0 {
		t.Errorf("the two Forgets (%v, %v) left the series empty", errs[0], errs[1])
	}
}

// TestForgetWaitsForASharedLock: a Forget that removes needs the
// repository to itself, so it fails beside a reader, and with a lock wait it
// runs once the reader is gone.
func TestForgetWaitsForASharedLock(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	snap := mustBackup(t, repo, smallFiles("app", 2))
	named := ark.SnapshotFilter{IDs: []ark.SnapshotID{snap.ID}}

	rd, err := openOn(t, b).OpenReader(ctx)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, named); !errors.Is(err, ark.ErrRepoLocked) {
		_ = rd.Close()
		t.Fatalf("Forget beside a reader = %v, want ErrRepoLocked", err)
	}

	const held = 500 * time.Millisecond
	start := time.Now()
	go func() {
		time.Sleep(held)
		_ = rd.Close()
	}()
	if _, err := openOn(t, b, ark.WithLockWait(time.Minute)).Forget(ctx, ark.ForgetPolicy{}, named); err != nil {
		t.Fatalf("Forget with a lock wait: %v", err)
	}
	if d := time.Since(start); d < held {
		t.Errorf("Forget with a lock wait returned after %s, before the reader closed at %s", d, held)
	}
	if snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots after the forget = %v, %v; want none", snaps, err)
	}
}

// TestForgetDryRunRunsBesideABackup: a dry run removes nothing, so the
// shared lock is enough and a running backup does not stop it.
func TestForgetDryRunRunsBesideABackup(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	mustBackup(t, repo, smallFiles("app", 2))

	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	backer := openOn(t, b, ark.WithLockEvent(func(ev ark.LockEvent) {
		if ev.Kind == ark.LockAcquired {
			once.Do(func() { close(parked) })
			<-release
		}
	}))
	backedUp := make(chan error, 1)
	go func() {
		_, err := backer.Backup(ctx, smallFiles("app", 3))
		backedUp <- err
	}()
	<-parked

	_, err := repo.Forget(ctx, ark.ForgetPolicy{KeepLast: 1}, ark.SnapshotFilter{}, ark.WithForgetDryRun())
	close(release)
	if err != nil {
		t.Errorf("Forget dry run beside a backup: %v", err)
	}
	if err := <-backedUp; err != nil {
		t.Errorf("Backup: %v", err)
	}
}
