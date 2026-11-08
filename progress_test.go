package ark_test

import (
	"context"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// collect records every event a handler receives.
func collect(events *[]ark.ProgressEvent) ark.ProgressFunc {
	return func(e ark.ProgressEvent) { *events = append(*events, e) }
}

func phases(events []ark.ProgressEvent) map[ark.Phase]bool {
	seen := make(map[ark.Phase]bool, len(events))
	for _, e := range events {
		seen[e.Phase] = true
	}
	return seen
}

func TestBackupProgressReportsItsPhases(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	var events []ark.ProgressEvent
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"},
		ark.WithBackupProgress(collect(&events))); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	seen := phases(events)
	for _, want := range []ark.Phase{ark.PhaseScan, ark.PhaseStore, ark.PhaseFinalize} {
		if !seen[want] {
			t.Errorf("backup did not report phase %q, got %v", want, events)
		}
	}

	var withPath bool
	for _, e := range events {
		if e.Phase == ark.PhaseStore && e.CurrentPath == "data.txt" {
			withPath = true
		}
	}
	if !withPath {
		t.Errorf("no store event carried CurrentPath, got %v", events)
	}
}

func TestRestoreProgressReportsItsPhases(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var events []ark.ProgressEvent
	if _, err := repo.Restore(ctx, snap.ID, discardSink{},
		ark.WithRestoreProgress(collect(&events))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	seen := phases(events)
	for _, want := range []ark.Phase{ark.PhaseLoad, ark.PhaseWrite} {
		if !seen[want] {
			t.Errorf("restore did not report phase %q, got %v", want, events)
		}
	}

	for _, e := range events {
		if e.Phase == ark.PhaseWrite && e.FilesTotal != 1 {
			t.Errorf("write event reports %d files total, want 1", e.FilesTotal)
		}
	}
}

// The point of per-call progress: an observer only sees its own operation.
func TestProgressIsScopedToOneCall(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	var observed []ark.ProgressEvent
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"},
		ark.WithBackupProgress(collect(&observed))); err != nil {
		t.Fatalf("observed backup: %v", err)
	}
	before := len(observed)

	if _, err := repo.Backup(ctx, stubSource{name: "other", content: "two"}); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if len(observed) != before {
		t.Errorf("observer saw %d events from another call", len(observed)-before)
	}
}

func operations(events []ark.ProgressEvent) map[ark.Operation]bool {
	seen := make(map[ark.Operation]bool, len(events))
	for _, e := range events {
		seen[e.Operation] = true
	}
	return seen
}

func TestRepositoryProgressStampsTheOperation(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var events []ark.ProgressEvent
	repo, err := ark.Open(ctx, b, testPassword, ark.WithProgress(collect(&events)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backupEvents := len(events)
	if got := operations(events); len(got) != 1 || !got[ark.OperationBackup] {
		t.Errorf("backup events carry operations %v, want only backup", got)
	}

	if _, err := repo.Restore(ctx, snap.ID, discardSink{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := operations(events[backupEvents:]); len(got) != 1 || !got[ark.OperationRestore] {
		t.Errorf("restore events carry operations %v, want only restore", got)
	}

	var own []ark.ProgressEvent
	before := len(events)
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "two"},
		ark.WithBackupProgress(collect(&own))); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(events) != before || len(operations(own)) != 1 || !operations(own)[ark.OperationBackup] {
		t.Errorf("per-call observer: default saw %d events, call saw %v", len(events)-before, own)
	}

	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "three"},
		ark.WithBackupProgress(nil)); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(events) == before {
		t.Error("a nil per-call observer did not fall back to the repository default")
	}
}

// A handler set with WithProgress is shared by concurrent operations, so it
// is called concurrently; this runs under -race.
func TestRepositoryProgressIsCalledConcurrently(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var (
		mu     sync.Mutex
		events []ark.ProgressEvent
	)
	repo, err := ark.Open(ctx, b, testPassword, ark.WithProgress(func(e ark.ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	mu.Lock()
	events = nil
	mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.Restore(ctx, snap.ID, discardSink{})
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	got := operations(events)
	if !got[ark.OperationBackup] || !got[ark.OperationRestore] || len(got) != 2 {
		t.Errorf("concurrent operations reported %v, want backup and restore", got)
	}
}
