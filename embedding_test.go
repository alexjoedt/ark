package ark_test

import (
	"bytes"
	"context"
	"io"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// callCounter counts calls and bytes per method beneath an InstrumentedBackend,
// the reference its Stats are held against.
type callCounter struct {
	ark.Backend
	put, get, getRange, exists, del, list, stat, sync atomic.Int64
	putBytes, getBytes, getRangeBytes                 atomic.Int64
}

type countedReader struct {
	io.ReadCloser
	n *atomic.Int64
}

func (r countedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.n.Add(int64(n))
	return n, err
}

func (c *callCounter) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	c.put.Add(1)
	return c.Backend.Put(ctx, h, countedReader{io.NopCloser(r), &c.putBytes})
}

func (c *callCounter) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	c.get.Add(1)
	rc, err := c.Backend.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	return countedReader{rc, &c.getBytes}, nil
}

func (c *callCounter) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	c.getRange.Add(1)
	data, err := c.Backend.GetRange(ctx, h, offset, length)
	c.getRangeBytes.Add(int64(len(data)))
	return data, err
}

func (c *callCounter) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	c.exists.Add(1)
	return c.Backend.Exists(ctx, h)
}

func (c *callCounter) Delete(ctx context.Context, h ark.Handle) error {
	c.del.Add(1)
	return c.Backend.Delete(ctx, h)
}

func (c *callCounter) Sync(ctx context.Context) error {
	c.sync.Add(1)
	return c.Backend.Sync(ctx)
}

func (c *callCounter) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	c.list.Add(1)
	return c.Backend.List(ctx, t, fn)
}

func (c *callCounter) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	c.stat.Add(1)
	return c.Backend.Stat(ctx, h)
}

func (c *callCounter) callsAndBytes() map[string][2]int64 {
	return map[string][2]int64{
		"Put":      {c.put.Load(), c.putBytes.Load()},
		"Get":      {c.get.Load(), c.getBytes.Load()},
		"GetRange": {c.getRange.Load(), c.getRangeBytes.Load()},
		"Exists":   {c.exists.Load(), 0},
		"Delete":   {c.del.Load(), 0},
		"Sync":     {c.sync.Load(), 0},
		"List":     {c.list.Load(), 0},
		"Stat":     {c.stat.Load(), 0},
	}
}

func statsCallsAndBytes(s ark.BackendStats) map[string][2]int64 {
	return map[string][2]int64{
		"Put":      {s.Put.Calls, s.Put.Bytes},
		"Get":      {s.Get.Calls, s.Get.Bytes},
		"GetRange": {s.GetRange.Calls, s.GetRange.Bytes},
		"Exists":   {s.Exists.Calls, s.Exists.Bytes},
		"Delete":   {s.Delete.Calls, s.Delete.Bytes},
		"Sync":     {s.Sync.Calls, s.Sync.Bytes},
		"List":     {s.List.Calls, s.List.Bytes},
		"Stat":     {s.Stat.Calls, s.Stat.Bytes},
	}
}

// opRecorder is a repository-wide observer that, unlike recorder, locks its
// state: calls of concurrent operations may overlap.
type opRecorder struct {
	mu      sync.Mutex
	events  []ark.ProgressEvent
	onEvent func(ark.ProgressEvent)
}

func (r *opRecorder) observe(e ark.ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if r.onEvent != nil {
		r.onEvent(e)
	}
}

func (r *opRecorder) take() []ark.ProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.events
	r.events = nil
	return got
}

// checkOp asserts that op, among interleaved events, reported at least one
// event before its finalize and follows the finalize rule.
func checkOp(t *testing.T, events []ark.ProgressEvent, op ark.Operation) {
	t.Helper()
	var phases []ark.Phase
	for _, e := range events {
		if e.Operation == op {
			phases = append(phases, e.Phase)
		}
	}
	if len(phases) < 2 {
		t.Errorf("%s: want events before the finalize, got %v", op, phases)
		return
	}
	checkFinalize(t, op, phases, true)
}

// noClose shares a backend with a second handle whose Close must not close it.
type noClose struct{ ark.Backend }

func (noClose) Close() error { return nil }

// gateSource is a stubSource whose Walk waits for release first.
type gateSource struct {
	stubSource
	release <-chan struct{}
}

