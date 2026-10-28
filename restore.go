package ark

import (
	"context"
	"fmt"
	"io"
	"time"
)

type restoreStats struct {
	FilesRestored int
	BytesRestored int64
	ChunksFetched int
	Duration      time.Duration
}

// runRestore streams a snapshot into sink. It walks the tree in order and
// reads the chunks of the current file only, so the bytes it holds are the
// chunk cache plus one chunk per worker, whatever the size of the snapshot.
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

	opts.progress(ProgressEvent{Phase: PhaseLoad})

	meta := SourceMeta{
		Name:   m.SourceName,
		Type:   m.SourceType,
		Labels: m.SourceMeta,
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
	err = walkSnapshot(ctx, r.trees, m, newPathFilter(opts.paths), func(fe fileEntry) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var rd io.Reader
		switch fe.Type {
		case FileTypeRegular:
			// The sink sees a plain reader and never learns about the CAS.
			stream := fetcher.reader(ctx, fe.Chunks)
			defer stream.Close() // only cancels fetches still running
			rd = stream

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
	logger.InfoContext(ctx, "restore completed",
		"files", stats.FilesRestored,
		"bytes", stats.BytesRestored,
		"duration", stats.Duration,
	)
	return &stats, nil
}
