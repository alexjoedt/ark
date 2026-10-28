package ark_test

import (
	"context"
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
