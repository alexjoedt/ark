package ark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/user"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Lock timings. A lock is refreshed every lockRefreshInterval
// and counts as stale once it is lockStaleAge old, so a holder that stops
// refreshing is reclaimed after five missed refreshes. An operation whose own
// lock has not refreshed within lockRefreshTimeout gives up rather than keep
// writing under a lock others may already treat as stale.
const lockStaleAge = 30 * time.Minute

// lockRefreshInterval is how often a held lock rewrites its object,
// lockRefreshTimeout how long it may go without, lockCheckInterval how often
// its age is checked against that and lockDropTimeout how long removing it
// after an operation or a failed attempt may take. Like lockSettleDelay they
// are variables so the test binary can shorten them; nothing outside the
// package can.
var (
	lockRefreshInterval = 5 * time.Minute
	lockRefreshTimeout  = 22*time.Minute + 30*time.Second
	lockCheckInterval   = time.Second
	lockDropTimeout     = time.Minute
)

// lockSettleDelay is the pause between publishing our own lock and re-reading
// the namespace, long enough for a concurrent writer's Put to become visible.
// It is a variable so the test binary can shorten it; nothing outside the
// package can.
var lockSettleDelay = 200 * time.Millisecond

// LockEventKind says what happened to a lock, see LockEvent. Its value is
// the kind's name.
type LockEventKind string

// The lock events WithLockEvent reports.
const (
	// LockAcquired: the operation holds its lock and starts.
	LockAcquired LockEventKind = "acquired"
	// LockRejected: an attempt found conflicting locks and backed off. Under
	// WithLockWait every attempt reports one.
	LockRejected LockEventKind = "rejected"
	// LockReleased: the operation is done, its lock object is about to be
	// deleted.
	LockReleased LockEventKind = "released"
	// LockLeftOver: deleting this repository's lock object failed, after a
	// release, an attempt that backed off or a lock write that failed. The
	// object may be left behind, a failed write may also have stored none; one
	// that is left blocks others until it is stale or Unlock removes it.
	LockLeftOver LockEventKind = "left-over"
)

// LockEvent is one change of a lock this repository takes, see WithLockEvent.
type LockEvent struct {
	Kind      LockEventKind
	Exclusive bool // the class of this repository's lock
	// LockID names this repository's lock object, as Locks lists it. It is
	// empty on a rejection found before the object was written.
	LockID string
	// Holders are the conflicting locks a rejected attempt listed, nil on
	// every other kind.
	Holders []LockInfo
}

// reportLock calls the repository's lock hook, if any.
func (r *Repository) reportLock(ev LockEvent) {
	if r.lockEvent != nil {
		r.lockEvent(ev)
	}
}

// lockObject is the JSON body of a lock, frozen by the v1 format.
type lockObject struct {
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
	PID       int       `json:"pid"`
	UID       int       `json:"uid"`
	GID       int       `json:"gid"`
}

// LockInfo describes one lock object in the repository.
type LockInfo struct {
	ID        string
	Time      time.Time
	Exclusive bool
	Hostname  string
	Username  string
	PID       int
	Stale     bool
}

// newLockObject fills in the identity of this process.
func newLockObject(now time.Time, exclusive bool) lockObject {
	host, _ := os.Hostname()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return lockObject{
		Time:      now.UTC(),
		Exclusive: exclusive,
		Hostname:  host,
		Username:  name,
		PID:       os.Getpid(),
		UID:       os.Getuid(),
		GID:       os.Getgid(),
	}
}

// stale reports whether the lock may be ignored and removed. A lock of this
// host whose process is gone is stale at once; any lock is stale once it is
// older than lockStaleAge. Another host's fresh lock is never stale, its
// process cannot be inspected from here.
func (l lockObject) stale(now time.Time) bool {
	if now.Sub(l.Time) >= lockStaleAge {
		return true
	}
	host, err := os.Hostname()
	if err != nil || l.Hostname != host || l.PID <= 0 {
		return false
	}
	return !processAlive(l.PID)
}

func (l lockObject) info(id string, now time.Time) LockInfo {
	return LockInfo{
		ID:        id,
		Time:      l.Time,
		Exclusive: l.Exclusive,
		Hostname:  l.Hostname,
		Username:  l.Username,
		PID:       l.PID,
		Stale:     l.stale(now),
	}
}

// processAlive reports whether a process with this pid exists. Signal 0 is
// delivered to no one, it only runs the kernel's permission and existence
// checks. A permission error means the process exists under another user.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// putLock seals a lock object and writes it under name.
func (r *Repository) putLock(ctx context.Context, name string, l lockObject) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("lock: failed to marshal: %w", err)
	}
	return r.putSealed(ctx, Handle{Type: LockFile, Name: name}, data)
}

