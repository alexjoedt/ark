package arklocal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// trackWrites says whether Put and Delete record what they touch so Sync can
// fsync it. Linux does not need it: syncfs flushes the whole filesystem. On
// macOS there is no syncfs, and sync(2) neither waits nor flushes the drive
// cache, so Sync fsyncs every recorded path and then one F_FULLFSYNC. Tests
// switch it on to run the logic anywhere.
var trackWrites = false

// fsyncPath flushes the file or directory at p. A path that is gone has
// nothing to flush; its parent directory was recorded by whoever removed it.
var fsyncPath = func(p string) error {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// written is the set of files and directories changed since the last Sync.
type written struct {
	mu    sync.Mutex
	files map[string]struct{}
	dirs  map[string]struct{}
}

// record notes the file p and every directory from dir up to root, since
// MkdirAll may have created any of them. An empty p records only directories.
func (w *written) record(root, p, dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.files == nil {
		w.files, w.dirs = map[string]struct{}{}, map[string]struct{}{}
	}
	if p != "" {
		w.files[p] = struct{}{}
	}
	root = filepath.Clean(root)
	for d := dir; ; d = filepath.Dir(d) {
		w.dirs[d] = struct{}{}
		if d == root || d == filepath.Dir(d) {
			return
		}
	}
}

// snapshot returns the recorded files and directories.
func (w *written) snapshot() (files, dirs []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := range w.files {
		files = append(files, p)
	}
	for d := range w.dirs {
		dirs = append(dirs, d)
	}
	return files, dirs
}

// forget drops the paths of a snapshot every flush of which succeeded; what
// was recorded after the snapshot stays.
func (w *written) forget(files, dirs []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range files {
		delete(w.files, p)
	}
	for _, d := range dirs {
		delete(w.dirs, d)
	}
}

// flush fsyncs the recorded files, then the directories, and keeps every path
// for the next Sync when any of them fails.
func (w *written) flush() (files, dirs []string, err error) {
	files, dirs = w.snapshot()
	var errs []error
	for _, p := range append(files, dirs...) {
		if err := fsyncPath(p); err != nil {
			errs = append(errs, fmt.Errorf("arklocal: sync %s: %w", p, err))
		}
	}
	return files, dirs, errors.Join(errs...)
}
