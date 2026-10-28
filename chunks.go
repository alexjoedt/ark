package ark

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// chunkFetcher reads chunk content, serving repeats from a cache. The zero
// cache is allowed and simply never hits.
type chunkFetcher struct {
	store   *objectStore
	cache   *chunkCache
	workers int

	// mu serialises onRead, so a progress handler is never called from two
	// fetches at once even though the fetches themselves run in parallel.
	mu     sync.Mutex
	read   int
	onRead func(read int)
}

func (r *Repository) newChunkFetcher(cache *chunkCache) *chunkFetcher {
	workers := r.concurrency
	if workers < 1 {
		workers = runtime.NumCPU() * 2
	}
	return &chunkFetcher{store: r.chunks, cache: cache, workers: workers}
}

func (f *chunkFetcher) get(ctx context.Context, id string) ([]byte, error) {
	data, ok := f.cache.get(id)
	if !ok {
		var err error
		if data, err = f.store.Get(ctx, id); err != nil {
			return nil, err
		}
		f.cache.put(id, data)
	}
	f.count()
	return data, nil
}

// count reports one more chunk read, cache hits included, so progress tracks
// the chunk count the manifest recorded.
func (f *chunkFetcher) count() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read++
	if f.onRead != nil {
		f.onRead(f.read)
	}
}

// chunkResult is one fetched chunk, or the error that stopped it.
type chunkResult struct {
	data []byte
	err  error
}

// reader returns the content of one file as a stream, fetching up to
// f.workers chunks at a time and yielding them in order.
//
// The bytes in flight are therefore bounded by the cache cap plus one chunk
// per worker, whatever the size of the file. The caller closes the reader,
// including when it stops early.
func (f *chunkFetcher) reader(ctx context.Context, refs []chunkRef) *chunkStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &chunkStream{
		pending: make(chan chan chunkResult, f.workers),
		cancel:  cancel,
	}

	go func() {
		defer close(s.pending)

		// Bounds the fetches running at once; s.pending bounds the ones that
		// finished early and wait to be read.
		sem := make(chan struct{}, f.workers)
		for _, ref := range refs {
			slot := make(chan chunkResult, 1)
			select {
			case s.pending <- slot:
			case <-ctx.Done():
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				slot <- chunkResult{err: ctx.Err()}
				return
			}
			go func(id string) {
				defer func() { <-sem }()
				data, err := f.get(ctx, id)
				slot <- chunkResult{data: data, err: err}
			}(ref.ID)
		}
	}()

	return s
}

// chunkStream is the ordered reader over one file's chunks returned by
// chunkFetcher.reader.
type chunkStream struct {
	pending chan chan chunkResult
	cancel  context.CancelFunc
	current []byte
	err     error
}

func (s *chunkStream) Read(p []byte) (int, error) {
	for len(s.current) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		slot, ok := <-s.pending
		if !ok {
			return 0, io.EOF
		}
		res := <-slot
		if res.err != nil {
			s.err = fmt.Errorf("ark: failed to fetch chunk: %w", res.err)
			return 0, s.err
		}
		s.current = res.data
	}
	n := copy(p, s.current)
	s.current = s.current[n:]
	return n, nil
}

// Close stops the fetches still running. It is safe to call more than once and
// after the stream is drained.
func (s *chunkStream) Close() error {
	s.cancel()
	for slot := range s.pending {
		<-slot // let every started fetch hand back its result and exit
	}
	s.current = nil
	return nil
}
