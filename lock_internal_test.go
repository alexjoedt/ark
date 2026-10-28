package ark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain shortens the lock settle delay. Every operation acquires a lock, so
// the real 200 ms would dominate the runtime of the whole suite. The protocol
// under test is unchanged, only the pause between its two listings. The
// argon2id cost drops for the same reason; Open reads it from the key file.
func TestMain(m *testing.M) {
	lockSettleDelay = time.Millisecond
	defaultKDF = kdfParams{Time: 1, Memory: 64, Threads: 1}
	os.Exit(m.Run())
}

// testClock is the repository's time source in these tests. Locks age by
// moving it, not by waiting.
type testClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration // added after every reading, zero for a standing clock
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) setStep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = d
}

// lockRepo opens a repository over a stub backend with a clock the test drives.
func lockRepo(t *testing.T) (*Repository, *stubBackend, *testClock) {
	t.Helper()
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithPassword("ark-test")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, WithPassword("ark-test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock := &testClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	repo.clock = clock.Now
	t.Cleanup(func() { _ = repo.Close() })
	return repo, b, clock
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	return cmd.Process.Pid
}

func lockNames(t *testing.T, b *stubBackend) []string {
	t.Helper()
	var names []string
	err := b.List(context.Background(), LockFile, func(i BlobInfo) error {
		names = append(names, i.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("List locks: %v", err)
	}
	return names
}

func TestLockObjectIsSealedInTheLocksNamespace(t *testing.T) {
	ctx := context.Background()
	repo, b, _ := lockRepo(t)

	_, lock, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	names := lockNames(t, b)
	if len(names) != 1 {
		t.Fatalf("locks namespace holds %d objects, want 1", len(names))
	}

	h := Handle{Type: LockFile, Name: names[0]}
	if stored := readObject(t, b, h); stored[0] != formatSealed {
		t.Fatalf("lock starts with 0x%02x, want the sealed format byte", stored[0])
	}
	raw, err := repo.getSealed(ctx, h)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("lock object is not JSON: %v", err)
	}
	for _, field := range []string{"time", "exclusive", "hostname", "username", "pid", "uid", "gid"} {
		if _, ok := got[field]; !ok {
			t.Errorf("lock object has no %q field: %s", field, raw)
		}
	}

	var l lockObject
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("unmarshal lock: %v", err)
	}
	if !l.Exclusive {
		t.Error("exclusive lock was written as shared")
	}
	if l.PID != os.Getpid() {
		t.Errorf("lock pid is %d, want %d", l.PID, os.Getpid())
	}
	if l.Time.IsZero() {
		t.Error("lock carries no time")
	}
}

func readObject(t *testing.T, b Backend, h Handle) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("Get %s: %v", h.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", h.Name, err)
	}
	return data
}

func TestHeldLockRefreshesItself(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	restore := lockRefreshInterval
	lockRefreshInterval = time.Millisecond
	defer func() { lockRefreshInterval = restore }()

	_, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	first, err := repo.loadLock(ctx, lock.name)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}

	clock.advance(lockRefreshTimeout)

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := repo.loadLock(ctx, lock.name)
		if err != nil {
			t.Fatalf("load lock: %v", err)
		}
		if current.Time.After(first.Time) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lock object still carries its original time %s", first.Time)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSharedLocksCoexist(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := lockRepo(t)

	_, first, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("first shared lock: %v", err)
	}
	defer first.release(ctx)

	_, second, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	second.release(ctx)
}

func TestExclusiveLockExcludesEveryOther(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := lockRepo(t)

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	defer held.release(ctx)

	for _, exclusive := range []bool{false, true} {
		if _, _, err := repo.acquire(ctx, exclusive); !errors.Is(err, ErrRepoLocked) {
			t.Errorf("acquire(exclusive=%v) under an exclusive lock: %v, want ErrRepoLocked", exclusive, err)
		}
	}
}

func TestSharedLockBlocksTheExclusiveOne(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := lockRepo(t)

	_, held, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
	defer held.release(ctx)

	if _, _, err := repo.acquire(ctx, true); !errors.Is(err, ErrRepoLocked) {
		t.Errorf("exclusive lock under a shared one: %v, want ErrRepoLocked", err)
	}
}

func TestErrRepoLockedNamesTheHolder(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	defer held.release(ctx)

	clock.advance(90 * time.Second)

	_, _, err = repo.acquire(ctx, false)
	if err == nil {
		t.Fatal("acquire under an exclusive lock succeeded")
	}
	msg := err.Error()
	host, _ := os.Hostname()
	for _, want := range []string{host, strconv.Itoa(os.Getpid()), "1m30s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}

func TestLockWaitRetriesUntilTheLockIsFree(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := lockRepo(t)
	repo.clock = time.Now // the backoff needs a clock that moves on its own
	repo.lockWait = 5 * time.Second

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		held.release(ctx)
	}()

	_, second, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire with WithLockWait: %v", err)
	}
	second.release(ctx)
}

