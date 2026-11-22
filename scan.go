package ark

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// scanTick throttles the PhaseScan events of a running scan.
const scanTick = 100 * time.Millisecond

// backupScan counts the files and bytes of a second walk of the source beside
// the store. Its events go out under the backup's progress mutex, which also
// guards done and the totals.
type backupScan struct {
	mu         *sync.Mutex
	done       bool
	filesTotal int
	bytesTotal int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startScan starts the counting walk when opts ask for it and src is a
// RewalkSource; otherwise it returns nil. mu serialises progress
// calls with the store's.
func startScan(ctx context.Context, src Source, opts backupOptions, mu *sync.Mutex, logger *slog.Logger) *backupScan {
	if !opts.scan {
		return nil
	}
	// A wrapper that does not forward Rewalk silently costs the totals.
	rs, ok := src.(RewalkSource)
	if !ok {
		logger.DebugContext(ctx, "counting scan skipped")
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &backupScan{mu: mu, cancel: cancel}
	s.wg.Go(func() {
		var files int
		var bytes int64
		last := time.Now()
		err := rs.Rewalk(ctx, func(entry FileInfo) error {
			switch entry.Type {
			case FileTypeRegular:
				files++
				bytes += entry.Size
			case FileTypeHardlink:
				files++
			case FileTypeDirectory, FileTypeSymlink, FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice,
				FileTypeUnsupported:
				// no files: a backup counts none of them in FileCount
			}
			if time.Since(last) >= scanTick {
				last = time.Now()
				s.emit(opts.progress, files, bytes, false)
			}
			return nil
		})
		if errors.Is(err, errors.ErrUnsupported) {
			logger.DebugContext(ctx, "counting scan skipped")
			return
		}
		if err != nil {
			logger.DebugContext(ctx, "counting scan dropped", "error", err)
			return
		}
		s.emit(opts.progress, files, bytes, true)
	})
	return s
}

// emit sends one PhaseScan event with the count so far in FilesDone and
// BytesDone; final also sets it as the totals.
func (s *backupScan) emit(progress ProgressFunc, files int, bytes int64, final bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := ProgressEvent{Phase: PhaseScan, FilesDone: files, BytesDone: bytes}
	if final {
		s.done, s.filesTotal, s.bytesTotal = true, files, bytes
		e.FilesTotal, e.BytesTotal = files, bytes
	}
	progress(e)
}

// totals returns the final count, zero until the scan finished. The caller
// holds the progress mutex.
func (s *backupScan) totals() (int, int64) {
	if s == nil || !s.done {
		return 0, 0
	}
	return s.filesTotal, s.bytesTotal
}

// stop cancels a scan that still runs and waits until it sent its last event.
// It is safe on a nil scan and more than once.
func (s *backupScan) stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}
