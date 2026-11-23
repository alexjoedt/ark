package arklocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/alexjoedt/ark"
)

// TestSyncFlushesWhatWasWritten pins the macOS barrier: Sync fsyncs every file
// and directory Put and Delete touched since the last Sync, then flushes the
// root once, and keeps the set when any step fails.
func TestSyncFlushesWhatWasWritten(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	origTrack, origPath, origFS := trackWrites, fsyncPath, syncFS
	t.Cleanup(func() { trackWrites, fsyncPath, syncFS = origTrack, origPath, origFS })
	trackWrites = true

	var steps []string
	var failPath string
	fsyncPath = func(p string) error {
		steps = append(steps, "fsync "+strings.TrimPrefix(p, b.basePath))
		if p == failPath {
			return syscall.EIO
		}
		return origPath(p)
	}
	fullErr := error(nil)
	syncFS = func(*os.File) error { steps = append(steps, "full"); return fullErr }

	h := ark.Handle{Type: ark.DataPackFile, Name: "abcdef0123456789"}
	gone := ark.Handle{Type: ark.SnapshotFile, Name: "01hxyz"}
	for _, x := range []ark.Handle{h, gone} {
		if err := b.Put(ctx, x, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"fsync /datapacks/ab/abcdef0123456789", "fsync /snapshots/01hxyz",
		"fsync ", "fsync /datapacks", "fsync /datapacks/ab", "fsync /snapshots", "full",
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if last := steps[len(steps)-1]; last != "full" || strings.Count(strings.Join(steps, ","), "full") != 1 {
		t.Fatalf("steps %v, want one full flush last", steps)
	}
	slices.Sort(steps)
	slices.Sort(want)
	if !slices.Equal(steps, want) {
		t.Fatalf("steps %v, want %v", steps, want)
	}

	steps = nil
	if err := b.Sync(ctx); err != nil || !slices.Equal(steps, []string{"full"}) {
		t.Fatalf("second Sync = %v with %v, want only the full flush", err, steps)
	}

	if err := b.Put(ctx, h, strings.NewReader("y")); err != nil {
		t.Fatal(err)
	}
	failPath = filepath.Join(b.basePath, "datapacks", "ab", h.Name)
	steps = nil
	if err := b.Sync(ctx); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Sync with a failing fsync = %v, want EIO", err)
	}
	if slices.Contains(steps, "full") {
		t.Fatalf("full flush after a failed fsync: %v", steps)
	}
	failPath, fullErr = "", syscall.EIO
	steps = nil
	if err := b.Sync(ctx); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Sync with a failing full flush = %v, want EIO", err)
	}
	if !slices.Contains(steps, "fsync /datapacks/ab/abcdef0123456789") {
		t.Fatalf("the set was dropped after a failure: %v", steps)
	}
	fullErr = nil
	steps = nil
	if err := b.Sync(ctx); err != nil || !slices.Contains(steps, "fsync /datapacks/ab") {
		t.Fatalf("retry = %v with %v", err, steps)
	}
	steps = nil
	if err := b.Sync(ctx); err != nil || !slices.Equal(steps, []string{"full"}) {
		t.Fatalf("Sync after success = %v with %v, want only the full flush", err, steps)
	}
}

// TestSyncKeepsAPutMadeDuringTheFlush pins that a path recorded again after
// Sync fsynced it is flushed by the next Sync, not forgotten with the first.
func TestSyncKeepsAPutMadeDuringTheFlush(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	origTrack, origPath, origFS := trackWrites, fsyncPath, syncFS
	t.Cleanup(func() { trackWrites, fsyncPath, syncFS = origTrack, origPath, origFS })
	trackWrites = true
	syncFS = func(*os.File) error { return nil }

	first := ark.Handle{Type: ark.DataPackFile, Name: "abcdef0123456789"}
	late := ark.Handle{Type: ark.DataPackFile, Name: "ab00000000000000"}
	shard := filepath.Join(b.basePath, "datapacks", "ab")
	if err := b.Put(ctx, first, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	var steps []string
	raced := false
	fsyncPath = func(p string) error {
		steps = append(steps, p)
		err := origPath(p)
		if p == shard && !raced {
			raced = true
			if err := b.Put(ctx, late, strings.NewReader("y")); err != nil {
				t.Fatal(err)
			}
		}
		return err
	}
	if err := b.Sync(ctx); err != nil || !raced {
		t.Fatalf("Sync = %v, raced %v", err, raced)
	}
	steps = nil
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{shard, filepath.Join(shard, late.Name)} {
		if !slices.Contains(steps, p) {
			t.Errorf("the next Sync did not flush %s: %v", p, steps)
		}
	}
	if slices.Contains(steps, filepath.Join(shard, first.Name)) {
		t.Errorf("the next Sync flushed the first object again: %v", steps)
	}
}
