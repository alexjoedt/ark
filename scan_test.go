package ark_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
)

// scanBehaviour is what the counting walk of a scanSource does after it
// yielded every entry.
type scanBehaviour int

const (
	scanSucceeds scanBehaviour = iota
	scanFails
	scanHangs // until ctx is cancelled
)

// scanSource wraps a Source whose files are read inside fn, so the walk of
// the store calls open before it returns and the walk that returns without
// one is the scan. With gate set, every open waits for the scan to return,
// so the scan always finishes before the first file is stored; with release
// set, the opens wait for release instead.
type scanSource struct {
	ark.Source
	rewalk    bool
	gate      bool
	behaviour scanBehaviour
	release   <-chan struct{}

	walks     atomic.Int32
	scanned   chan struct{}
	sawCancel atomic.Bool
}

func newScanSource(src ark.Source, gate bool, b scanBehaviour) *scanSource {
	return &scanSource{Source: src, rewalk: true, gate: gate, behaviour: b, scanned: make(chan struct{})}
}

func (s *scanSource) Rewalk() bool { return s.rewalk }

func (s *scanSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	s.walks.Add(1)
	opened := false
	err := s.Source.Walk(ctx, func(e ark.FileInfo, open func() (io.ReadCloser, error)) error {
		if open == nil {
			return fn(e, nil)
		}
		return fn(e, func() (io.ReadCloser, error) {
			opened = true
			if s.gate {
				release := s.release
				if release == nil {
					release = s.scanned
				}
				select {
				case <-release:
				case <-time.After(10 * time.Second):
					return nil, errors.New("the scan never returned")
				}
			}
			return open()
		})
	})
	if opened || err != nil {
		return err
	}
	switch s.behaviour {
	case scanSucceeds:
	case scanFails:
		err = errors.New("scan failed")
	case scanHangs:
		<-ctx.Done()
		s.sawCancel.Store(true)
		err = ctx.Err()
	}
	close(s.scanned)
	return err
}

// eventLog records the events of one backup without a lock: overlapping
// calls trip the inFlight check and, under -race, the race detector. counted
// is closed on the first scan event with a count, which for a scan that
// takes microseconds is the final one.
type eventLog struct {
	events   []ark.ProgressEvent
	inFlight atomic.Int32
	overlap  atomic.Bool
	counted  chan struct{}
	seen     bool
}

func newEventLog() *eventLog { return &eventLog{counted: make(chan struct{})} }

func (l *eventLog) observe(e ark.ProgressEvent) {
	if l.inFlight.Add(1) > 1 {
		l.overlap.Store(true)
	}
	defer l.inFlight.Add(-1)
	time.Sleep(50 * time.Microsecond)
	if e.Phase == ark.PhaseScan && e.FilesTotal > 0 && !l.seen {
		l.seen = true
		close(l.counted)
	}
	l.events = append(l.events, e)
}

func (l *eventLog) backup(t *testing.T, repo *ark.Repository, src ark.Source, opts ...ark.BackupOption) *ark.Snapshot {
	t.Helper()
	l.events = nil
	snap, err := repo.Backup(context.Background(), src, append(opts, ark.WithBackupProgress(l.observe))...)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if l.overlap.Load() {
		t.Error("observer calls overlapped")
	}
	if n := len(l.events); n == 0 || l.events[n-1].Phase != ark.PhaseFinalize {
		t.Fatalf("want finalize last, got %d events", n)
	}
	return snap
}

// storeTotals fails unless every PhaseStore event carries the given totals.
func (l *eventLog) storeTotals(t *testing.T, files int, bytes int64) {
	t.Helper()
	stores := 0
	for _, e := range l.events {
		if e.Phase != ark.PhaseStore {
			continue
		}
		stores++
		if e.FilesTotal != files || e.BytesTotal != bytes {
			t.Fatalf("store event %s carries totals %d/%d, want %d/%d",
				e.CurrentPath, e.FilesTotal, e.BytesTotal, files, bytes)
		}
	}
	if stores == 0 {
		t.Fatal("no store events")
	}
}

func TestBackupScanWalksOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rewalk bool
		scan   bool
		walks  int32
	}{
		{"rewalk source with scan", true, true, 2},
		{"rewalk source without scan", true, false, 1},
		{"source that opts out", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := openConcRepo(t, 2)
			src := newScanSource(&concSource{name: "s", files: concFiles(4)}, false, scanSucceeds)
			src.rewalk = tc.rewalk
			var opts []ark.BackupOption
			if tc.scan {
				opts = append(opts, ark.WithBackupScan())
			}
			if _, err := repo.Backup(context.Background(), src, opts...); err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if got := src.walks.Load(); got != tc.walks {
				t.Errorf("Walk calls = %d, want %d", got, tc.walks)
			}
		})
	}
	if _, ok := any(ark.NewStreamSource("s", strings.NewReader(""))).(ark.RewalkSource); ok {
		t.Error("StreamSource implements RewalkSource; its reader cannot be walked twice")
	}
}

