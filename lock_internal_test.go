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
	"unsafe"
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
func lockRepo(t *testing.T, opts ...Option) (*Repository, *stubBackend, *testClock) {
	t.Helper()
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithPassword("ark-test")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, WithPassword("ark-test"), opts...)
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

	clock.advance(time.Minute)

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

func TestLockBackoffAddsJitterUpToTheInterval(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		base    time.Duration
	}{{0, lockBackoffFloor}, {1, min(2*lockBackoffFloor, lockBackoffMax)}, {1 << 20, lockBackoffMax}} {
		seen := map[time.Duration]bool{}
		for range 50 {
			d := lockBackoff(tc.attempt)
			if d < tc.base || d >= 2*tc.base {
				t.Fatalf("lockBackoff(%d) = %v, want in [%v, %v)", tc.attempt, d, tc.base, 2*tc.base)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Errorf("lockBackoff(%d) returned only %v in 50 calls, want jitter", tc.attempt, seen)
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

func TestLockWaitEndsAtItsDeadline(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := lockRepo(t)
	repo.clock = time.Now
	repo.lockWait = 50 * time.Millisecond

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	defer held.release(ctx)

	start := time.Now()
	if _, _, err := repo.acquire(ctx, false); !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("acquire under an exclusive lock: %v, want ErrRepoLocked", err)
	}
	if took := time.Since(start); took >= lockBackoffFloor {
		t.Fatalf("acquire with a 50ms wait took %v, want it to stop sleeping at the deadline", took)
	}
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

func TestCheckWarnsAboutAnOldZeroByteLock(t *testing.T) {
	ctx := context.Background()
	repo, b, clock := lockRepo(t)

	fresh := generateID(clock.now)
	old := generateID(clock.now.Add(-lockStaleAge))
	for _, name := range []string{fresh, old} {
		if err := b.Put(ctx, Handle{Type: LockFile, Name: name}, strings.NewReader("")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	res, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "lock "+old+" does not open") {
		t.Errorf("Check warnings %q, want one naming %s", res.Warnings, old)
	}
}

// slowAcquire lets the clock pass age once the first lock object is stored,
// as an acquire does whose second listing is slow, and fails every lock write
// after it.
type slowAcquire struct {
	*failingLocks
	clock *testClock
	age   time.Duration
}

func (b slowAcquire) Put(ctx context.Context, h Handle, r io.Reader) error {
	err := b.failingLocks.Put(ctx, h, r)
	if h.Type == LockFile && err == nil {
		b.clock.advance(b.age)
		b.breakLockWrites()
	}
	return err
}

// The others count a lock's age from the time its object carries, so the
// refresh timeout runs from there and not from the end of the acquire.
func TestRefreshTimeoutRunsFromTheStoredLockObject(t *testing.T) {
	ctx := context.Background()
	repo, b, clock := lockRepo(t)
	const acquire = 10 * time.Minute
	repo.backend = slowAcquire{&failingLocks{stubBackend: b}, clock, acquire}

	restore := lockRefreshInterval
	lockRefreshInterval = time.Millisecond
	defer func() { lockRefreshInterval = restore }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	clock.advance(lockRefreshTimeout - acquire)

	select {
	case <-lockCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("the operation runs on although its lock object is %s old", lockRefreshTimeout)
	}
}

// hangingLocks blocks every lock write after the first, the acquire's, until
// free is closed, whatever happens to the context of the write. hung is
// closed once a write blocks.
type hangingLocks struct {
	*stubBackend
	free, hung chan struct{}

	mu     sync.Mutex
	writes int
}

func (b *hangingLocks) Put(ctx context.Context, h Handle, r io.Reader) error {
	if h.Type == LockFile {
		b.mu.Lock()
		b.writes++
		first := b.writes == 1
		if b.writes == 2 {
			close(b.hung)
		}
		b.mu.Unlock()
		if !first {
			<-b.free
		}
	}
	return b.stubBackend.Put(ctx, h, r)
}

// A refresh that never returns must neither keep the operation running nor
// hold up its release, and the object it stores should it land late must go.
func TestReleaseReturnsWhileALockRefreshHangs(t *testing.T) {
	ctx := context.Background()
	repo, b, _ := lockRepo(t)
	hb := &hangingLocks{stubBackend: b, free: make(chan struct{}), hung: make(chan struct{})}
	repo.backend = hb
	free := sync.OnceFunc(func() { close(hb.free) })
	defer free()

	interval, timeout := lockRefreshInterval, lockRefreshTimeout
	lockRefreshInterval, lockRefreshTimeout = time.Millisecond, 100*time.Millisecond
	defer func() { lockRefreshInterval, lockRefreshTimeout = interval, timeout }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The clock stands still, so only the time that really passes can end
	// the operation.
	for name, ch := range map[string]<-chan struct{}{"a refresh hang": hb.hung, "the operation end": lockCtx.Done()} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("waited in vain for %s", name)
		}
	}

	released := make(chan struct{})
	go func() {
		lock.release(ctx)
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("release waits for a lock refresh that does not return")
	}
	if names := lockNames(t, b); len(names) != 0 {
		t.Fatalf("release left the lock objects %v", names)
	}

	free()
	<-lock.done
	if names := lockNames(t, b); len(names) != 0 {
		t.Errorf("the refresh that landed after the release left the lock objects %v", names)
	}
}

// A suspended or stopped process misses refreshes without any of them
// failing. Once its wall clock says the stored object passed the refresh
// timeout, a refresh that succeeds must not let the operation run on: others
// may have broken the lock in between.
func TestRefreshAfterALongStopAborts(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	restore := lockRefreshInterval
	lockRefreshInterval = time.Millisecond
	defer func() { lockRefreshInterval = restore }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	clock.advance(lockRefreshTimeout)

	select {
	case <-lockCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the operation runs on after its lock went unrefreshed past the timeout")
	}
}

// Tickers stop while a host is suspended, so after a resume the next refresh
// is up to lockRefreshInterval away. The operation must stop at the next
// check of the wall clock, not keep writing until then.
func TestResumedHostAbortsBeforeTheNextRefresh(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	restore := lockCheckInterval
	lockCheckInterval = time.Millisecond
	defer func() { lockCheckInterval = restore }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	clock.advance(lockRefreshTimeout)

	select {
	case <-lockCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the operation runs on until the next refresh after its host slept past the timeout")
	}
}

// A lock past its refresh timeout may already have been broken by others: a
// refresh then would republish it. keepAlive gives it up without a write.
func TestNoRefreshOnceTheTimeoutPassed(t *testing.T) {
	ctx := context.Background()
	repo, _, clock := lockRepo(t)

	interval, check := lockRefreshInterval, lockCheckInterval
	lockRefreshInterval, lockCheckInterval = time.Millisecond, time.Hour
	defer func() { lockRefreshInterval, lockCheckInterval = interval, check }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)
	first, err := repo.loadLock(ctx, lock.name)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}

	clock.advance(lockRefreshTimeout)
	select {
	case <-lock.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock is still refreshed after its timeout passed")
	}
	if lockCtx.Err() == nil {
		t.Fatal("the refresh stopped but the operation runs on")
	}
	last, err := repo.loadLock(ctx, lock.name)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	if !last.Time.Equal(first.Time) {
		t.Errorf("the lock object was rewritten at %s after its timeout passed", last.Time)
	}
}

