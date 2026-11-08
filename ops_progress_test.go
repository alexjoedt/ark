package ark_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// ioBackend counts every backend call and the Syncs among them.
type ioBackend struct {
	ark.Backend
	calls, syncs atomic.Int64
}

func (b *ioBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	b.calls.Add(1)
	return b.Backend.Put(ctx, h, r)
}

func (b *ioBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	b.calls.Add(1)
	return b.Backend.Get(ctx, h)
}

func (b *ioBackend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	b.calls.Add(1)
	return b.Backend.Exists(ctx, h)
}

func (b *ioBackend) Delete(ctx context.Context, h ark.Handle) error {
	b.calls.Add(1)
	return b.Backend.Delete(ctx, h)
}

func (b *ioBackend) Sync(ctx context.Context) error {
	b.calls.Add(1)
	b.syncs.Add(1)
	return b.Backend.Sync(ctx)
}

func (b *ioBackend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	b.calls.Add(1)
	return b.Backend.List(ctx, t, fn)
}

func (b *ioBackend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	b.calls.Add(1)
	return b.Backend.Stat(ctx, h)
}

// observed is one event plus the backend counters at the moment it arrived.
type observed struct {
	ark.ProgressEvent
	calls, syncs int64
}

// recorder is a repository-wide observer without a lock: overlapping calls
// trip the inFlight check and, under -race, the race detector.
type recorder struct {
	b        *ioBackend
	events   []observed
	inFlight atomic.Int32
	overlap  atomic.Bool
}

func (r *recorder) observe(e ark.ProgressEvent) {
	if r.inFlight.Add(1) > 1 {
		r.overlap.Store(true)
	}
	defer r.inFlight.Add(-1)
	r.events = append(r.events, observed{e, r.b.calls.Load(), r.b.syncs.Load()})
}

// take returns the events since the last take and checks the invariants every
// operation shares: its own Operation on every event, the first one before
// any I/O, no overlapping calls and finalize exactly once, last, on success.
func (r *recorder) take(t *testing.T, op ark.Operation, succeeded bool) []observed {
	t.Helper()
	got := r.events
	r.events = nil
	if r.overlap.Load() {
		t.Errorf("%s: observer calls overlapped", op)
	}
	if len(got) == 0 {
		t.Fatalf("%s: no events", op)
	}
	if got[0].calls != 0 {
		t.Errorf("%s: first event after %d backend calls, want before any I/O", op, got[0].calls)
	}
	finals := 0
	for i, e := range got {
		if e.Operation != op {
			t.Errorf("%s: event %d carries operation %q", op, i, e.Operation)
		}
		if e.Phase == ark.PhaseFinalize {
			finals++
		}
	}
	switch {
	case !succeeded && finals != 0:
		t.Errorf("%s: failed operation emitted %d finalize events", op, finals)
	case succeeded && (finals != 1 || got[len(got)-1].Phase != ark.PhaseFinalize):
		t.Errorf("%s: want exactly one finalize event, last; got %v", op, phaseList(got))
	}
	return got
}

func phaseList(events []observed) []ark.Phase {
	out := make([]ark.Phase, len(events))
	for i, e := range events {
		out[i] = e.Phase
	}
	return out
}

// openRecorded opens a repository with two snapshots whose observer is a
// recorder, and resets the backend counters so each operation starts at zero.
func openRecorded(t *testing.T) (*ark.Repository, *recorder, []*ark.Snapshot) {
	t.Helper()
	ctx := context.Background()
	b := &ioBackend{Backend: arkmem.New()}
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rec := &recorder{b: b}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithProgress(rec.observe))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var snaps []*ark.Snapshot
	for _, content := range []string{"one", "two"} {
		s, err := repo.Backup(ctx, stubSource{name: "app", content: content})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		snaps = append(snaps, s)
	}
	rec.reset()
	return repo, rec, snaps
}

func (r *recorder) reset() {
	r.events = nil
	r.b.calls.Store(0)
	r.b.syncs.Store(0)
}

func TestForgetReportsProgress(t *testing.T) {
	for _, dry := range []bool{false, true} {
		repo, rec, snaps := openRecorded(t)
		var opts []ark.ForgetOption
		if dry {
			opts = append(opts, ark.WithForgetDryRun())
		}
		_, err := repo.Forget(context.Background(), ark.ForgetPolicy{},
			ark.SnapshotFilter{IDs: []string{snaps[0].ID}}, opts...)
		if err != nil {
			t.Fatalf("Forget (dry %v): %v", dry, err)
		}
		got := rec.take(t, ark.OperationForget, true)
		if p := phaseList(got); len(p) != 2 || p[0] != ark.PhaseLoad {
			t.Errorf("dry %v: phases %v, want load then finalize", dry, p)
		}
		if last := got[len(got)-1]; last.FilesDone != 1 || last.FilesTotal != 1 {
			t.Errorf("dry %v: finalize reports %d/%d manifests, want 1/1", dry, last.FilesDone, last.FilesTotal)
		}
	}
}

