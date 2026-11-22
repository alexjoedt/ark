package ark_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

var errLockReadDown = errors.New("lock read failed in transport")

// lockReadFails fails every Get of a lock object while armed, the way a
// backend whose connection drops does, and counts the lock Gets it sees. With
// armOnPut it arms itself once a lock object is stored.
type lockReadFails struct {
	ark.Backend
	armed, armOnPut atomic.Bool
	gets            atomic.Int32
}

func (b *lockReadFails) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type == ark.LockFile {
		b.gets.Add(1)
		if b.armed.Load() {
			return nil, errLockReadDown
		}
	}
	return b.Backend.Get(ctx, h)
}

func (b *lockReadFails) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	err := b.Backend.Put(ctx, h, r)
	if h.Type == ark.LockFile && b.armOnPut.Load() {
		b.armed.Store(true)
	}
	return err
}

// prunable stores two snapshots and forgets the first, so a prune has
// something to delete.
func prunable(t *testing.T, b ark.Backend) *ark.Repository {
	t.Helper()
	repo := openRepo(t, b)
	first := mustBackup(t, repo, smallFiles("one", 4))
	mustBackup(t, repo, smallFiles("two", 4))
	if _, err := repo.Forget(context.Background(), ark.ForgetPolicy{},
		ark.SnapshotFilter{IDs: []ark.SnapshotID{first.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	return repo
}

// stored names every object outside locks/.
func stored(t *testing.T, b ark.Backend) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, typ := range []ark.HandleType{
		ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile, ark.SnapshotFile,
	} {
		out[typ.String()] = slices.Sorted(slices.Values(objectNames(t, b, typ)))
	}
	return out
}

func openOn(t *testing.T, b ark.Backend, opts ...ark.Option) *ark.Repository {
	t.Helper()
	repo, err := ark.Open(context.Background(), b, testPassword, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// TestPruneStopsWhenALockDoesNotRead: a lock that fails to read may be the
// shared lock of a running reader. A prune that passed over it would delete
// what the reader needs, so it stops and deletes nothing.
func TestPruneStopsWhenALockDoesNotRead(t *testing.T) {
	ctx := context.Background()
	b := &lockReadFails{Backend: arkmem.New()}
	repo := prunable(t, b)
	rd, err := openOn(t, b).OpenReader(ctx)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer rd.Close()

	before := stored(t, b)
	b.armed.Store(true)
	if _, err := repo.Prune(ctx); !errors.Is(err, errLockReadDown) {
		t.Fatalf("Prune beside a lock that fails to read = %v, want the read error", err)
	}
	b.armed.Store(false)
	if after := stored(t, b); !maps.EqualFunc(before, after, slices.Equal) {
		t.Errorf("objects after the stopped prune = %v, want %v", after, before)
	}
	if locks := objectNames(t, b, ark.LockFile); len(locks) != 1 {
		t.Errorf("locks after the stopped prune = %v, want only the reader's", locks)
	}
}

// TestReaderStopsWhenTheExclusiveLockDoesNotRead: a shared lock is never
// taken beside an exclusive one that could not be read.
func TestReaderStopsWhenTheExclusiveLockDoesNotRead(t *testing.T) {
	ctx := context.Background()
	b := &lockReadFails{Backend: arkmem.New()}
	repo := prunable(t, b)

	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	pruner := openOn(t, b, ark.WithLockEvent(func(ev ark.LockEvent) {
		if ev.Kind == ark.LockAcquired && ev.Exclusive {
			once.Do(func() { close(parked) })
			<-release
		}
	}))
	pruned := make(chan error, 1)
	go func() {
		_, err := pruner.Prune(ctx)
		pruned <- err
	}()
	<-parked

	b.armed.Store(true)
	rd, err := repo.OpenReader(ctx)
	b.armed.Store(false)
	close(release)
	if err == nil {
		_ = rd.Close()
	}
	if !errors.Is(err, errLockReadDown) {
		t.Errorf("OpenReader beside an exclusive lock that fails to read = %v, want the read error", err)
	}
	if err := <-pruned; err != nil {
		t.Errorf("Prune: %v", err)
	}
}

// TestReadErrorStopsALockWait: a read error is not a conflict, so a wait for
// the lock does not retry it.
func TestReadErrorStopsALockWait(t *testing.T) {
	ctx := context.Background()
	b := &lockReadFails{Backend: arkmem.New()}
	prunable(t, b)
	rd, err := openOn(t, b).OpenReader(ctx)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer rd.Close()

	repo := openOn(t, b, ark.WithLockWait(time.Minute))
	b.gets.Store(0)
	b.armed.Store(true)
	start := time.Now()
	_, err = repo.Prune(ctx)
	b.armed.Store(false)
	if !errors.Is(err, errLockReadDown) || errors.Is(err, ark.ErrRepoLocked) {
		t.Fatalf("Prune with a lock wait = %v, want the read error", err)
	}
	if n := b.gets.Load(); n != 1 {
		t.Errorf("Prune read a lock %d times, want 1: no retry", n)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Prune returned after %s, want at once", d)
	}
}

// TestReadErrorAfterTheOwnLockDeletesIt: a read error in the second listing
// stops the acquisition, and the lock written before it goes, as for one
// that was refused.
func TestReadErrorAfterTheOwnLockDeletesIt(t *testing.T) {
	ctx := context.Background()
	b := &lockReadFails{Backend: arkmem.New()}
	repo := openRepo(t, b)
	b.armOnPut.Store(true)
	_, err := repo.Backup(ctx, smallFiles("app", 2))
	b.armOnPut.Store(false)
	b.armed.Store(false)
	if !errors.Is(err, errLockReadDown) {
		t.Fatalf("Backup = %v, want the read error of the second listing", err)
	}
	if locks := objectNames(t, b, ark.LockFile); len(locks) != 0 {
		t.Errorf("locks left after the stopped acquisition: %v", locks)
	}
}

// goneLock lists a lock that a Get no longer finds, as one released between
// the listing and the read.
type goneLock struct{ ark.Backend }

func (b goneLock) List(ctx context.Context, typ ark.HandleType, fn func(ark.BlobInfo) error) error {
	if typ == ark.LockFile {
		if err := fn(ark.BlobInfo{Name: "released-since-the-listing", Size: 100}); err != nil {
			return err
		}
	}
	return b.Backend.List(ctx, typ, fn)
}

func TestLockGoneBeforeItsReadIsPassedOver(t *testing.T) {
	ctx := context.Background()
	b := goneLock{arkmem.New()}
	repo := prunable(t, b)
	if _, err := repo.Prune(ctx); err != nil {
		t.Errorf("Prune beside a lock released since the listing: %v", err)
	}
}

// TestLockWithUndecodableJSONIsPassedOver: a lock that opens and whose JSON
// does not decode guards nothing anyone can name; it is passed over with a
// warning, as one whose tag does not verify.
func TestLockWithUndecodableJSONIsPassedOver(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	openRepo(t, b)
	var logs bytes.Buffer
	repo := openOn(t, b, ark.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	h := ark.Handle{Type: ark.LockFile, Name: "garbled"}
	if err := ark.PutObject(ctx, repo, h, []byte(`{"exclusive":tru`)); err != nil {
		t.Fatalf("store the lock: %v", err)
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune beside a lock whose JSON does not decode: %v", err)
	}
	if !strings.Contains(logs.String(), "lock=garbled") {
		t.Errorf("no warning names the garbled lock, logs:\n%s", logs.String())
	}
}
