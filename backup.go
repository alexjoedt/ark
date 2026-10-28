package ark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// runBackup executes the CDC backup pipeline by walking src and chunking
// every regular file into the CAS. Returns the completed manifest.
func (r *Repository) runBackup(ctx context.Context, src Source, opts backupOptions) (*manifest, error) {
	start := time.Now()

	meta := src.Meta()
	logger := r.logger.With("component", "backup", "source", meta.Name, "host", opts.host)
	parentID := opts.parent
	if parentID == "" {
		var err error
		parentID, err = r.latestSnapshotID(ctx, meta.Name)
		if err != nil {
			logger.WarnContext(ctx, "failed to resolve the parent snapshot", "error", err)
		}
	}

	now := time.Now().UTC()
	m := &manifest{
		Version:    manifestVersion3,
		ID:         generateID(now),
		ParentID:   parentID,
		CreatedAt:  now,
		SourceName: meta.Name,
		SourceType: meta.Type,
		SourceMeta: meta.Labels,
		Tags:       opts.tags,
		Host:       opts.host,
		Comment:    opts.comment,
	}

	cr := chunker.New(nil, r.chunker)
	w := newChunkWriter(ctx, r.chunks, r.concurrency)
	var scannedEntries []fileEntry
	var stats SnapshotStats

	opts.progress(ProgressEvent{Phase: PhaseScan})

	err := src.Walk(ctx, func(entry FileInfo, r2 io.Reader) error {
		fe := fileEntry{FileInfo: entry}

		switch entry.Type {
		case FileTypeDirectory:
			stats.DirCount++

		case FileTypeSymlink:
			// no extra stats

		case FileTypeHardlink:
			// A second name for content already stored under fe.LinkDest.
			// It restores as a file, so it counts, but it reads no bytes.
			stats.FileCount++

		case FileTypeRegular:
			chunks, err := chunkFileData(w, cr, r2)
			if err != nil {
				return fmt.Errorf("backup: failed to chunk file %s: %w", entry.Path, err)
			}
			fe.Chunks = chunks

			// A file that shrank while it was read yields fewer bytes than
			// its stat size; the snapshot records the bytes it stored.
			fe.Size = 0
			for _, c := range chunks {
				fe.Size += c.Length
			}
			stats.FileCount++
			stats.TotalSize += fe.Size

			opts.progress(ProgressEvent{
				Phase:       PhaseStore,
				CurrentPath: entry.Path,
				FilesDone:   stats.FileCount,
				BytesDone:   stats.TotalSize,
			})

		case FileTypeUnsupported:
			// Devices, fifos, sockets: the v1 format does not model them.
			// Drop the entry and count it.
			stats.Skipped++
			return nil

		default:
			stats.Skipped++
			return nil
		}

		scannedEntries = append(scannedEntries, fe)
		return nil
	})
	werr := w.wait()
	if err != nil {
		return nil, fmt.Errorf("backup: walk failed: %w", err)
	}
	if werr != nil {
		return nil, fmt.Errorf("backup: failed to store chunks: %w", werr)
	}
	w.fill(scannedEntries)
	for _, e := range scannedEntries {
		stats.countChunks(e.Chunks)
	}

	opts.progress(ProgressEvent{Phase: PhaseFinalize, FilesDone: stats.FileCount, BytesDone: stats.TotalSize})

	rootHash, err := buildTree(ctx, r.trees, scannedEntries)
	if err != nil {
		return nil, fmt.Errorf("backup: failed to build snapshot tree: %w", err)
	}
	m.RootTree = rootHash

	stats.Duration = time.Since(start)

	stats.computeRatios()

	m.Stats = stats

	if err := r.saveManifest(ctx, m); err != nil {
		return nil, err
	}

	logger.InfoContext(ctx, "backup completed",
		"id", m.ID,
		"files", stats.FileCount,
		"chunks_total", stats.ChunksTotal,
		"chunks_new", stats.ChunksNew,
		"chunks_reused", stats.ChunksReused,
		"skipped", stats.Skipped,
		"total_size", stats.TotalSize,
		"stored_size", stats.StoredSize,
		"duration", stats.Duration,
	)

	return m, nil
}

// chunkFileData reads r through cr, hands every chunk to w, and returns the
// chunk references. Their stored sizes are set by w.fill once w is drained. cr is built from the repository config, never
// from the code defaults, so a later change to the defaults keeps
// deduplication working.
func chunkFileData(w *chunkWriter, cr *chunker.Chunker, r io.Reader) ([]chunkRef, error) {
	cr.Reset(r)
	var refs []chunkRef
	for {
		chunk, err := cr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		if err := w.put(chunk); err != nil {
			return nil, fmt.Errorf("failed to store chunk: %w", err)
		}
		refs = append(refs, chunkRef{
			ID:     chunk.Hash,
			Length: int64(chunk.Length),
			Offset: int64(chunk.Start),
		})
	}
	return refs, nil
}
