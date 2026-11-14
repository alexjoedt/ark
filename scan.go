package ark

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// scanTick throttles the PhaseScan events of a running scan.
const scanTick = 100 * time.Millisecond

// backupScan counts the files and bytes of a second walk of the source beside
// the store. Its events go out under the backup's progress mutex, which also
// guards done and the final totals.
type backupScan struct {
	files atomic.Int64
	bytes atomic.Int64

	mu         *sync.Mutex
	done       bool
	filesTotal int
	bytesTotal int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startScan starts the counting walk when opts ask for it and src promises a
// second concurrent Walk; otherwise it returns nil. mu serialises progress
// calls with the store's.
func startScan(ctx context.Context, src Source, opts backupOptions, mu *sync.Mutex, logger *slog.Logger) *backupScan {
	rs, ok := src.(RewalkSource)
	if !opts.scan || !ok || !rs.Rewalk() {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &backupScan{mu: mu, cancel: cancel}
	errc := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		errc <- src.Walk(ctx, func(entry FileInfo, _ func() (io.ReadCloser, error)) error {
			switch entry.Type {
			case FileTypeRegular:
				s.files.Add(1)
				s.bytes.Add(entry.Size)
			case FileTypeHardlink:
				s.files.Add(1)
			case FileTypeDirectory, FileTypeSymlink, FileTypeUnsupported:
				// no files: a backup counts none of them in FileCount
			}
			return nil
		})
	}()
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(scanTick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if ctx.Err() == nil {
					s.emit(opts.progress, false)
				}
			case err := <-errc:
				if err != nil {
					logger.DebugContext(ctx, "scan dropped", "error", err)
					return
				}
				s.emit(opts.progress, true)
				return
			}
		}
	}()
	return s
}

// emit sends one PhaseScan event with the count so far; final marks the
// count as the source's totals.
func (s *backupScan) emit(progress ProgressFunc, final bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, bytes := int(s.files.Load()), s.bytes.Load()
	if final {
		s.done, s.filesTotal, s.bytesTotal = true, files, bytes
	}
	progress(ProgressEvent{Phase: PhaseScan, FilesTotal: files, BytesTotal: bytes})
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