func TestLockIsStaleAfterThirtyMinutes(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	foreign := lockObject{Time: clock.now, Exclusive: true, Hostname: "other-host", PID: 4242}
	if err := repo.putLock(ctx, "foreign", foreign); err != nil {
		t.Fatalf("putLock: %v", err)
	}

	// A fresh lock of another host is never stale: its process cannot be
	// inspected from here.
	if _, _, err := repo.acquire(ctx, true); !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("acquire under a fresh foreign lock: %v, want ErrRepoLocked", err)
	}

	clock.advance(lockStaleAge)

	locks, err := repo.Locks(ctx)
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	if len(locks) != 1 || !locks[0].Stale {
		t.Fatalf("Locks reported %+v, want one stale lock", locks)
	}
	if _, lock, err := repo.acquire(ctx, true); err != nil {
		t.Errorf("acquire over a stale lock: %v", err)
	} else {
		lock.release(ctx)
	}
}

func TestLockOfThisHostIsStaleOnceItsProcessIsGone(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this machine")
	}
	dead := lockObject{Time: clock.now, Exclusive: true, Hostname: host, PID: deadPID(t)}
	if err := repo.putLock(ctx, "dead", dead); err != nil {
		t.Fatalf("putLock: %v", err)
	}

	locks, err := repo.Locks(ctx)
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	if len(locks) != 1 || !locks[0].Stale {
		t.Fatalf("Locks reported %+v, want one stale lock", locks)
	}

	// Fresh by time, but its process is gone, so it does not block.
	lock, err := func() (*repoLock, error) {
		_, l, err := repo.acquire(ctx, true)
		return l, err
	}()
	if err != nil {
		t.Fatalf("acquire over a lock with a dead pid: %v", err)
	}
	lock.release(ctx)
}

func TestZeroByteLockIsIgnored(t *testing.T) {
	ctx := context.Background()
	repo, b, _ := lockRepo(t)

	if err := b.Put(ctx, Handle{Type: LockFile, Name: "half-written"}, strings.NewReader("")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	locks, err := repo.Locks(ctx)
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	if len(locks) != 0 {
		t.Errorf("Locks reported %+v, want none", locks)
	}
	if _, lock, err := repo.acquire(ctx, true); err != nil {
		t.Errorf("acquire beside a zero-byte lock object: %v", err)
	} else {
		lock.release(ctx)
	}
}

func TestUnlockRemovesStaleLocksAndForceRemovesAll(t *testing.T) {
	ctx := context.Background()
	repo, b, clock := lockRepo(t)

	stale := lockObject{Time: clock.now.Add(-lockStaleAge), Hostname: "other-host", PID: 4242}
	fresh := lockObject{Time: clock.now, Hostname: "other-host", PID: 4243}
	if err := repo.putLock(ctx, "stale", stale); err != nil {
		t.Fatalf("putLock: %v", err)
	}
	if err := repo.putLock(ctx, "fresh", fresh); err != nil {
		t.Fatalf("putLock: %v", err)
	}

	removed, err := repo.Unlock(ctx)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if removed != 1 {
		t.Errorf("Unlock removed %d locks, want 1", removed)
	}
	if names := lockNames(t, b); len(names) != 1 || names[0] != "fresh" {
		t.Errorf("locks left: %v, want [fresh]", names)
	}

	removed, err = repo.Unlock(ctx, WithForce())
	if err != nil {
		t.Fatalf("Unlock force: %v", err)
	}
	if removed != 1 {
		t.Errorf("forced Unlock removed %d locks, want 1", removed)
	}
	if names := lockNames(t, b); len(names) != 0 {
		t.Errorf("locks left after force: %v, want none", names)
	}
}

// failingLocks refuses to write lock objects once armed, so a held lock can no
// longer refresh itself.
type failingLocks struct {
	*stubBackend

	mu     sync.Mutex
	broken bool
}

func (b *failingLocks) breakLockWrites() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.broken = true
}

func (b *failingLocks) Put(ctx context.Context, h Handle, r io.Reader) error {
	b.mu.Lock()
	broken := b.broken
	b.mu.Unlock()
	if broken && h.Type == LockFile {
		return errors.New("backend is down")
	}
	return b.stubBackend.Put(ctx, h, r)
}

func TestOperationAbortsWhenTheLockCannotBeRefreshed(t *testing.T) {
	ctx := context.Background()

	b := &failingLocks{stubBackend: newStubBackend()}
	if err := Init(ctx, b, WithPassword("ark-test")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, WithPassword("ark-test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	// Every reading of the clock jumps past the refresh timeout, so two ticks
	// are enough to reach the abort.
	clock := &testClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	repo.clock = clock.Now

	restore := lockRefreshInterval
	lockRefreshInterval = time.Millisecond
	defer func() { lockRefreshInterval = restore }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	b.breakLockWrites()
	clock.setStep(lockRefreshTimeout)

	select {
	case <-lockCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the operation was not aborted after the refresh deadline passed")
	}
}
