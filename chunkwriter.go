package ark

import (
	"context"
	"math/bits"
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

	// alloc and release are set together by pooled: chunkers from chunker
	// take chunk buffers from alloc and put returns each one through release,
	// exactly once. Both nil: chunk data belongs to the caller of put.
	alloc   func(size int) []byte
	release func([]byte)
}

// chunkBufs pools chunk buffers by capacity, a power of two per pool, so a
// buffer is less than twice the chunk it holds. One pool of largest-chunk
// buffers halved the collections of a backup, but held 4 MiB per chunk in
// flight and raised the peak memory by 45%.
type chunkBufs struct {
	pools [bits.UintSize]sync.Pool
	// heads recycles the *[]byte a pool stores, so a put does not allocate
	// a slice header of its own.
	heads sync.Pool
}

func (p *chunkBufs) get(size int) []byte {
	class := bits.Len(uint(max(size, 1) - 1))
	if h, ok := p.pools[class].Get().(*[]byte); ok {
		b := (*h)[:size]
		*h = nil
		p.heads.Put(h)
		return b
	}
	return make([]byte, size, 1<<class)
}

// put takes b back. b must come from get and nothing may read it afterwards:
// any buffer with a power-of-two capacity is pooled, whatever made it.
func (p *chunkBufs) put(b []byte) {
	c := uint(cap(b))
	if bits.OnesCount(c) != 1 {
		return
	}
	h, ok := p.heads.Get().(*[]byte)
	if !ok {
		h = new([]byte)
	}
	*h = b
	p.pools[bits.TrailingZeros(c)].Put(h)
}

// pooled makes w the owner of the chunk buffers: the chunkers w.chunker
// returns take them from a pool, and put gives each one back once nothing
// reads it any more.
func (w *chunkWriter) pooled() *chunkWriter {
	bufs := new(chunkBufs)
	w.alloc, w.release = bufs.get, bufs.put
	return w
}

// chunker returns a chunker for chunks that go to w.put.
func (w *chunkWriter) chunker(cfg chunker.Config) *chunker.Chunker {
	cr := chunker.New(nil, cfg)
	if w.alloc != nil {
		cr.SetAlloc(w.alloc)
	}
	return cr
}

// free returns the buffer of a chunk nothing reads any more.
func (w *chunkWriter) free(data []byte) {
	if w.release != nil {
		w.release(data)
	}
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
// earlier write hit. On every path c.Data is released once: here when the
// chunk is not scheduled, by the worker once the chunk is sealed.
func (w *chunkWriter) put(c chunker.Chunk) error {
	if err := w.failure(); err != nil {
		w.free(c.Data)
		return err
	}
	w.mu.Lock()
	if _, ok := w.seen[c.Hash]; ok {
		w.mu.Unlock()
		w.free(c.Data)
		return nil
	}
	w.seen[c.Hash] = struct{}{}
	w.mu.Unlock()

	select {
	case w.sem <- struct{}{}:
	case <-w.ctx.Done():
		w.free(c.Data)
		return w.fail(w.ctx.Err())
	}
	w.wg.Go(func() {
		defer func() { <-w.sem }()
		// putNamed seals c.Data into a buffer of its own and keeps nothing.
		size, err := w.store.putNamed(w.ctx, c.Hash, c.Data)
		w.free(c.Data)
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