// loadLocks reads every lock object in the repository, newest first.
//
// A zero-byte object is another writer's Put in flight, not a lock, and is
// skipped; so is one that does not open or parse, which would otherwise block
// the repository forever. Those are returned by name for Unlock WithForce,
// and so is a zero-byte object named older than lockStaleAge or not named
// like a lock: that is a truncated lock (or, briefly, a refresh in flight on
// a backend whose Put is not atomic).
func (r *Repository) loadLocks(ctx context.Context) (map[string]lockObject, []string, error) {
	now := r.now()
	var names, unreadable []string
	err := r.backend.List(ctx, LockFile, func(info BlobInfo) error {
		if info.Size == 0 {
			if t, ok := idTime(info.Name); !ok || now.Sub(t) >= lockStaleAge {
				unreadable = append(unreadable, info.Name)
			}
			return nil
		}
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("lock: failed to list locks: %w", err)
	}

	locks := make(map[string]lockObject, len(names))
	for _, name := range names {
		l, err := r.loadLock(ctx, name)
		switch {
		case errors.Is(err, ErrNotFound):
			continue // released since the listing
		case err != nil:
			unreadable = append(unreadable, name)
			continue
		}
		locks[name] = l
	}
	return locks, unreadable, nil
}

func (r *Repository) loadLock(ctx context.Context, name string) (lockObject, error) {
	data, err := r.getSealed(ctx, Handle{Type: LockFile, Name: name})
	if err != nil {
		return lockObject{}, err
	}
	var l lockObject
	if err := json.Unmarshal(data, &l); err != nil {
		return lockObject{}, fmt.Errorf("lock: failed to parse %s: %w", name, err)
	}
	return l, nil
}

// conflict returns the locks that block a new one of this class, sorted by
// name, or nil when none does. An exclusive lock needs an empty namespace; a
// shared lock only needs no exclusive holder. Stale locks are ignored, ours is
// skipped by name.
func conflict(locks map[string]lockObject, own string, exclusive bool, now time.Time) []LockInfo {
	names := make([]string, 0, len(locks))
	for name := range locks {
		names = append(names, name)
	}
	sort.Strings(names) // a stable answer beats whichever the map yields first

	var held []LockInfo
	for _, name := range names {
		if name == own {
			continue
		}
		l := locks[name]
		if !exclusive && !l.Exclusive {
			continue
		}
		if info := l.info(name, now); !info.Stale {
			held = append(held, info)
		}
	}
	return held
}

// lockedError wraps ErrRepoLocked with the holder's identity, so an operator
// reading the message knows which process to look at.
func lockedError(h LockInfo, now time.Time) error {
	return fmt.Errorf("%w: %s lock held by %s pid %d for %s",
		ErrRepoLocked, lockClass(h.Exclusive), h.Hostname, h.PID, now.Sub(h.Time).Round(time.Second))
}

// lockClass names the class of a lock.
func lockClass(exclusive bool) string {
	if exclusive {
		return "exclusive"
	}
	return "shared"
}

// repoLock is a held lock. Its refresh goroutine keeps the object young, its
// watch goroutine cancels the operation's context once the object is too old.
type repoLock struct {
	repo      *Repository
	name      string
	exclusive bool
	timeout   time.Duration   // lockRefreshTimeout when the lock was taken
	ctx       context.Context // the operation's
	cancel    context.CancelFunc
	done      chan struct{} // closed when keepAlive returns

	abortOnce sync.Once
	expired   chan struct{} // closed by abort

	mu sync.Mutex
	// stored is the time the stored lock object carries, which the others
	// count its age from; started is when its write began, on the monotonic
	// clock, which goes on counting while a refresh hangs or the wall clock
	// steps back.
	stored, started time.Time
	gaveUp          bool // release stopped waiting for a hung refresh
}

// acquire takes a lock of the requested class and returns it together with a
// context that is cancelled when refreshing fails for lockRefreshTimeout.
//
// The protocol is list, put, wait, list again: the second listing catches a
// writer whose Put was not yet visible during the first, and both sides then
// back off, so no pair of conflicting locks survives.
func (r *Repository) acquire(ctx context.Context, exclusive bool) (context.Context, *repoLock, error) {
	deadline := r.now().Add(r.lockWait)
	for attempt := 0; ; attempt++ {
		lockCtx, l, err := r.tryAcquire(ctx, exclusive)
		if err == nil {
			return lockCtx, l, nil
		}
		left := deadline.Sub(r.now())
		if !errors.Is(err, ErrRepoLocked) || left <= 0 {
			return nil, nil, err
		}
		if waitErr := sleepCtx(ctx, min(lockBackoff(attempt), left)); waitErr != nil {
			return nil, nil, waitErr
		}
	}
}

// Backoff bounds for a waiting acquire, so a queue of waiters spreads out
// instead of hammering the backend. With jitter a sleep lasts up to twice the
// bound; acquire never sleeps past its deadline.
const (
	lockBackoffFloor = 500 * time.Millisecond
	lockBackoffMax   = 8 * time.Second
)

// lockBackoff doubles the wait with every failed attempt, up to lockBackoffMax,
// and adds a random jitter of up to that wait, so waiters that collided do not
// retry in lockstep. The shift is capped so a long wait never overflows into a
// zero or negative delay.
func lockBackoff(attempt int) time.Duration {
	d := min(lockBackoffFloor<<min(attempt, 16), lockBackoffMax)
	return d + rand.N(d) //nolint:gosec // jitter, not a secret
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	case <-t.C:
		return nil
	}
}

