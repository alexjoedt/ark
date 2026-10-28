package ark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"syscall"
	"time"
)

// Lock timings. A lock is refreshed every lockRefreshInterval
// and counts as stale once it is lockStaleAge old, so a holder that stops
// refreshing is reclaimed after five missed refreshes. An operation whose own
// lock has not refreshed within lockRefreshTimeout gives up rather than keep
// writing under a lock others may already treat as stale.
const (
	lockStaleAge       = 30 * time.Minute
	lockRefreshTimeout = 22*time.Minute + 30*time.Second
)

// lockRefreshInterval is how often a held lock rewrites its object. Like
// lockSettleDelay it is a variable so the test binary can shorten it; nothing
// outside the package can.
var lockRefreshInterval = 5 * time.Minute

// lockSettleDelay is the pause between publishing our own lock and re-reading
// the namespace, long enough for a concurrent writer's Put to become visible.
// It is a variable so the test binary can shorten it; nothing outside the
// package can.
var lockSettleDelay = 200 * time.Millisecond

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
// the repository forever. Those are returned by name for Unlock WithForce.
func (r *Repository) loadLocks(ctx context.Context) (map[string]lockObject, []string, error) {
	var names []string
	err := r.backend.List(ctx, LockFile, func(info BlobInfo) error {
		if info.Size == 0 {
			return nil
		}
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("lock: failed to list locks: %w", err)
	}

	locks := make(map[string]lockObject, len(names))
	var unreadable []string
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

// conflict returns the lock that blocks a new one of this class, or nil when
// none does. An exclusive lock needs an empty namespace; a shared lock only
// needs no exclusive holder. Stale locks are ignored, ours is skipped by name.
func conflict(locks map[string]lockObject, own string, exclusive bool, now time.Time) *LockInfo {
	names := make([]string, 0, len(locks))
	for name := range locks {
		names = append(names, name)
	}
	sort.Strings(names) // a stable answer beats whichever the map yields first

	for _, name := range names {
		if name == own {
			continue
		}
		l := locks[name]
		if !exclusive && !l.Exclusive {
			continue
		}
		if l.stale(now) {
			continue
		}
		info := l.info(name, now)
		return &info
	}
	return nil
}

// lockedError wraps ErrRepoLocked with the holder's identity, so an operator
// reading the message knows which process to look at.
func lockedError(h LockInfo, now time.Time) error {
	kind := "shared"
	if h.Exclusive {
		kind = "exclusive"
	}
	return fmt.Errorf("%w: %s lock held by %s pid %d for %s",
		ErrRepoLocked, kind, h.Hostname, h.PID, now.Sub(h.Time).Round(time.Second))
}

// repoLock is a held lock. Its refresh goroutine keeps the object young and
// cancels the operation's context when it cannot.
type repoLock struct {
	repo   *Repository
	name   string
	cancel context.CancelFunc
	done   chan struct{}
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
		if !errors.Is(err, ErrRepoLocked) || !r.now().Before(deadline) {
			return nil, nil, err
		}
		if waitErr := sleepCtx(ctx, lockBackoff(attempt)); waitErr != nil {
			return nil, nil, waitErr
		}
	}
}

// Backoff bounds for a waiting acquire, so a queue of waiters spreads out
// instead of hammering the backend.
const (
	lockBackoffFloor = 500 * time.Millisecond
	lockBackoffMax   = 8 * time.Second
)

// lockBackoff doubles the wait with every failed attempt, up to lockBackoffMax.
// The shift is capped so a long wait never overflows into a zero or negative
// delay.
func lockBackoff(attempt int) time.Duration {
	return min(lockBackoffFloor<<min(attempt, 16), lockBackoffMax)
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

func (r *Repository) tryAcquire(ctx context.Context, exclusive bool) (context.Context, *repoLock, error) {
	now := r.now()

	locks, unreadable, err := r.loadLocks(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range unreadable {
		r.logger.WarnContext(ctx, "ignoring a lock that does not open, Unlock with WithForce removes it", "lock", name)
	}
	if held := conflict(locks, "", exclusive, now); held != nil {
		return nil, nil, lockedError(*held, now)
	}

	name := generateID(now)
	if err := r.putLock(ctx, name, newLockObject(now, exclusive)); err != nil {
		return nil, nil, err
	}

	if err := sleepCtx(ctx, lockSettleDelay); err != nil {
		r.removeLock(context.WithoutCancel(ctx), name)
		return nil, nil, err
	}

	locks, _, err = r.loadLocks(ctx)
	if err != nil {
		r.removeLock(context.WithoutCancel(ctx), name)
		return nil, nil, err
	}
	if held := conflict(locks, name, exclusive, r.now()); held != nil {
		r.removeLock(context.WithoutCancel(ctx), name)
		return nil, nil, lockedError(*held, r.now())
	}

	lockCtx, cancel := context.WithCancel(ctx)
	l := &repoLock{repo: r, name: name, cancel: cancel, done: make(chan struct{})}
	go l.keepAlive(lockCtx, exclusive)
	return lockCtx, l, nil
}

// keepAlive rewrites the lock object every lockRefreshInterval. When no
// refresh has succeeded for lockRefreshTimeout it cancels the operation: past
// that point another process may consider the lock stale and prune underneath
// it.
func (l *repoLock) keepAlive(ctx context.Context, exclusive bool) {
	defer close(l.done)

	t := time.NewTicker(lockRefreshInterval)
	defer t.Stop()

	lastOK := l.repo.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		now := l.repo.now()
		if err := l.repo.putLock(ctx, l.name, newLockObject(now, exclusive)); err != nil {
			l.repo.logger.WarnContext(ctx, "lock refresh failed", "lock", l.name, "error", err)
			if now.Sub(lastOK) >= lockRefreshTimeout {
				l.repo.logger.ErrorContext(ctx, "lock could not be refreshed, aborting operation",
					"lock", l.name, "since", now.Sub(lastOK))
				l.cancel()
				return
			}
			continue
		}
		lastOK = now
	}
}

// release stops the refresh goroutine and removes the lock object. It runs
// after the operation, including a cancelled one, so it does not inherit the
// operation's context.
func (l *repoLock) release(ctx context.Context) {
	l.cancel()
	<-l.done
	l.repo.removeLock(context.WithoutCancel(ctx), l.name)
}

func (r *Repository) removeLock(ctx context.Context, name string) {
	if err := r.backend.Delete(ctx, Handle{Type: LockFile, Name: name}); err != nil {
		r.logger.WarnContext(ctx, "failed to remove lock", "lock", name, "error", err)
	}
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
