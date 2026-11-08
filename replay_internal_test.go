package ark

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestReplayedLockIsHonouredUntilItIsStale replays a released exclusive lock.
// It blocks every other lock like the original until lockStaleAge, then is
// ignored. The lock names this live test process, so only the stale age
// applies; a lock of an exited process on this host is covered by
// TestLockOfThisHostIsStaleOnceItsProcessIsGone.
func TestReplayedLockIsHonouredUntilItIsStale(t *testing.T) {
	ctx := context.Background()
	repo, b, clock := lockRepo(t)

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	names := lockNames(t, b)
	if len(names) != 1 {
		t.Fatalf("locks namespace holds %d objects, want 1", len(names))
	}
	h := Handle{Type: LockFile, Name: names[0]}
	old := readObject(t, b, h)
	held.release(ctx)

	if err := b.Put(ctx, h, bytes.NewReader(old)); err != nil {
		t.Fatalf("replay lock: %v", err)
	}
	if _, _, err := repo.acquire(ctx, false); !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("acquire beside the replayed lock: %v, want ErrRepoLocked", err)
	}

	clock.advance(lockStaleAge)
	locks, err := repo.Locks(ctx)
	if err != nil || len(locks) != 1 || !locks[0].Stale {
		t.Fatalf("Locks = %+v, %v; want the replayed lock stale", locks, err)
	}
	_, lock, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("acquire over the stale replayed lock: %v", err)
	}
	lock.release(ctx)
}

// TestReplayedConfigFailsOpen replays the config of before a config change.
// The key file is bound to the new bytes, so Open fails with
// ErrCorruptConfig. Replaying the old key file with it is a rollback of the
// whole repository state and opens: there is no freshness anchor.
func TestReplayedConfigFailsOpen(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfgHandle := Handle{Type: ConfigFile, Name: configName}
	oldCfg := readObject(t, b, cfgHandle)
	old, err := parseRepositoryConfig(oldCfg)
	if err != nil {
		t.Fatalf("parseRepositoryConfig: %v", err)
	}
	oldKey := onlyKeyHandle(t, b)
	oldKeyData := readObject(t, b, oldKey)

	rebindConfig(t, b, func(cfg *repositoryConfig) { cfg.Chunker.Min++ })
	repo, err := Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open after the change: %v", err)
	}
	_ = repo.Close()

	if err := b.Put(ctx, cfgHandle, bytes.NewReader(oldCfg)); err != nil {
		t.Fatalf("replay config: %v", err)
	}
	if _, err := Open(ctx, b, WithoutPassword()); !errors.Is(err, ErrCorruptConfig) {
		t.Fatalf("Open with the replayed config = %v, want ErrCorruptConfig", err)
	}

	if err := b.Put(ctx, oldKey, bytes.NewReader(oldKeyData)); err != nil {
		t.Fatalf("replay key file: %v", err)
	}
	repo, err = Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open with the replayed config and key file: %v", err)
	}
	defer repo.Close()
	if repo.chunker.MinSize != old.Chunker.Min {
		t.Errorf("chunker min = %d, want the rolled back %d", repo.chunker.MinSize, old.Chunker.Min)
	}
}

func onlyKeyHandle(t *testing.T, b Backend) Handle {
	t.Helper()
	var names []string
	if err := b.List(context.Background(), KeyFile, func(info BlobInfo) error {
		names = append(names, info.Name)
		return nil
	}); err != nil {
		t.Fatalf("List keys: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("repository has %d key files, want 1", len(names))
	}
	return Handle{Type: KeyFile, Name: names[0]}
}