func (s gateSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return context.DeadlineExceeded
	}
	return s.stubSource.Walk(ctx, fn)
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestEmbeddingObservesEveryOperation is the embedder's view of the hooks: one
// handle opened with WithProgress, WithLockEvent and an instrumented backend
// runs every operation through the public API alone.
func TestEmbeddingObservesEveryOperation(t *testing.T) {
	t.Run("every operation", func(t *testing.T) {
		ctx := context.Background()
		counter := &callCounter{Backend: arkmem.New()}
		ib := ark.Instrument(counter)
		if err := ark.Init(ctx, ib, testPassword); err != nil {
			t.Fatalf("Init: %v", err)
		}

		var lockMu sync.Mutex
		var lockEvents []ark.LockEvent
		release := make(chan struct{})
		var releaseOnce sync.Once
		rec := &opRecorder{}
		repo, err := ark.Open(ctx, ib, testPassword,
			ark.WithProgress(rec.observe),
			// Far above the holder's tiny backup, so a slow box still
			// sees the prune through instead of a lock error.
			ark.WithLockWait(time.Minute),
			ark.WithLockEvent(func(e ark.LockEvent) {
				lockMu.Lock()
				lockEvents = append(lockEvents, e)
				lockMu.Unlock()
				if e.Kind == ark.LockRejected {
					releaseOnce.Do(func() { close(release) })
				}
			}))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = repo.Close() })

		blob := payload(1) + payload(2)
		snap, err := repo.Backup(ctx, ark.NewStreamSource("dump", strings.NewReader(blob)))
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		checkOp(t, rec.take(), ark.OperationBackup)

		var restored bytes.Buffer
		if _, err := repo.Restore(ctx, snap.ID, ark.NewStreamSink(&restored)); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		events := rec.take()
		checkOp(t, events, ark.OperationRestore)
		if last := events[len(events)-1]; last.FilesTotal == 0 || last.FilesDone != last.FilesTotal ||
			last.BytesDone != last.BytesTotal || last.ChunksTotal == 0 || last.ChunksDone != last.ChunksTotal {
			t.Errorf("restore finalize %+v, want the totals reached", last)
		}
		if restored.String() != blob {
			t.Errorf("stream restored %d bytes, not the %d backed up", restored.Len(), len(blob))
		}

		next, err := repo.Backup(ctx, ark.NewStreamSource("dump", strings.NewReader(payload(3))))
		if err != nil {
			t.Fatalf("second Backup: %v", err)
		}
		rec.take()
		if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID}}); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		checkOp(t, rec.take(), ark.OperationForget)

		// A second handle on the same backend holds a shared lock until the
		// prune has been rejected once.
		held := make(chan struct{})
		var heldOnce sync.Once
		holder, err := ark.Open(ctx, noClose{ib}, testPassword, ark.WithLockEvent(func(e ark.LockEvent) {
			if e.Kind == ark.LockAcquired {
				heldOnce.Do(func() { close(held) })
			}
		}))
		if err != nil {
			t.Fatalf("Open holder: %v", err)
		}
		t.Cleanup(func() { _ = holder.Close() })
		holderDone := make(chan error, 1)
		go func() {
			_, err := holder.Backup(ctx, gateSource{stubSource{name: "app", content: "held"}, release})
			holderDone <- err
		}()
		waitFor(t, held, "the holder's shared lock")
		lockMu.Lock()
		lockEvents = nil
		lockMu.Unlock()

		if _, err := repo.Prune(ctx); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		checkOp(t, rec.take(), ark.OperationPrune)
		if err := <-holderDone; err != nil {
			t.Fatalf("holder Backup: %v", err)
		}
		lockMu.Lock()
		pruneLocks := lockEvents
		lockMu.Unlock()
		rejected, acquired := -1, -1
		for i, e := range pruneLocks {
			switch {
			case e.Kind == ark.LockRejected && rejected < 0:
				rejected = i
				if !e.Exclusive || len(e.Holders) == 0 || e.Holders[0].Exclusive {
					t.Errorf("rejected event %+v, want an exclusive attempt blocked by a shared holder", e)
				}
			case e.Kind == ark.LockAcquired && acquired < 0:
				acquired = i
			}
		}
		if rejected < 0 || acquired < rejected {
			t.Errorf("prune lock events %+v, want a rejection before the acquire", pruneLocks)
		}

		if _, err := repo.Check(ctx); err != nil {
			t.Fatalf("Check: %v", err)
		}
		checkOp(t, rec.take(), ark.OperationCheck)
		if _, err := repo.Verify(ctx, next.ID); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		checkOp(t, rec.take(), ark.OperationVerify)

		want, got := counter.callsAndBytes(), statsCallsAndBytes(ib.Stats())
		if !maps.Equal(got, want) {
			t.Errorf("instrumented calls and bytes %v, want the counted %v", got, want)
		}
	})

	t.Run("backup and check concurrently", func(t *testing.T) {
		ctx := context.Background()
		b := arkmem.New()
		if err := ark.Init(ctx, b, testPassword); err != nil {
			t.Fatalf("Init: %v", err)
		}
		rec := &opRecorder{}
		repo, err := ark.Open(ctx, b, testPassword, ark.WithProgress(rec.observe))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		rec.take()

		// The backup reports its scan, then waits inside Walk until the check
		// has reported, so the two operations' events interleave.
		scanning, checking := make(chan struct{}), make(chan struct{})
		var scanOnce, checkOnce sync.Once
		rec.mu.Lock()
		rec.onEvent = func(e ark.ProgressEvent) {
			if e.Operation == ark.OperationBackup {
				scanOnce.Do(func() { close(scanning) })
			}
			if e.Operation == ark.OperationCheck {
				checkOnce.Do(func() { close(checking) })
			}
		}
		rec.mu.Unlock()

		backupDone := make(chan error, 1)
		go func() {
			_, err := repo.Backup(ctx, gateSource{stubSource{name: "app", content: "two"}, checking})
			backupDone <- err
		}()
		waitFor(t, scanning, "the backup's first event")
		if _, err := repo.Check(ctx); err != nil {
			t.Fatalf("Check: %v", err)
		}
		if err := <-backupDone; err != nil {
			t.Fatalf("Backup: %v", err)
		}

		events := rec.take()
		checkOp(t, events, ark.OperationBackup)
		checkOp(t, events, ark.OperationCheck)
		firstCheck, backupFinal := -1, -1
		for i, e := range events {
			if e.Operation == ark.OperationCheck && firstCheck < 0 {
				firstCheck = i
			}
			if e.Operation == ark.OperationBackup && e.Phase == ark.PhaseFinalize {
				backupFinal = i
			}
		}
		if events[0].Operation != ark.OperationBackup || firstCheck < 0 || backupFinal < firstCheck {
			t.Errorf("want check events inside the backup's run, got %v", events)
		}
	})
}