// suspended returns t moved on by d on the wall clock only, as [time.Now] reads
// after a host slept for d: the monotonic clock stops during a suspend.
// [time.Time] is {wall uint64; ext int64; loc}, ext holds the monotonic reading.
func suspended(t time.Time, d time.Duration) time.Time {
	type rep struct {
		wall uint64
		ext  int64
	}
	s := t.Add(d)
	(*rep)(unsafe.Pointer(&s)).ext = (*rep)(unsafe.Pointer(&t)).ext
	return s
}

// signalLocks signals every lock write as it starts.
type signalLocks struct {
	*stubBackend
	puts chan struct{}
}

func (b signalLocks) Put(ctx context.Context, h Handle, r io.Reader) error {
	if h.Type == LockFile {
		select {
		case b.puts <- struct{}{}:
		default:
		}
	}
	return b.stubBackend.Put(ctx, h, r)
}

func TestRefreshCountsTheTimeAHostSlept(t *testing.T) {
	ctx := context.Background()
	repo, b, clock := lockRepo(t)
	base := time.Now()
	if s := suspended(base, time.Hour); s.Sub(base) != 0 || s.Round(0).Sub(base) != time.Hour {
		t.Fatalf("suspended does not model a suspend: monotonic %s, wall %s", s.Sub(base), s.Round(0).Sub(base))
	}
	clock.mu.Lock()
	clock.now = base
	clock.mu.Unlock()

	puts := make(chan struct{}, 1)
	repo.backend = signalLocks{b, puts}

	restore := lockRefreshInterval
	lockRefreshInterval = time.Millisecond
	defer func() { lockRefreshInterval = restore }()

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	// The second refresh starts once the first is stored, so the age runs
	// from a reading of the clock with a monotonic part.
	<-puts
	for range 2 {
		select {
		case <-puts:
		case <-time.After(5 * time.Second):
			t.Fatal("the lock is not refreshed")
		}
	}
	clock.mu.Lock()
	clock.now = suspended(base, lockStaleAge)
	clock.mu.Unlock()

	select {
	case <-lockCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the operation runs on after its host slept past the stale age")
	}
}

