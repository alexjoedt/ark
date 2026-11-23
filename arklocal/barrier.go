package arklocal

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// trackWrites says whether Put and Delete record what they touch so Sync can
// fsync it. Linux does not need it: syncfs flushes the whole filesystem. On
// macOS there is no syncfs, and sync(2) neither waits nor flushes the drive
// cache, so Sync fsyncs every recorded path and then one F_FULLFSYNC. Tests
// switch it on to run the logic anywhere.
var trackWrites = false

// fsyncPath flushes the file or directory at p. A path that is gone has
// nothing to flush; its parent directory was recorded by whoever removed it.
// It calls fsync(2), not File.Sync: on macOS that is F_FULLFSYNC, a flush of
// the whole drive cache per path, which Sync issues once at the end.
var fsyncPath = func(p string) error {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for {
		if err = unix.Fsync(int(f.Fd())); !errors.Is(err, unix.EINTR) {
			break
		}
	}
	_ = f.Close()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// written is the set of files and directories changed since the last Sync.
// Each path carries the number of the record call that last named it, so a
// Sync forgets only what it flushed: a path recorded again while the flush
// ran stays for the next one.
type written struct {
	mu    sync.Mutex
	seq   uint64
	files map[string]uint64
	dirs  map[string]uint64
}

// record notes the file p and every directory from dir up to root, since
// MkdirAll may have created any of them. An empty p records only directories.
func (w *written) record(root, p, dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.files == nil {
		w.files, w.dirs = map[string]uint64{}, map[string]uint64{}
	}
	w.seq++
	if p != "" {
		w.files[p] = w.seq
	}
	root = filepath.Clean(root)
	for d := dir; ; d = filepath.Dir(d) {
		w.dirs[d] = w.seq
		if d == root || d == filepath.Dir(d) {
			return
		}
	}
}

// snapshot returns a copy of the recorded files and directories.
func (w *written) snapshot() (files, dirs map[string]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.files), maps.Clone(w.dirs)
}

// forget drops the paths of a snapshot every flush of which succeeded; what
// was recorded after the snapshot, the same path included, stays.
func (w *written) forget(files, dirs map[string]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, seq := range files {
		if w.files[p] == seq {
			delete(w.files, p)
		}
	}
	for d, seq := range dirs {
		if w.dirs[d] == seq {
			delete(w.dirs, d)
		}
	}
}

// flush fsyncs the recorded files, then the directories, and keeps every path
// for the next Sync when any of them fails.
func (w *written) flush() (files, dirs map[string]uint64, err error) {
	files, dirs = w.snapshot()
	var errs []error
	for _, set := range []map[string]uint64{files, dirs} {
		for p := range set {
			if err := fsyncPath(p); err != nil {
				errs = append(errs, fmt.Errorf("arklocal: sync %s: %w", p, err))
			}
		}
	}
	return files, dirs, errors.Join(errs...)
}