func TestBackupScanTotalsReachStoreEvents(t *testing.T) {
	repo := openConcRepo(t, 2)
	src := newScanSource(&concSource{name: "s", files: concFiles(20)}, true, scanSucceeds)
	log := newEventLog()
	src.release = log.counted
	snap := log.backup(t, repo, src, ark.WithBackupScan())

	var final *ark.ProgressEvent
	for i, e := range log.events {
		if e.Phase == ark.PhaseStore {
			break
		}
		if e.Phase == ark.PhaseScan && e.FilesTotal > 0 {
			final = &log.events[i]
		}
	}
	if final == nil {
		t.Fatal("no scan event with totals before the first store event")
	}
	if final.FilesTotal != snap.Stats.FileCount || final.BytesTotal != snap.Stats.TotalSize {
		t.Errorf("scan totals %d/%d, snapshot %d/%d",
			final.FilesTotal, final.BytesTotal, snap.Stats.FileCount, snap.Stats.TotalSize)
	}
	log.storeTotals(t, snap.Stats.FileCount, snap.Stats.TotalSize)
}

// slowRewalk is a ConcurrentSource whose walks take a few milliseconds per
// entry and whose opens take more, so the scan's throttled events land among
// the store's.
type slowRewalk struct{ *concSource }

func (s slowRewalk) Rewalk() bool { return true }

func (s slowRewalk) Walk(ctx context.Context, fn ark.WalkFunc) error {
	return s.concSource.Walk(ctx, func(e ark.FileInfo, open func() (io.ReadCloser, error)) error {
		time.Sleep(3 * time.Millisecond)
		return fn(e, open)
	})
}

func TestBackupScanEventsInterleaveWithoutOverlap(t *testing.T) {
	repo := openConcRepo(t, 4)
	src := slowRewalk{&concSource{name: "s", concurrent: true, files: concFiles(100), hook: func(int) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	}}}
	log := newEventLog()
	snap := log.backup(t, repo, src, ark.WithBackupScan())

	running, done := 0, false
	var last ark.ProgressEvent
	for _, e := range log.events {
		if e.Phase == ark.PhaseScan {
			if e.FilesTotal < last.FilesTotal || e.BytesTotal < last.BytesTotal {
				t.Errorf("scan count went back from %d/%d to %d/%d",
					last.FilesTotal, last.BytesTotal, e.FilesTotal, e.BytesTotal)
			}
			if e.FilesTotal > 0 {
				last = e
				running++
			}
			done = e.FilesTotal == snap.Stats.FileCount && e.BytesTotal == snap.Stats.TotalSize
		}
		if e.Phase == ark.PhaseStore {
			if done && (e.FilesTotal != snap.Stats.FileCount || e.BytesTotal != snap.Stats.TotalSize) {
				t.Errorf("store event after the scan carries %d/%d", e.FilesTotal, e.BytesTotal)
			}
			if !done && (e.FilesTotal != 0 || e.BytesTotal != 0) {
				t.Errorf("store event before the scan finished carries %d/%d", e.FilesTotal, e.BytesTotal)
			}
		}
	}
	if running < 2 {
		t.Errorf("%d scan events with a count, want running events beside the final one", running)
	}
	// 100 entries at 3 ms take about 300 ms, so a tick every 100 ms sends a
	// handful of events, not one per entry.
	if running > 10 {
		t.Errorf("%d scan events with a count, want them throttled", running)
	}
}

func TestBackupScanCountsLikeTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one.txt", strings.Repeat("1", 3000))
	write("a/two.txt", strings.Repeat("2", 5000))
	write("skip/big.bin", strings.Repeat("x", 90000))
	write("a/drop.tmp", strings.Repeat("y", 7000))
	write("a/changes.txt", "v1")
	if err := os.Link(filepath.Join(dir, "a/one.txt"), filepath.Join(dir, "a/link.txt")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, rel := range []string{"a/one.txt", "a/two.txt", "a/changes.txt"} {
		if err := os.Chtimes(filepath.Join(dir, rel), old, old); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := arkfs.NewSource(dir, arkfs.WithExclude("skip", "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	repo := openConcRepo(t, 2)

	// The changed file comes first in the walk, so the gate holds the store
	// before it reports any file.
	for run := range 2 {
		if run == 1 {
			write("a/changes.txt", "version two")
		}
		src := newScanSource(fs, true, scanSucceeds)
		log := newEventLog()
		src.release = log.counted
		snap := log.backup(t, repo, src, ark.WithBackupScan())
		if run == 1 && snap.Stats.FilesUnchanged == 0 {
			t.Fatal("second backup took no file unchanged")
		}
		log.storeTotals(t, snap.Stats.FileCount, snap.Stats.TotalSize)
		if snap.Stats.FileCount != 4 {
			t.Errorf("run %d: snapshot holds %d files, want 4 (excludes applied)", run, snap.Stats.FileCount)
		}
	}
}

func TestBackupScanFailureIsDropped(t *testing.T) {
	repo := openConcRepo(t, 2)
	src := newScanSource(&concSource{name: "s", files: concFiles(6)}, true, scanFails)
	log := newEventLog()
	snap := log.backup(t, repo, src, ark.WithBackupScan())
	if snap.Stats.FileCount != 6 {
		t.Errorf("snapshot holds %d files, want 6", snap.Stats.FileCount)
	}
	log.storeTotals(t, 0, 0)
}

func TestBackupScanStopsWhenTheStoreIsDone(t *testing.T) {
	repo := openConcRepo(t, 2)
	src := newScanSource(&concSource{name: "s", files: concFiles(6)}, false, scanHangs)
	log := newEventLog()
	log.backup(t, repo, src, ark.WithBackupScan())
	if !src.sawCancel.Load() {
		t.Error("backup returned before the scan was cancelled")
	}
	log.storeTotals(t, 0, 0)
}
