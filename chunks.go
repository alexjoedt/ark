package ark

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// defaultPrefetchBytes is the floor of the bytes a chunkFetcher holds fetched
// but not yet read. The budget rises to one maximal chunk per worker when the
// chunker is configured larger, so a big file still keeps every worker busy.
const defaultPrefetchBytes int64 = 64 << 20

// chunkFetcher reads chunk content, serving repeats from a cache. The zero
// cache is allowed and simply never hits.
//
// All streams of one fetcher share its worker slots and its byte budget, so a
// caller may open the streams of upcoming files ahead of time: the fetches of
// the file being read overlap with the fetches of the next ones, and memory
// stays at the budget plus the cache, whatever the number of files.
type chunkFetcher struct {
	store   *objectStore
	cache   *chunkCache
	workers int
	sem     chan struct{} // fetches in flight across all streams
	budget  *byteBudget   // bytes fetched and not yet handed to a Read

	// turn is the done channel of the last stream handed out. Streams start
	// their fetches in the order reader was called, so the budget is granted
	// in that order too and an upcoming file can never starve the current one.
	turnMu sync.Mutex
	turn   chan struct{}

	// flights are the cache misses being fetched right now, so streams of
	// files sharing a chunk take one GET between them instead of one each.
	flightMu sync.Mutex
	flights  map[string]*flight

	// mu serialises onRead, so a progress handler is never called from two
	// fetches at once even though the fetches themselves run in parallel.
	mu     sync.Mutex
	read   int
	onRead func(read int)
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

func (r *Repository) newChunkFetcher(cache *chunkCache) *chunkFetcher {
	workers := r.concurrency
	if workers < 1 {
		workers = runtime.NumCPU() * 2
	}
	floor := r.prefetchBytes
	if floor <= 0 {
		floor = defaultPrefetchBytes
	}
	budget := max(floor, int64(workers)*int64(r.chunker.MaxSize))
	turn := make(chan struct{})
	close(turn)
	return &chunkFetcher{
		store:   r.chunks,
		cache:   cache,
		workers: workers,
		sem:     make(chan struct{}, workers),
		budget:  newByteBudget(budget),
		turn:    turn,
		flights: make(map[string]*flight),
	}
}

func (f *chunkFetcher) get(ctx context.Context, id string) ([]byte, error) {
	data, ok := f.cache.get(id)
	if !ok {
		var err error
		if data, err = f.fetch(ctx, id); err != nil {
			return nil, err
		}
	}
	f.count()
	return data, nil
}

// fetch reads one chunk from the store, merging concurrent misses for the same
// id into one GET. A follower whose leader failed fetches on its own: a stream
// closed early cancels only its own GET, never the streams sharing the chunk.
func (f *chunkFetcher) fetch(ctx context.Context, id string) ([]byte, error) {
	f.flightMu.Lock()
	if fl, ok := f.flights[id]; ok {
		f.flightMu.Unlock()
		select {
		case <-fl.done:
			if fl.err == nil {
				return fl.data, nil
			}
			return f.fetch(ctx, id)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// A leader caches before it leaves flights, so a flight that ended
	// between the caller's cache miss and this lock left its chunk here.
	if data, ok := f.cache.get(id); ok {
		f.flightMu.Unlock()
		return data, nil
	}
	fl := &flight{done: make(chan struct{})}
	f.flights[id] = fl
	f.flightMu.Unlock()

	fl.data, fl.err = f.store.Get(ctx, id)
	if fl.err == nil {
		f.cache.put(id, fl.data)
	}
	f.flightMu.Lock()
	delete(f.flights, id)
	f.flightMu.Unlock()
	close(fl.done)
	return fl.data, fl.err
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

// chunkResult is one fetched chunk, or the error that stopped it, plus the
// bytes it holds of the budget.
type chunkResult struct {
	data []byte
	held int64
	err  error
}

// reader returns the content of one file as a stream, fetching chunks in the
// background and yielding them in order.
//
// Streams must be read in the order reader handed them out, as a restore does
// with the files of a snapshot. The caller closes the reader, including when
// it stops early.
func (f *chunkFetcher) reader(ctx context.Context, refs []chunkRef) *chunkStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &chunkStream{
		pending: make(chan chan chunkResult, len(refs)+1), // one spare for the error slot
		cancel:  cancel,
		budget:  f.budget,
	}

	f.turnMu.Lock()
	prev := f.turn
	done := make(chan struct{})
	f.turn = done
	f.turnMu.Unlock()

	go func() {
		// A stream that stopped early passes its turn on only after the one
		// before it, or a later file could take the whole budget ahead of the
		// file being read. pending closes first, so Close never waits for it.
		defer func() {
			<-prev
			close(done)
		}()
		defer close(s.pending)

		select {
		case <-prev:
		case <-ctx.Done():
			s.pending <- failed(ctx.Err())
			return
		}
		for _, ref := range refs {
			held := ref.Length
			if err := f.budget.acquire(ctx, held); err != nil {
				s.pending <- failed(err)
				return
			}
			select {
			case f.sem <- struct{}{}:
			case <-ctx.Done():
				f.budget.release(held)
				s.pending <- failed(ctx.Err())
				return
			}
			slot := make(chan chunkResult, 1)
			s.pending <- slot
			go func(id string) {
				defer func() { <-f.sem }()
				data, err := f.get(ctx, id)
				slot <- chunkResult{data: data, held: held, err: err}
			}(ref.ID)
		}
	}()

	return s
}

func failed(err error) chan chunkResult {
	slot := make(chan chunkResult, 1)
	slot <- chunkResult{err: err}
	return slot
}

// chunkStream is the ordered reader over one file's chunks returned by
// chunkFetcher.reader.
type chunkStream struct {
	pending chan chan chunkResult
	cancel  context.CancelFunc
	budget  *byteBudget
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
		s.budget.release(res.held)
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
		res := <-slot // let every started fetch hand back its result and exit
		s.budget.release(res.held)
	}
	s.current = nil
	return nil
}

// byteBudget bounds the bytes held between fetch and read. A request larger
// than the whole budget is granted once nothing else is held, so one oversized
// chunk slows a stream down instead of stopping it.
//
// The turn chain lets one producer acquire at a time, so a single buffered
// signal is enough to wake it: a release that nobody waits for leaves the
// signal behind, and the next acquire just rechecks once.
type byteBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	freed chan struct{}
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{limit: limit, freed: make(chan struct{}, 1)}
}

func (b *byteBudget) acquire(ctx context.Context, n int64) error {
	for {
		b.mu.Lock()
		if b.used == 0 || b.used+n <= b.limit {
			b.used += n
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
		select {
		case <-b.freed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *byteBudget) release(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	select {
	case b.freed <- struct{}{}:
	default:
	}
}
