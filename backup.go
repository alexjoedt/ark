package ark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// runBackup executes the CDC backup pipeline by walking src and chunking
// every regular file into the CAS. Returns the completed manifest.
func (r *Repository) runBackup(
	ctx context.Context, src Source, opts backupOptions, session *packSession,
) (*manifest, error) {
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

	parent, err := r.parentFiles(ctx, session.trees, parentID, meta.Name)
	if err != nil {
		logger.WarnContext(ctx, "failed to load the parent snapshot, reading every file",
			"parent", parentID, "error", err)
	}

	now := time.Now().UTC()
	m := &manifest{
		Version:    manifestVersion3,
		ID:         newSnapshotID(),
		ParentID:   parentID,
		CreatedAt:  now,
		SourceName: meta.Name,
		SourceType: meta.Type,
		SourceMeta: maps.Clone(meta.Labels),
		Excludes:   slices.Clone(meta.Excludes),
		Tags:       opts.tags,
		Host:       opts.host,
		Comment:    opts.comment,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var cr *chunker.Chunker
	w := newChunkWriter(ctx, session.chunks, r.concurrency)
	var scannedEntries []fileEntry
	var stats SnapshotStats

	// Progress calls never overlap, but the readers finish files concurrently.
	// An empty path counts a hardlink, which reads nothing and reports nothing.
	var progressMu sync.Mutex
	var filesDone int
	var bytesDone int64
	fileDone := func(path string, refs []chunkRef) {
		progressMu.Lock()
		defer progressMu.Unlock()
		filesDone++
		if path == "" {
			return
		}
		bytesDone += refsSize(refs)
		opts.progress(ProgressEvent{Phase: PhaseStore, CurrentPath: path, FilesDone: filesDone, BytesDone: bytesDone})
	}

	var readers *fileReaders
	var pending []pendingFile
	// chunkErr is the first file that failed to chunk, on either path.
	var chunkErr error
	if cs, ok := src.(ConcurrentSource); ok && cs.ConcurrentOpen() {
		readers = newFileReaders(ctx, cancel, w, r.chunker, r.concurrency, fileDone)
	}
	// A wrapper that does not forward ConcurrentOpen silently costs the
	// parallel read, so the chosen path is logged.
	logger.DebugContext(ctx, "reading files", "parallel", readers != nil)

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
			fileDone("", nil)

		case FileTypeRegular:
			switch prev, ok := parent[normEntryPath(entry.Path)]; {
			case ok && unchanged(entry, prev.FileInfo):
				// The chunks are in the repository already, so they are
				// reused, exactly as a re-read would have found them.
				fe.Chunks = prev.Chunks
				stats.FilesUnchanged++
				fileDone(entry.Path, fe.Chunks)
			case readers != nil:
				// The entry keeps its place in the walk order; its chunks
				// are filled in once the readers are done.
				p := pendingFile{index: len(scannedEntries), refs: new([]chunkRef)}
				if err := readers.submit(fileJob{path: entry.Path, open: open, refs: p.refs}); err != nil {
					return err
				}
				pending = append(pending, p)
			default:
				if cr == nil {
					cr = chunker.New(nil, r.chunker)
				}
				chunks, err := chunkFile(w, cr, open)
				if err != nil {
					chunkErr = fmt.Errorf("failed to chunk file %s: %w", entry.Path, err)
					return chunkErr
				}
				fe.Chunks = chunks
				fileDone(entry.Path, fe.Chunks)
			}

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
	// A file that failed first stopped the walk and is the cause; a walk that
	// failed on its own stops the readers.
	fileFailed := chunkErr != nil || readers != nil && readers.failure() != nil
	if err != nil {
		cancel()
	}
	if readers != nil {
		chunkErr = readers.wait()
	}
	werr := w.wait()
	if err != nil && !fileFailed {
		return nil, fmt.Errorf("backup: walk failed: %w", err)
	}
	if chunkErr != nil {
		return nil, fmt.Errorf("backup: %w", chunkErr)
	}
	if werr != nil {
		return nil, fmt.Errorf("backup: failed to store chunks: %w", werr)
	}
	for _, p := range pending {
		scannedEntries[p.index].Chunks = *p.refs
	}
	for i := range scannedEntries {
		fe := &scannedEntries[i]
		if fe.Type != FileTypeRegular {
			continue
		}
		// A file that shrank while it was read yields fewer bytes than its
		// stat size; the snapshot records the bytes it stored.
		fe.Size = refsSize(fe.Chunks)
		stats.FileCount++
		stats.TotalSize += fe.Size
	}
	w.stats(&stats, scannedEntries)

	rootHash, err := buildTree(ctx, session.trees, scannedEntries, r.concurrency)
	if err != nil {
		return nil, fmt.Errorf("backup: failed to build snapshot tree: %w", err)
	}
	m.RootTree = rootHash
	if err := session.finish(ctx); err != nil {
		return nil, fmt.Errorf("backup: failed to store packs: %w", err)
	}

	stats.Duration = time.Since(start)

	stats.computeRatios()

	m.Stats = stats

	if err := r.saveManifest(ctx, m); err != nil {
		return nil, err
	}
	opts.progress(ProgressEvent{Phase: PhaseFinalize, FilesDone: stats.FileCount, BytesDone: stats.TotalSize})

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

// parentFiles loads the regular files of snapshot id that a backup of source
// may take unread, keyed by path. An empty id has no parent and returns no
// files. A parent of another source is refused: with only Size and ModTime to
// compare, its files could match another volume's by accident.
func (r *Repository) parentFiles(
	ctx context.Context, trees *objectStore, id, source string,
) (map[string]fileEntry, error) {
	if id == "" {
		return map[string]fileEntry{}, nil
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.SourceName != source {
		return nil, fmt.Errorf("parent %s belongs to source %q", id, m.SourceName)
	}
	// A rewrite in the same timestamp tick as the parent's read leaves the
	// stat unchanged, so a file whose mtime is not clearly older than the
	// parent's start is read again. Two seconds cover 1 s ticks (tar headers,
	// ext3, NFS) and 2 s ticks (FAT).
	racy := m.CreatedAt.Add(-2 * time.Second)
	files := make(map[string]fileEntry)
	// Nothing reads a tree node after this walk: its whole packs go with it
	// and do not stay in the store for the length of the run.
	walk := *trees
	walk.packs = newChunkCache(treePackCacheBytes)
	err = walkTree(ctx, &walk, m.RootTree, func(e fileEntry) error {
		if e.Type == FileTypeRegular && e.ModTime.Before(racy) {
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
// chunk references. cr is built from the repository config, never from the
// code defaults, so a later change to the defaults keeps deduplication working.
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

// refsSize is the number of bytes the chunk references cover.
func refsSize(refs []chunkRef) int64 {
	var n int64
	for _, c := range refs {
		n += c.Length
	}
	return n
}

// pendingFile is a scanned entry whose chunks a reader fills into refs.
type pendingFile struct {
	index int
	refs  *[]chunkRef
}

// fileJob is one file a reader opens, chunks and stores, its refs written to
// refs.
type fileJob struct {
	path string
	open func() (io.ReadCloser, error)
	refs *[]chunkRef
}

// fileReaders reads, chunks and hashes the files of a ConcurrentSource on a
// bounded set of goroutines with one chunker each, so memory stays at workers
// times the chunker buffer. The first error cancels the backup's context.
type fileReaders struct {
	firstError
	ctx  context.Context
	jobs chan fileJob
	wg   sync.WaitGroup
}

func newFileReaders(
	ctx context.Context, cancel context.CancelFunc, w *chunkWriter, cfg chunker.Config, workers int,
	done func(path string, refs []chunkRef),
) *fileReaders {
	fr := &fileReaders{firstError: firstError{cancel: cancel}, ctx: ctx, jobs: make(chan fileJob)}
	for range max(workers, 1) {
		fr.wg.Go(func() {
			cr := chunker.New(nil, cfg)
			for job := range fr.jobs {
				if fr.ctx.Err() != nil {
					_ = fr.fail(fr.ctx.Err())
					continue
				}
				refs, err := chunkFile(w, cr, job.open)
				if err != nil {
					_ = fr.fail(fmt.Errorf("failed to chunk file %s: %w", job.path, err))
					continue
				}
				*job.refs = refs
				done(job.path, refs)
			}
		})
	}
	return fr
}

// submit hands job to the next idle reader, blocking while all are busy. It
// returns the error that stopped the readers.
func (fr *fileReaders) submit(job fileJob) error {
	select {
	case fr.jobs <- job:
		return nil
	case <-fr.ctx.Done():
		return fr.fail(fr.ctx.Err())
	}
}

// wait lets the readers finish the submitted files, stops them and returns the
// first error. It must be called once, after the last submit.
func (fr *fileReaders) wait() error {
	close(fr.jobs)
	fr.wg.Wait()
	return fr.failure()
}
