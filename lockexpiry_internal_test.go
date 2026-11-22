package ark

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// agingBackend ages the repository clock by the lock timeout once the first
// call that at matches returns, as a host suspended right after it would,
// and records the writes and deletes that follow.
type agingBackend struct {
	Backend
	clock *testClock
	at    func(op string, t HandleType) bool

	mu    sync.Mutex
	aged  bool
	after []string // op and namespace of every Put and Delete once aged
	syncs int      // Syncs once aged
}

func (b *agingBackend) Sync(ctx context.Context) error {
	err := b.Backend.Sync(ctx)
	b.mu.Lock()
	if b.aged {
		b.syncs++
	}
	b.mu.Unlock()
	return err
}

func (b *agingBackend) seen(op string, t HandleType) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aged && t != LockFile && (op == "put" || op == "delete") {
		b.after = append(b.after, op+" "+t.String())
	}
	if !b.aged && b.at(op, t) {
		b.aged = true
		b.clock.advance(lockRefreshTimeout)
	}
}

func (b *agingBackend) Put(ctx context.Context, h Handle, r io.Reader) error {
	err := b.Backend.Put(ctx, h, r)
	b.seen("put", h.Type)
	return err
}

func (b *agingBackend) Delete(ctx context.Context, h Handle) error {
	err := b.Backend.Delete(ctx, h)
	b.seen("delete", h.Type)
	return err
}

func (b *agingBackend) Get(ctx context.Context, h Handle) (io.ReadCloser, error) {
	rc, err := b.Backend.Get(ctx, h)
	b.seen("get", h.Type)
	return rc, err
}

func (b *agingBackend) Exists(ctx context.Context, h Handle) (bool, error) {
	ok, err := b.Backend.Exists(ctx, h)
	b.seen("exists", h.Type)
	return ok, err
}

func (b *agingBackend) List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error {
	err := b.Backend.List(ctx, t, fn)
	b.seen("list", t)
	return err
}

// wrote reports the writes and deletes after the clock aged, packs aside.
func (b *agingBackend) wrote() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.aged {
		return []string{"clock never aged"}
	}
	var out []string
	for _, w := range b.after {
		if !strings.HasSuffix(w, "packs") || strings.HasPrefix(w, "delete") {
			out = append(out, w)
		}
	}
	return out
}

func on(op string, t HandleType) func(string, HandleType) bool {
	return func(o string, h HandleType) bool { return o == op && h == t }
}

// noWatchTick keeps the watch from ever ticking during the test, so only the
// checks before a write can see that the lock aged.
func noWatchTick(t *testing.T) {
	t.Helper()
	restore := lockCheckInterval
	lockCheckInterval = time.Hour
	t.Cleanup(func() { lockCheckInterval = restore })
}

func agingRepo(t *testing.T, at func(string, HandleType) bool) (*Repository, *agingBackend) {
	t.Helper()
	noWatchTick(t)
	repo, _, clock := lockRepo(t)
	b := &agingBackend{Backend: repo.backend, clock: clock, at: func(string, HandleType) bool { return false }}
	repo.backend = b
	b.at = at
	return repo, b
}

func wantExpired(t *testing.T, err error, b *agingBackend) {
	t.Helper()
	if !errors.Is(err, errLockExpired) {
		t.Errorf("err = %v, want errLockExpired", err)
	}
	if w := b.wrote(); len(w) > 0 {
		t.Errorf("wrote after the lock expired: %v", w)
	}
}

// A backup suspended past its lock timeout, with no watch tick since, writes
// no index object and no manifest. Without the checks the first two cases
// write both and the backup succeeds.
func TestBackupPastItsLockTimeoutWritesNoIndexOrManifest(t *testing.T) {
	for name, at := range map[string]func(string, HandleType) bool{
		"aged after a pack":         on("put", DataPackFile),
		"aged after the data index": on("put", DataIndexFile),
		"aged after the tree index": on("put", TreeIndexFile),
		"aged during the manifest":  on("put", SnapshotFile),
	} {
		t.Run(name, func(t *testing.T) {
			repo, b := agingRepo(t, at)
			_, err := repo.Backup(context.Background(), NewStreamSource("f", strings.NewReader("content")))
			wantExpired(t, err, b)
		})
	}
}

func exportedArchive(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	repo, _, _ := lockRepo(t)
	snap, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content")))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var buf bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return buf.Bytes()
}

func TestImportPastItsLockTimeoutWritesNoIndexOrManifest(t *testing.T) {
	archive := exportedArchive(t)
	for name, at := range map[string]func(string, HandleType) bool{
		"aged after a pack":    on("put", DataPackFile),
		"aged after the index": on("put", TreeIndexFile),
	} {
		t.Run(name, func(t *testing.T) {
			repo, b := agingRepo(t, at)
			_, err := repo.Import(context.Background(), bytes.NewReader(archive))
			wantExpired(t, err, b)
		})
	}
}