// lockNow reads the repository clock without its monotonic part. Lock ages
// are wall clock differences, as the others compute them: a monotonic reading
// would not count the time a suspended host slept.
func (r *Repository) lockNow() time.Time { return r.now().Round(0) }

func (r *Repository) tryAcquire(ctx context.Context, exclusive bool) (context.Context, *repoLock, error) {
	now := r.lockNow()

	locks, unreadable, err := r.loadLocks(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range unreadable {
		r.logger.WarnContext(ctx, "ignoring a lock that does not open, Unlock with WithForce removes it", "lock", name)
	}
	if held := conflict(locks, "", exclusive, now); held != nil {
		err := lockedError(held[0], now)
		r.reportLock(LockEvent{Kind: LockRejected, Exclusive: exclusive, Holders: held})
		return nil, nil, err
	}

	name, started := generateID(now), time.Now()
	if err := r.putLock(ctx, name, newLockObject(now, exclusive)); err != nil {
		// A put that failed may still have stored the object.
		r.dropLock(ctx, name, exclusive)
		return nil, nil, err
	}
	stored := now

	if err := sleepCtx(ctx, lockSettleDelay); err != nil {
		r.dropLock(ctx, name, exclusive)
		return nil, nil, err
	}

	locks, _, err = r.loadLocks(ctx)
	if err != nil {
		r.dropLock(ctx, name, exclusive)
		return nil, nil, err
	}
	now = r.lockNow()
	if held := conflict(locks, name, exclusive, now); held != nil {
		err := lockedError(held[0], now)
		gone := r.removeLock(ctx, name)
		r.reportLock(LockEvent{Kind: LockRejected, Exclusive: exclusive, LockID: name, Holders: held})
		if !gone {
			r.reportLock(LockEvent{Kind: LockLeftOver, Exclusive: exclusive, LockID: name})
		}
		return nil, nil, err
	}

	if age := now.Sub(stored); age >= lockRefreshTimeout {
		r.dropLock(ctx, name, exclusive)
		return nil, nil, fmt.Errorf("lock: acquiring took %s, others may treat the lock as stale", age)
	}

	lockCtx, cancel := context.WithCancel(ctx)
	l := &repoLock{
		repo: r, name: name, exclusive: exclusive, timeout: lockRefreshTimeout,
		ctx: lockCtx, cancel: cancel, done: make(chan struct{}), expired: make(chan struct{}),
		stored: stored, started: started,
	}
	go l.keepAlive(lockCtx)
	go l.watch(lockCheckInterval)
	r.reportLock(LockEvent{Kind: LockAcquired, Exclusive: exclusive, LockID: name})
	return lockCtx, l, nil
}

// keepAlive rewrites the lock object every lockRefreshInterval. It never
// refreshes a lock whose timeout has passed: others may already have broken
// it, and a new object would hand it back to an operation that is about to
// abort.
func (l *repoLock) keepAlive(ctx context.Context) {
	defer close(l.done)
	defer l.removeAbandoned()

	t := time.NewTicker(lockRefreshInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// select picks either case once both are ready
		if ctx.Err() != nil || l.expire() {
			return
		}

		start, now := time.Now(), l.repo.lockNow()
		if err := l.repo.putLock(ctx, l.name, newLockObject(now, l.exclusive)); err != nil {
			l.repo.logger.WarnContext(ctx, "lock refresh failed", "lock", l.name, "error", err)
			continue
		}
		l.mu.Lock()
		l.stored, l.started = now, start
		l.mu.Unlock()
	}
}

// watch aborts the operation once its lock object reaches the timeout, until
// keepAlive returns. It never waits on the backend, so it fires while a
// refresh hangs, also after release. It checks every lockCheckInterval
// because tickers stop while a host is suspended: after a resume only the
// next check sees the time slept on the wall clock.
func (l *repoLock) watch(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
		}
		if l.expire() {
			return
		}
	}
}