// An acquire that takes the refresh timeout or longer holds a lock others
// may already treat as stale: it fails instead of reporting the lock.
func TestAcquireSlowerThanTheRefreshTimeoutFails(t *testing.T) {
	ctx := context.Background()
	events := &lockEvents{}
	repo, b, clock := lockRepo(t, WithLockEvent(events.add))
	repo.backend = slowAcquire{&failingLocks{stubBackend: b}, clock, lockRefreshTimeout}

	lockCtx, lock, err := repo.acquire(ctx, false)
	if err == nil {
		lock.release(ctx)
		t.Fatalf("acquire succeeded, its context ended with %v", lockCtx.Err())
	}
	wantKinds(t, events.take())
	if names := lockNames(t, b); len(names) != 0 {
		t.Errorf("the failed acquire left the lock objects %v", names)
	}
}

// stalledLocks fails every lock write and blocks every lock delete until its
// context ends, as a backend does whose server stopped answering.
type stalledLocks struct{ *stubBackend }

func (b stalledLocks) Put(ctx context.Context, h Handle, r io.Reader) error {
	if h.Type == LockFile {
		return errors.New("timeout")
	}
	return b.stubBackend.Put(ctx, h, r)
}

func (b stalledLocks) Delete(ctx context.Context, h Handle) error {
	if h.Type == LockFile {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.stubBackend.Delete(ctx, h)
}

func TestFailedAcquireDoesNotWaitOnAStalledBackend(t *testing.T) {
	repo, b, _ := lockRepo(t)
	repo.backend = stalledLocks{b}

	restore := lockDropTimeout
	lockDropTimeout = 50 * time.Millisecond
	defer func() { lockDropTimeout = restore }()

	failed := make(chan error, 1)
	go func() {
		_, _, err := repo.acquire(context.Background(), true)
		failed <- err
	}()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("acquire succeeded although its lock write failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire waits on the removal of its failed lock write")
	}
}

// landedThenFailed stores a lock object and reports the write as failed, as
// a connection does that drops before the reply.
type landedThenFailed struct{ *stubBackend }

func (b landedThenFailed) Put(ctx context.Context, h Handle, r io.Reader) error {
	if err := b.stubBackend.Put(ctx, h, r); err != nil || h.Type != LockFile {
		return err
	}
	return errors.New("connection lost")
}

func TestFailedLockWriteLeavesNoLockBehind(t *testing.T) {
	ctx := context.Background()
	repo, b, _ := lockRepo(t)
	repo.backend = landedThenFailed{b}

	if _, _, err := repo.acquire(ctx, true); err == nil {
		t.Fatal("acquire succeeded although its lock write failed")
	}
	if names := lockNames(t, b); len(names) != 0 {
		t.Errorf("a failed acquire left the lock objects %v", names)
	}
}
