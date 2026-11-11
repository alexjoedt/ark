package ark

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

type restoreStats struct {
	FilesRestored int
	BytesRestored int64
	ChunksFetched int
	Duration      time.Duration
}

// runRestore streams a snapshot into sink, one file at a time in tree order,
// with the chunks of the upcoming files fetched while the sink writes the
// current one (streamSnapshot).
func (r *Repository) runRestore(
	ctx context.Context,
	snapshotID string,
	sink Sink,
	opts restoreOptions,
) (*restoreStats, error) {
	logger := r.logger.With("component", "restore", "snapshot", snapshotID)

	start := time.Now()

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	// Fetches report from their own goroutines while the sink reports from
	// this one; the handler sees one call at a time.
	var progressMu sync.Mutex
	report := opts.progress
	opts.progress = func(e ProgressEvent) {
		progressMu.Lock()
		defer progressMu.Unlock()
		report(e)
	}
	opts.progress(ProgressEvent{Phase: PhaseLoad})

	meta := SourceMeta{
		Name:     m.SourceName,
		Type:     m.SourceType,
		Labels:   m.SourceMeta,
		Excludes: m.Excludes,
	}
	if err := sink.Open(ctx, meta); err != nil {
		return nil, fmt.Errorf("restore: sink rejected snapshot: %w", err)
	}

	limit := defaultRestoreCache
	if opts.cacheSet {
		limit = opts.cacheLimit
	}
	fetcher := r.newChunkFetcher(newChunkCache(limit))
	fetcher.onRead = func(read int) {
		opts.progress(ProgressEvent{Phase: PhaseFetch, ChunksDone: read, ChunksTotal: m.Stats.ChunksTotal})
	}

	logger.InfoContext(ctx, "starting restore", "files", m.Stats.FileCount, "chunks", m.Stats.ChunksTotal)

	var stats restoreStats
	err = streamSnapshot(ctx, r.trees, fetcher, m, newPathFilter(opts.paths), func(entry openEntry) error {
		return receive(ctx, sink, entry, m, &stats, opts)
	})
	if err != nil {
		// The Sink contract calls Close on abort too; a streaming sink
		// otherwise waits on input that never comes.
		if closeErr := sink.Close(); closeErr != nil {
			logger.WarnContext(ctx, "sink close after failed restore", "error", closeErr)
		}
		return nil, err
	}

	if err := sink.Close(); err != nil {
		return nil, fmt.Errorf("restore: sink close failed: %w", err)
	}

	stats.ChunksFetched = fetcher.read
	stats.Duration = time.Since(start)
	opts.progress(ProgressEvent{
		Phase:       PhaseFinalize,
		FilesTotal:  m.Stats.FileCount,
		FilesDone:   stats.FilesRestored,
		BytesTotal:  m.Stats.TotalSize,
		BytesDone:   stats.BytesRestored,
		ChunksTotal: m.Stats.ChunksTotal,
		ChunksDone:  stats.ChunksFetched,
	})
	logger.InfoContext(ctx, "restore completed",
		"files", stats.FilesRestored,
		"bytes", stats.BytesRestored,
		"duration", stats.Duration,
	)
	return &stats, nil
}

// openEntry is one entry of the snapshot with its content stream opened ahead
// of time, nil when the entry carries no content.
type openEntry struct {
	fe     fileEntry
	stream *chunkStream
}

func (e openEntry) close() {
	if e.stream != nil {
		_ = e.stream.Close() // never fails, only cancels
	}
}

// streamSnapshot hands every entry of the snapshot to fn in tree order with
// its content already streaming. One goroutine walks the tree and opens the
// chunk streams of the upcoming files, so their fetches run while fn writes
// the current one; the bytes held ahead are bounded by the fetcher's budget
// and the cache, whatever the size of the snapshot. fn's error, or a
// cancelled ctx, stops the walk and every fetch; the streams opened ahead are
// drained and closed before streamSnapshot returns.
func streamSnapshot(
	ctx context.Context,
	trees *objectStore,
	fetcher *chunkFetcher,
	m *manifest,
	filter pathFilter,
	fn func(entry openEntry) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The queue holds the files opened ahead of fn. Its depth bounds the
	// entries without content (directories, empty files); the byte budget
	// bounds the rest.
	queue := make(chan openEntry, 4*fetcher.workers)
	walkErr := make(chan error, 1)
	go func() {
		defer close(queue)
		walkErr <- walkSnapshot(ctx, trees, m, filter, func(fe fileEntry) error {
			entry := openEntry{fe: fe}
			if fe.Type == FileTypeRegular {
				entry.stream = fetcher.reader(ctx, fe.Chunks)
			}
			select {
			case queue <- entry:
				return nil
			case <-ctx.Done():
				entry.close()
				return ctx.Err()
			}
		})
	}()

	var err error
	for entry := range queue {
		if err == nil {
			err = ctx.Err() // a cancelled caller stops even when everything is fetched
		}
		if err != nil {
			entry.close() // drain the files opened ahead of the failure
			continue
		}
		err = fn(entry)
		entry.close() // only cancels fetches still running
		if err != nil {
			cancel() // stop the walk and every fetch before draining
		}
	}
	if err == nil {
		err = <-walkErr
	}
	return err
}

// receive hands one entry to the sink.
func receive(
	ctx context.Context,
	sink Sink,
	entry openEntry,
	m *manifest,
	stats *restoreStats,
	opts restoreOptions,
) error {
	fe := entry.fe
	var rd io.Reader
	switch fe.Type {
	case FileTypeRegular:
		// The sink sees a plain reader and never learns about the CAS.
		rd = entry.stream
		stats.FilesRestored++
		stats.BytesRestored += fe.Size

	case FileTypeHardlink:
		// No bytes move, but a file appears on disk.
		stats.FilesRestored++

	case FileTypeDirectory, FileTypeSymlink, FileTypeUnsupported:
		// Metadata only.
	}

	if err := sink.Receive(ctx, fe.FileInfo, rd); err != nil {
		return fmt.Errorf("restore: sink error for %s: %w", fe.Path, err)
	}

	opts.progress(ProgressEvent{
		Phase:       PhaseWrite,
		CurrentPath: fe.Path,
		FilesTotal:  m.Stats.FileCount,
		FilesDone:   stats.FilesRestored,
		BytesTotal:  m.Stats.TotalSize,
		BytesDone:   stats.BytesRestored,
	})
	return nil
}