// expire aborts the operation and reports true when the stored lock object
// is the timeout old: by the wall clock, which counts the time a suspended
// host slept, or by the monotonic one since its write began.
func (l *repoLock) expire() bool {
	now := l.repo.lockNow()
	l.mu.Lock()
	since := max(now.Sub(l.stored), time.Since(l.started))
	l.mu.Unlock()
	if since < l.timeout {
		return false
	}
	l.abort(since)
	return true
}

// abort cancels the operation whose lock went without a refresh for since and
// lets release stop waiting for a refresh in flight. Only the first call
// counts, and it logs only for an operation neither its caller nor a release
// has ended.
func (l *repoLock) abort(since time.Duration) {
	l.abortOnce.Do(func() {
		close(l.expired)
		if l.ctx.Err() == nil {
			l.repo.logger.ErrorContext(l.ctx, "lock could not be refreshed, aborting operation",
				"lock", l.name, "since", since)
		}
		l.cancel()
	})
}

// release stops the refresh goroutine and removes the lock object. It runs
// after the operation, including a cancelled one, so it does not inherit the
// operation's context. A refresh that hangs is waited for only until the lock
// expires; keepAlive removes the object again should that refresh land later.
func (l *repoLock) release(ctx context.Context) {
	l.cancel()
	select {
	case <-l.done:
	case <-l.expired:
		l.mu.Lock()
		l.gaveUp = true
		l.mu.Unlock()
	}
	l.repo.reportLock(LockEvent{Kind: LockReleased, Exclusive: l.exclusive, LockID: l.name})
	l.repo.dropLock(ctx, l.name, l.exclusive)
}

// removeAbandoned deletes the lock object once a refresh returns that release
// gave up waiting for. Release has reported already and its caller may be
// gone, so a failure is only logged.
func (l *repoLock) removeAbandoned() {
	l.mu.Lock()
	gaveUp := l.gaveUp
	l.mu.Unlock()
	if gaveUp {
		l.repo.removeLock(l.ctx, l.name)
	}
}

// dropLock removes our lock object name and reports LockLeftOver when it
// stays behind.
func (r *Repository) dropLock(ctx context.Context, name string, exclusive bool) {
	if !r.removeLock(ctx, name) {
		r.reportLock(LockEvent{Kind: LockLeftOver, Exclusive: exclusive, LockID: name})
	}
}

// removeLock deletes the lock object name and reports whether it is gone.
// The delete outlives a cancelled operation but not lockDropTimeout, so a
// stalled backend cannot hold up the caller. A failure is only logged: the
// lock goes stale and Unlock removes it.
func (r *Repository) removeLock(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockDropTimeout)
	defer cancel()
	if err := r.backend.Delete(ctx, Handle{Type: LockFile, Name: name}); err != nil {
		r.logger.WarnContext(ctx, "failed to remove lock", "lock", name, "error", err)
		return false
	}
	return true
}

// Locks lists the lock objects in the repository, newest first, stale ones
// included.
func (r *Repository) Locks(ctx context.Context) ([]LockInfo, error) {
	locks, _, err := r.loadLocks(ctx)
	if err != nil {
		return nil, err
	}
	now := r.now()

	out := make([]LockInfo, 0, len(locks))
	for name, l := range locks {
		out = append(out, l.info(name, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// unlockConfig holds the options of an Unlock call.
type unlockConfig struct{ force bool }

// UnlockOption configures an Unlock call.
type UnlockOption func(*unlockConfig)

// WithForce makes Unlock remove every lock, not only the stale ones. It breaks
// locks that running operations still hold, so the caller has to know that no
// such operation exists.
func WithForce() UnlockOption { return func(c *unlockConfig) { c.force = true } }

// Unlock removes stale locks and returns how many it removed.
// Pass WithForce to remove every lock, including those that do not open.
func (r *Repository) Unlock(ctx context.Context, opts ...UnlockOption) (int, error) {
	cfg := &unlockConfig{}
	for _, o := range opts {
		o(cfg)
	}

	locks, unreadable, err := r.loadLocks(ctx)
	if err != nil {
		return 0, err
	}
	now := r.now()

	var names []string
	for name, l := range locks {
		if cfg.force || l.stale(now) {
			names = append(names, name)
		}
	}
	if cfg.force {
		names = append(names, unreadable...)
	}
	for i, name := range names {
		if err := r.backend.Delete(ctx, Handle{Type: LockFile, Name: name}); err != nil {
			return i, fmt.Errorf("lock: failed to remove %s: %w", name, err)
		}
	}
	return len(names), nil
}
