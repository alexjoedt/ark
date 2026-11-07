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

// runRestore streams a snapshot into sink. One goroutine walks the tree in
// order and opens the chunk streams of the upcoming files, so their fetches
// run while the sink writes the current file; the sink still receives the
// files one at a time in tree order. The bytes held ahead are bounded by the
// fetcher's budget and the cache, whatever the size of the snapshot.
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The queue holds the files opened ahead of the sink. Its depth bounds
	// the entries without content (directories, empty files); the byte budget
	// bounds the rest.
	queue := make(chan restoreItem, 4*fetcher.workers)
	walkErr := make(chan error, 1)
	go func() {
		defer close(queue)
		walkErr <- walkSnapshot(ctx, r.trees, m, newPathFilter(opts.paths), func(fe fileEntry) error {
			item := restoreItem{fe: fe}
			if fe.Type == FileTypeRegular {
				item.stream = fetcher.reader(ctx, fe.Chunks)
			}
			select {
			case queue <- item:
				return nil
			case <-ctx.Done():
				item.close()
				return ctx.Err()
			}
		})
	}()

	var stats restoreStats
	for item := range queue {
		if err != nil {
			item.close() // drain the files opened ahead of the failure
			continue
		}
		err = r.restoreItem(ctx, sink, item, m, &stats, opts)
		if err != nil {
			cancel() // stop the walk and every fetch before draining
		}
	}
	if err == nil {
		err = <-walkErr
	}
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

// restoreItem is one entry of the snapshot with its content stream opened
// ahead of time, nil when the entry carries no content.
type restoreItem struct {
	fe     fileEntry
	stream *chunkStream
}

func (it restoreItem) close() {
	if it.stream != nil {
		_ = it.stream.Close() // never fails, only cancels
	}
}

// restoreItem hands one entry to the sink and closes its stream.
func (r *Repository) restoreItem(
	ctx context.Context,
	sink Sink,
	item restoreItem,
	m *manifest,
	stats *restoreStats,
	opts restoreOptions,
) error {
	defer item.close() // only cancels fetches still running

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	fe := item.fe
	var rd io.Reader
	switch fe.Type {
	case FileTypeRegular:
		// The sink sees a plain reader and never learns about the CAS.
		rd = item.stream
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
