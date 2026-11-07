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

	parent, err := r.parentFiles(ctx, parentID)
	if err != nil {
		logger.WarnContext(ctx, "failed to load the parent snapshot, reading every file",
			"parent", parentID, "error", err)
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

	err = src.Walk(ctx, func(entry FileInfo, open func() (io.ReadCloser, error)) error {
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
			if prev, ok := parent[normEntryPath(entry.Path)]; ok && unchanged(entry, prev.FileInfo) {
				// The chunks are in the repository already, so they are
				// reused, exactly as a re-read would have found them.
				for i := range prev.Chunks {
					prev.Chunks[i].Size = 0
				}
				fe.Chunks = prev.Chunks
				stats.FilesUnchanged++
			} else {
				chunks, err := chunkFile(w, cr, open)
				if err != nil {
					return fmt.Errorf("backup: failed to chunk file %s: %w", entry.Path, err)
				}
				fe.Chunks = chunks
			}

			// A file that shrank while it was read yields fewer bytes than
			// its stat size; the snapshot records the bytes it stored.
			fe.Size = 0
			for _, c := range fe.Chunks {
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
		"files_unchanged", stats.FilesUnchanged,
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

// parentFiles loads the regular files of snapshot id, keyed by path. An empty
// id has no parent and returns no files.
func (r *Repository) parentFiles(ctx context.Context, id string) (map[string]fileEntry, error) {
	if id == "" {
		return map[string]fileEntry{}, nil
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	files := make(map[string]fileEntry)
	err = walkTree(ctx, r.trees, m.RootTree, func(e fileEntry) error {
		if e.Type == FileTypeRegular {
			files[e.Path] = e
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// unchanged reports whether cur can take prev's chunks without being read.
// ChangeTime and Inode only count when both sides carry them, and a zero
// ModTime never matches: a source that reports no time cannot be trusted to
// report a change.
func unchanged(cur, prev FileInfo) bool {
	if cur.Size != prev.Size || cur.ModTime.IsZero() || !cur.ModTime.Equal(prev.ModTime) {
		return false
	}
	if cur.ChangeTime.IsZero() || prev.ChangeTime.IsZero() || cur.Inode == 0 || prev.Inode == 0 {
		return true
	}
	return cur.ChangeTime.Equal(prev.ChangeTime) && cur.Inode == prev.Inode
}

// chunkFile opens a regular file the source yielded and chunks it.
func chunkFile(w *chunkWriter, cr *chunker.Chunker, open func() (io.ReadCloser, error)) ([]chunkRef, error) {
	if open == nil {
		return nil, errors.New("source yielded no reader for a regular file")
	}
	rc, err := open()
	if err != nil {
		return nil, err
	}
	chunks, err := chunkFileData(w, cr, rc)
	if cerr := rc.Close(); err == nil && cerr != nil {
		err = cerr
	}
	return chunks, err
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
