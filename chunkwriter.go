package ark

import (
	"context"
	"sync"

	"github.com/alexjoedt/ark/chunker"
)

// chunkWriter stores chunks in the background with a bounded number being
// sealed and added to a pack, so compression and the pack uploads overlap
// with reading and chunking the source.
//
// Each distinct chunk is stored once per run and remembered with its sizes,
// which is what stats counts: the chunk references in the snapshot carry no
// stored size, so a node of unchanged files hashes to the node already stored.
type chunkWriter struct {
	firstError
	store *objectStore
	ctx   context.Context
	sem   chan struct{}
	wg    sync.WaitGroup

	mu     sync.Mutex
	seen   map[string]struct{}
	stored map[string]storedChunk
}

// firstError keeps the first error of a group of goroutines and cancels their
// context when it arrives.
type firstError struct {
	mu     sync.Mutex
	err    error
	cancel context.CancelFunc
}

// fail records err unless an earlier error is recorded, cancels on the first
// one and returns the recorded error.
func (f *firstError) fail(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
		f.cancel()
	}
	return f.err
}

// failure returns the recorded error, nil while there is none.
func (f *firstError) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// storedChunk is what a chunk this run wrote adds to the statistics.
type storedChunk struct {
	raw, stored int64
}

func newChunkWriter(ctx context.Context, store *objectStore, workers int) *chunkWriter {
	ctx, cancel := context.WithCancel(ctx)
	return &chunkWriter{
		firstError: firstError{cancel: cancel},
		store:      store,
		ctx:        ctx,
		sem:        make(chan struct{}, max(workers, 1)),
		seen:       make(map[string]struct{}),
		stored:     make(map[string]storedChunk),
	}
}

// put schedules c for storage. It blocks while every worker is busy, which
// bounds the chunk data held in memory, and returns the first error any
// earlier write hit.
func (w *chunkWriter) put(c chunker.Chunk) error {
	if err := w.failure(); err != nil {
		return err
	}
	w.mu.Lock()
	if _, ok := w.seen[c.Hash]; ok {
		w.mu.Unlock()
		return nil
	}
	w.seen[c.Hash] = struct{}{}
	w.mu.Unlock()

	select {
	case w.sem <- struct{}{}:
	case <-w.ctx.Done():
		return w.fail(w.ctx.Err())
	}
	w.wg.Go(func() {
		defer func() { <-w.sem }()
		size, err := w.store.putNamed(w.ctx, c.Hash, c.Data)
		if err != nil {
			_ = w.fail(err)
			return
		}
		if size > 0 {
			w.mu.Lock()
			w.stored[c.Hash] = storedChunk{raw: int64(c.Length), stored: int64(size)}
			w.mu.Unlock()
		}
	})
	return nil
}

// wait blocks until every scheduled write finished and returns the first
// error. It must be called once on every path, the error paths included.
func (w *chunkWriter) wait() error {
	w.wg.Wait()
	w.cancel()
	return w.failure()
}

// stats folds the run into s once wait returned: every reference in entries
// counts, the distinct chunks this run stored and the entries use are new, the
// rest the CAS held.
func (w *chunkWriter) stats(s *SnapshotStats, entries []fileEntry) {
	var added int
	counted := make(map[string]struct{})
	for _, e := range entries {
		added += len(e.Chunks)
		for _, c := range e.Chunks {
			sc, ok := w.stored[c.ID]
			if _, dup := counted[c.ID]; !ok || dup {
				continue
			}
			counted[c.ID] = struct{}{}
			s.ChunksNew++
			s.StoredSize += sc.stored
			s.NewChunksRawSize += sc.raw
		}
	}
	s.ChunksTotal += added
	s.ChunksReused = s.ChunksTotal - s.ChunksNew
}