func TestPruneReportsProgress(t *testing.T) {
	for _, dry := range []bool{false, true} {
		repo, rec, snaps := openRecorded(t)
		ctx := context.Background()
		if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snaps[0].ID}}); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		rec.reset()

		var opts []ark.PruneOption
		if dry {
			opts = append(opts, ark.WithPruneDryRun())
		}
		res, err := repo.Prune(ctx, opts...)
		if err != nil {
			t.Fatalf("Prune (dry %v): %v", dry, err)
		}
		if res.BlobsRemoved == 0 {
			t.Fatalf("dry %v: prune found nothing to remove, the test needs orphans", dry)
		}
		got := rec.take(t, ark.OperationPrune, true)
		if got[0].Phase != ark.PhaseLoad {
			t.Errorf("dry %v: first phase %q, want load", dry, got[0].Phase)
		}
		sweeps := got[1 : len(got)-1]
		if len(sweeps) == 0 {
			t.Fatalf("dry %v: no sweep events, got %v", dry, phaseList(got))
		}
		total := sweeps[0].ChunksTotal
		for i, e := range sweeps {
			if e.Phase != ark.PhaseSweep || e.ChunksDone != i || e.ChunksTotal != total {
				t.Errorf("dry %v: sweep event %d = %s %d/%d, want sweep %d/%d",
					dry, i, e.Phase, e.ChunksDone, e.ChunksTotal, i, total)
			}
		}
		if len(sweeps) != total+1 || total == 0 {
			t.Errorf("dry %v: %d sweep events for %d objects", dry, len(sweeps), total)
		}
		final := got[len(got)-1]
		if !dry && final.syncs == 0 {
			t.Error("finalize arrived before the deletions were synced")
		}
	}
}

func TestVerifyReportsProgress(t *testing.T) {
	repo, rec, snaps := openRecorded(t)
	if _, err := repo.Verify(context.Background(), snaps[1].ID[:12]); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := rec.take(t, ark.OperationVerify, true)
	if got[0].Phase != ark.PhaseLoad {
		t.Errorf("first phase %q, want load", got[0].Phase)
	}
	fetches := got[1 : len(got)-1]
	if len(fetches) == 0 {
		t.Fatalf("no fetch events, got %v", phaseList(got))
	}
	for i, e := range fetches {
		if e.Phase != ark.PhaseFetch || e.ChunksDone != i+1 || e.ChunksTotal != len(fetches) {
			t.Errorf("fetch event %d = %s %d/%d, want fetch %d/%d",
				i, e.Phase, e.ChunksDone, e.ChunksTotal, i+1, len(fetches))
		}
	}
	if got[0].CurrentPath != "" {
		t.Errorf("load event carries CurrentPath %q, want none before the prefix is resolved", got[0].CurrentPath)
	}
	for _, e := range got[1:] {
		if e.CurrentPath != snaps[1].ID {
			t.Errorf("%s event carries CurrentPath %q, want the resolved snapshot ID", e.Phase, e.CurrentPath)
		}
	}
}

func TestCheckReportsProgress(t *testing.T) {
	repo, rec, snaps := openRecorded(t)
	if _, err := repo.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got := rec.take(t, ark.OperationCheck, true)
	if got[0].Phase != ark.PhaseLoad {
		t.Errorf("first phase %q, want load", got[0].Phase)
	}
	if final := got[len(got)-1]; final.FilesDone != len(snaps) || final.FilesTotal != len(snaps) {
		t.Errorf("finalize reports %d/%d snapshots, want %d", final.FilesDone, final.FilesTotal, len(snaps))
	}

	// Per snapshot: its verify's fetch events with chunk counters starting
	// at one, then one event counting it as checked.
	checked := 0
	var current string
	for _, e := range got[1 : len(got)-1] {
		if e.Phase != ark.PhaseFetch || e.CurrentPath == "" || e.FilesTotal != len(snaps) {
			t.Fatalf("check event %s %q %d/%d, want fetch with a snapshot ID", e.Phase, e.CurrentPath, e.FilesDone,
				e.FilesTotal)
		}
		if e.ChunksTotal == 0 {
			checked++
			if e.CurrentPath != current || e.FilesDone != checked {
				t.Errorf("snapshot %q counted as %d checked after verifying %q", e.CurrentPath, e.FilesDone, current)
			}
			continue
		}
		if e.CurrentPath != current && e.ChunksDone != 1 {
			t.Errorf("snapshot %q starts at chunk %d, want 1", e.CurrentPath, e.ChunksDone)
		}
		if e.FilesDone != checked {
			t.Errorf("inner verify event reports %d checked, want %d", e.FilesDone, checked)
		}
		current = e.CurrentPath
	}
	if checked != len(snaps) {
		t.Errorf("%d snapshots counted as checked, want %d", checked, len(snaps))
	}
}

