package ark

import (
	"context"
	"sync"

	"github.com/alexjoedt/ark/chunker"
)

// chunkWriter stores chunks in the background with a bounded number of
// backend operations in flight, so compression and the backend round trips
// overlap with reading and chunking the source.
//
// Each distinct chunk is stored once per run. The stored size lands in a map
// and fill copies it onto the first reference in walk order, which keeps the
// snapshot identical to a sequential run.
type chunkWriter struct {
	store  *objectStore
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	err    error
	seen   map[string]struct{}
	stored map[string]int
}

func newChunkWriter(ctx context.Context, store *objectStore, workers int) *chunkWriter {
	ctx, cancel := context.WithCancel(ctx)
	return &chunkWriter{
		store:  store,
		ctx:    ctx,
		cancel: cancel,
		sem:    make(chan struct{}, max(workers, 1)),
		seen:   make(map[string]struct{}),
		stored: make(map[string]int),
	}
}

// put schedules c for storage. It blocks while every worker is busy, which
// bounds the chunk data held in memory, and returns the first error any
// earlier write hit.
func (w *chunkWriter) put(c chunker.Chunk) error {
	w.mu.Lock()
	if w.err != nil {
		w.mu.Unlock()
		return w.err
	}
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
			w.stored[c.Hash] = size
			w.mu.Unlock()
		}
	})
	return nil
}

// fail records the first error and stops the writes still running.
func (w *chunkWriter) fail(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
		w.cancel()
	}
	return w.err
}

// wait blocks until every scheduled write finished and returns the first
// error. It must be called once on every path, the error paths included.
func (w *chunkWriter) wait() error {
	w.wg.Wait()
	w.cancel()
	return w.err
}

// fill sets the stored size on the first reference to every chunk this run
// wrote, in entry order. Every other reference keeps size 0, reused.
func (w *chunkWriter) fill(entries []fileEntry) {
	for i := range entries {
		for j := range entries[i].Chunks {
			ref := &entries[i].Chunks[j]
			if size, ok := w.stored[ref.ID]; ok {
				ref.Size = size
				delete(w.stored, ref.ID)
			}
		}
	}
}
