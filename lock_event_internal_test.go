package ark

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

type lockEvents struct {
	mu  sync.Mutex
	got []LockEvent
}

func (e *lockEvents) add(ev LockEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
}

func (e *lockEvents) take() []LockEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	got := e.got
	e.got = nil
	return got
}

// eventKinds renders events as kind/class pairs for a readable diff.
func eventKinds(evs []LockEvent) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Kind) + " " + lockClass(ev.Exclusive)
	}
	return out
}

func wantKinds(t *testing.T, got []LockEvent, want ...string) {
	t.Helper()
	if !slices.Equal(eventKinds(got), want) {
		t.Fatalf("events %v, want %v", eventKinds(got), want)
	}
}

// noLockDeletes fails every Delete of a lock object.
type noLockDeletes struct{ *stubBackend }

func (b noLockDeletes) Delete(ctx context.Context, h Handle) error {
	if h.Type == LockFile {
		return errors.New("backend is down")
	}
	return b.stubBackend.Delete(ctx, h)
}

// cancelOnLockPut cancels the operation once its lock object is written, so
// the attempt aborts during the settle delay.
type cancelOnLockPut struct {
	noLockDeletes
	cancel context.CancelFunc
}

func (b cancelOnLockPut) Put(ctx context.Context, h Handle, r io.Reader) error {
	err := b.noLockDeletes.Put(ctx, h, r)
	if h.Type == LockFile {
		b.cancel()
	}
	return err
}

func TestLockEventSharedAcquiredThenReleased(t *testing.T) {
	ctx := context.Background()
	rec := &lockEvents{}
	repo, b, _ := lockRepo(t, WithLockEvent(rec.add))

	_, held, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
	names := lockNames(t, b)
	held.release(ctx)

	got := rec.take()
	wantKinds(t, got, "acquired shared", "released shared")
	if len(names) != 1 || got[0].LockID != names[0] || got[1].LockID != names[0] {
		t.Errorf("event lock IDs %q, %q, want the held object %v", got[0].LockID, got[1].LockID, names)
	}
	for _, ev := range got {
		if ev.Holders != nil {
			t.Errorf("%s event carries holders %v, want nil", ev.Kind, ev.Holders)
		}
	}
	if names := lockNames(t, b); len(names) != 0 {
		t.Errorf("locks after release: %v", names)
	}
}

func TestLockEventRejectedExclusiveListsEveryHolder(t *testing.T) {
	ctx := context.Background()
	rec := &lockEvents{}
	repo, b, _ := lockRepo(t, WithLockEvent(rec.add))

	for range 2 {
		_, held, err := repo.acquire(ctx, false)
		if err != nil {
			t.Fatalf("shared lock: %v", err)
		}
		defer held.release(ctx)
	}
	holders := lockNames(t, b)
	slices.Sort(holders)

	if _, _, err := repo.acquire(ctx, true); !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("exclusive under shared locks: %v, want ErrRepoLocked", err)
	}

	got := rec.take()
	wantKinds(t, got, "acquired shared", "acquired shared", "rejected exclusive")
	rej := got[2]
	var ids []string
	for _, h := range rej.Holders {
		ids = append(ids, h.ID)
		if h.Exclusive {
			t.Errorf("holder %s reported exclusive", h.ID)
		}
	}
	if !slices.Equal(ids, holders) {
		t.Errorf("rejected holders %v, want %v", ids, holders)
	}
	if rej.LockID != "" {
		t.Errorf("rejection before writing reports lock ID %q, want empty", rej.LockID)
	}
}

func TestLockEventLeftOverAfterAFailedDelete(t *testing.T) {
	ctx := context.Background()
	rec := &lockEvents{}
	repo, b, _ := lockRepo(t, WithLockEvent(rec.add))
	repo.backend = noLockDeletes{b}

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	held.release(ctx)

	got := rec.take()
	wantKinds(t, got, "acquired exclusive", "released exclusive", "left-over exclusive")
	if got[2].LockID != got[0].LockID || got[2].LockID == "" {
		t.Errorf("left-over lock ID %q, want the acquired %q", got[2].LockID, got[0].LockID)
	}
}

func TestLockEventLeftOverAfterAnAbortedAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &lockEvents{}
	repo, b, _ := lockRepo(t, WithLockEvent(rec.add))
	repo.backend = cancelOnLockPut{noLockDeletes{b}, cancel}

	if _, _, err := repo.acquire(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire cancelled while settling: %v, want context.Canceled", err)
	}
	got := rec.take()
	wantKinds(t, got, "left-over exclusive")
	if names := lockNames(t, b); len(names) != 1 || got[0].LockID != names[0] {
		t.Errorf("left-over lock ID %q, want the object left behind %v", got[0].LockID, names)
	}
}

func TestLockEventEveryWaitAttemptIsRejected(t *testing.T) {
	ctx := context.Background()
	rec := &lockEvents{}
	repo, b, clock := lockRepo(t, WithLockEvent(rec.add))

	_, held, err := repo.acquire(ctx, true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	defer held.release(ctx)
	holder := lockNames(t, b)
	rec.take()

	// A rejected attempt reads the clock three times (attempt, listing, time
	// left), so with a one second step the first leaves 1ms to back off and
	// the second runs past the deadline.
	clock.setStep(time.Second)
	repo.lockWait = 3*time.Second + time.Millisecond

	if _, _, err := repo.acquire(ctx, false); !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("acquire under an exclusive lock: %v, want ErrRepoLocked", err)
	}
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("events %v, want one rejection per attempt, two", eventKinds(got))
	}
	for _, ev := range got {
		if ev.Kind != LockRejected || len(ev.Holders) != 1 || ev.Holders[0].ID != holder[0] {
			t.Errorf("event %s holders %v, want rejected by %v", ev.Kind, ev.Holders, holder)
		}
	}
}