// forgottenRepo holds a kept and a forgotten backup, so a prune has packs to
// delete and an index to write that lists the kept ones.
func forgottenRepo(t *testing.T, at func(string, HandleType) bool) (*Repository, *agingBackend) {
	t.Helper()
	ctx := context.Background()
	repo, b := agingRepo(t, func(string, HandleType) bool { return false })
	if _, err := repo.Backup(ctx, NewStreamSource("kept", strings.NewReader("kept"))); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	snap, err := repo.Backup(ctx, NewStreamSource("gone", strings.NewReader("gone")))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := repo.Forget(ctx, ForgetPolicy{}, SnapshotFilter{IDs: []SnapshotID{snap.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	b.at = at
	return repo, b
}

func TestPrunePastItsLockTimeoutDeletesNothing(t *testing.T) {
	t.Run("aged at the index load", func(t *testing.T) {
		repo, b := forgottenRepo(t, on("get", DataIndexFile))
		_, err := repo.Prune(context.Background())
		wantExpired(t, err, b)
	})
	t.Run("aged after the new index", func(t *testing.T) {
		repo, b := forgottenRepo(t, on("put", TreeIndexFile))
		_, err := repo.Prune(context.Background())
		wantExpired(t, err, b)
	})
	t.Run("aged before the sweep of a pack no index lists", func(t *testing.T) {
		ctx := context.Background()
		repo, b := agingRepo(t, func(string, HandleType) bool { return false })
		if _, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content"))); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		stray := Handle{Type: DataPackFile, Name: strings.Repeat("ab", 32)}
		if err := b.Backend.Put(ctx, stray, strings.NewReader("orphan")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		b.at = on("list", TreePackFile)
		_, err := repo.Prune(ctx)
		wantExpired(t, err, b)
		if ok, _ := b.Backend.Exists(ctx, stray); !ok {
			t.Error("the pack no index lists is gone")
		}
	})
	t.Run("aged before the delete of an index object that does not open", func(t *testing.T) {
		ctx := context.Background()
		repo, b := agingRepo(t, func(string, HandleType) bool { return false })
		if _, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content"))); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		broken := Handle{Type: DataIndexFile, Name: strings.Repeat("cd", 32)}
		if err := b.Backend.Put(ctx, broken, strings.NewReader("garbage")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		b.at = on("put", DataIndexFile)
		_, err := repo.Prune(ctx)
		wantExpired(t, err, b)
		if ok, _ := b.Backend.Exists(ctx, broken); !ok {
			t.Error("the index object that does not open is gone")
		}
	})
	t.Run("aged before the lost-pack marker goes", func(t *testing.T) {
		ctx := context.Background()
		repo, b := agingRepo(t, func(string, HandleType) bool { return false })
		if _, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content"))); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if err := repo.markLostPacks(ctx); err != nil {
			t.Fatalf("markLostPacks: %v", err)
		}
		b.at = on("exists", LostPackFile)
		_, err := repo.Prune(ctx)
		wantExpired(t, err, b)
		if !repo.lostPacksMarked(ctx) {
			t.Error("the lost-pack marker is gone")
		}
	})
}

func TestForgetPastItsLockTimeoutRemovesNothing(t *testing.T) {
	ctx := context.Background()
	repo, b := agingRepo(t, func(string, HandleType) bool { return false })
	var ids []SnapshotID
	for range 2 {
		snap, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content")))
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		ids = append(ids, snap.ID)
	}
	b.at = func(string, HandleType) bool { return false }
	confirm := WithForgetConfirm(func(*ForgetResult) error {
		b.mu.Lock()
		b.aged = true
		b.mu.Unlock()
		b.clock.advance(lockRefreshTimeout)
		return nil
	})
	_, err := repo.Forget(ctx, ForgetPolicy{}, SnapshotFilter{IDs: ids}, confirm)
	wantExpired(t, err, b)
	for _, id := range ids {
		if ok, _ := b.Backend.Exists(ctx, Handle{Type: SnapshotFile, Name: string(id)}); !ok {
			t.Errorf("snapshot %s removed", id)
		}
	}
}

// The removals a forget made before its lock expired are synced anyway: a
// prune that takes over the stale lock may sweep their objects.
func TestForgetSyncsTheRemovalsBeforeItsLockExpired(t *testing.T) {
	ctx := context.Background()
	repo, b := agingRepo(t, func(string, HandleType) bool { return false })
	var ids []SnapshotID
	for range 2 {
		snap, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content")))
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		ids = append(ids, snap.ID)
	}
	b.at = on("delete", SnapshotFile)
	_, err := repo.Forget(ctx, ForgetPolicy{}, SnapshotFilter{IDs: ids})
	wantExpired(t, err, b)
	if b.syncs == 0 {
		t.Error("the removal before the lock expired was not synced")
	}
}

func TestChangePasswordPastItsLockTimeoutKeepsTheOldKey(t *testing.T) {
	repo, b := agingRepo(t, on("put", KeyFile))
	err := repo.ChangePassword(context.Background(), NewPassword("other"))
	wantExpired(t, err, b)
	if _, err := Open(context.Background(), b.Backend, NewPassword("ark-test")); err != nil {
		t.Errorf("the old password no longer opens: %v", err)
	}
}

func TestMarkLostPacksPastItsLockTimeoutWritesNoMarker(t *testing.T) {
	ctx := context.Background()
	repo, b := agingRepo(t, func(string, HandleType) bool { return false })
	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(lockCtx)
	b.mu.Lock()
	b.aged = true
	b.mu.Unlock()
	b.clock.advance(lockRefreshTimeout)
	wantExpired(t, repo.markLostPacks(lockCtx), b)
	if repo.lostPacksMarked(ctx) {
		t.Error("the lost-pack marker is stored")
	}
}