func TestFailedOperationsReportNoFinalize(t *testing.T) {
	repo, rec, snaps := openRecorded(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ops := []struct {
		op  ark.Operation
		run func() error
	}{
		{ark.OperationForget, func() error {
			_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snaps[0].ID}})
			return err
		}},
		{ark.OperationPrune, func() error { _, err := repo.Prune(ctx); return err }},
		{ark.OperationCheck, func() error { _, err := repo.Check(ctx); return err }},
		{ark.OperationVerify, func() error { _, err := repo.Verify(ctx, snaps[0].ID); return err }},
		{ark.OperationVerify, func() error {
			_, err := repo.Verify(context.Background(), "ffffffff")
			if !errors.Is(err, ark.ErrNotFound) {
				t.Errorf("Verify of a missing snapshot: %v, want ErrNotFound", err)
			}
			return err
		}},
	}
	for _, o := range ops {
		rec.reset()
		if err := o.run(); err == nil {
			t.Fatalf("%s succeeded on a cancelled context", o.op)
		}
		rec.take(t, o.op, false)
	}
}

// faultBackend fails Put and Delete for one handle type once fail is set.
type faultBackend struct {
	ark.Backend
	fail ark.HandleType
}

var errInjected = errors.New("injected fault")

func (b *faultBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if h.Type == b.fail {
		return errInjected
	}
	return b.Backend.Put(ctx, h, r)
}

func (b *faultBackend) Delete(ctx context.Context, h ark.Handle) error {
	if h.Type == b.fail {
		return errInjected
	}
	return b.Backend.Delete(ctx, h)
}

// openFaulty is openRecorded over a faultBackend that starts failing once the
// returned function names a handle type.
func openFaulty(t *testing.T) (*ark.Repository, *recorder, []*ark.Snapshot, func(ark.HandleType)) {
	t.Helper()
	ctx := context.Background()
	fb := &faultBackend{Backend: arkmem.New()}
	b := &ioBackend{Backend: fb}
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rec := &recorder{b: b}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithProgress(rec.observe))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var snaps []*ark.Snapshot
	for _, content := range []string{"one", "two"} {
		s, err := repo.Backup(ctx, stubSource{name: "app", content: content})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		snaps = append(snaps, s)
	}
	rec.reset()
	return repo, rec, snaps, func(h ark.HandleType) { fb.fail = h }
}

func TestBackendFaultsFailTheOperation(t *testing.T) {
	ctx := context.Background()

	t.Run("forget", func(t *testing.T) {
		repo, rec, snaps, fail := openFaulty(t)
		fail(ark.SnapshotFile)
		_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snaps[0].ID}})
		if !errors.Is(err, errInjected) {
			t.Fatalf("Forget with a failing delete: %v, want the backend error", err)
		}
		rec.take(t, ark.OperationForget, false)
	})

	t.Run("backup", func(t *testing.T) {
		repo, rec, _, fail := openFaulty(t)
		fail(ark.SnapshotFile)
		if _, err := repo.Backup(ctx, stubSource{name: "app", content: "three"}); !errors.Is(err, errInjected) {
			t.Fatalf("Backup with a failing manifest write: %v, want the backend error", err)
		}
		for _, e := range rec.events {
			if e.Phase == ark.PhaseFinalize {
				t.Fatal("backup emitted finalize although its manifest was never written")
			}
		}
	})

	t.Run("prune", func(t *testing.T) {
		repo, _, snaps, fail := openFaulty(t)
		if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snaps[0].ID}}); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		before, err := repo.Prune(ctx, ark.WithPruneDryRun())
		if err != nil {
			t.Fatalf("Prune dry run: %v", err)
		}
		fail(ark.BlobFile)
		res, err := repo.Prune(ctx)
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
		after, err := repo.Prune(ctx, ark.WithPruneDryRun())
		if err != nil {
			t.Fatalf("Prune dry run: %v", err)
		}
		// The trees go, the chunks stay: what stays is neither removed nor freed.
		if res.BlobsRemoved == 0 || after.BlobsRemoved == 0 {
			t.Fatalf("removed %d, %d left, the test needs both trees and chunks orphaned",
				res.BlobsRemoved, after.BlobsRemoved)
		}
		if res.BlobsRemoved+after.BlobsRemoved != before.BlobsRemoved ||
			res.BytesFreed+after.BytesFreed != before.BytesFreed {
			t.Errorf("prune removed %d blobs / %d bytes, %d / %d left, of %d / %d orphaned",
				res.BlobsRemoved, res.BytesFreed, after.BlobsRemoved, after.BytesFreed,
				before.BlobsRemoved, before.BytesFreed)
		}
	})
}
