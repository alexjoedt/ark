package ark

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"runtime"
	"slices"
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
//
// Chunks are read by pack: a pack has at most one read running, and the chunks
// wanted from it meanwhile share the next one, a few ranges for a pack's worth
// of small files instead of one round trip each.
type chunkFetcher struct {
	store   *objectStore
	cache   *chunkCache
	workers int
	budget  *byteBudget // bytes fetched and not yet handed to a Read

	// turn is the done channel of the last stream handed out. Streams start
	// their fetches in the order reader was called, so the budget is granted
	// in that order too and an upcoming file can never starve the current one.
	turnMu sync.Mutex
	turn   chan struct{}

	// mu guards the chunks wanted and the pack reads running.
	mu sync.Mutex
	// flights are the chunks wanted and not yet read, so streams of files
	// sharing a chunk take one read between them instead of one each.
	flights map[string]*flight
	pending map[string][]*flight // by pack, wanted and in no read yet
	order   []string             // packs with pending chunks, oldest first
	reading map[string]bool      // packs with a read running
	running int                  // pack reads running, at most workers
	reads   sync.WaitGroup

	// countMu serialises onRead, so a progress handler is never called from
	// two streams at once.
	countMu sync.Mutex
	read    int
	onRead  func(read int)
}

// flight is one chunk wanted from a pack. done closes once data or err is set.
type flight struct {
	id   string
	ctx  context.Context // the read of its pack runs under the first one's
	at   packEntry
	done chan struct{}
	data []byte
	err  error
}

func landed(data []byte, err error) *flight {
	fl := &flight{done: make(chan struct{}), data: data, err: err}
	close(fl.done)
	return fl
}

func (r *Repository) newChunkFetcher(chunks *objectStore, cache *chunkCache) *chunkFetcher {
	workers := r.concurrency
	if workers < 1 {
		workers = runtime.NumCPU() * 2
	}
	floor := r.prefetchBytes
	if floor <= 0 {
		floor = defaultPrefetchBytes
	}
	return newFetcher(chunks, cache, workers, max(floor, int64(workers)*int64(r.chunker.MaxSize)))
}

func newFetcher(chunks *objectStore, cache *chunkCache, workers int, budget int64) *chunkFetcher {
	turn := make(chan struct{})
	close(turn)
	return &chunkFetcher{
		store:   chunks,
		cache:   cache,
		workers: workers,
		budget:  newByteBudget(budget),
		turn:    turn,
		flights: make(map[string]*flight),
		pending: make(map[string][]*flight),
		reading: make(map[string]bool),
	}
}

// want returns the flight of the chunk id, starting the read of its pack when
// a worker slot is free and the pack has no read running.
func (f *chunkFetcher) want(ctx context.Context, id string) *flight {
	if data, ok := f.cache.get(id); ok {
		return landed(data, nil)
	}
	pack, at, err := f.store.locate(ctx, id)
	if err != nil {
		return landed(nil, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl, ok := f.flights[id]; ok {
		return fl
	}
	// A read caches before it leaves flights, so a flight that ended since
	// the first look left its chunk here.
	if data, ok := f.cache.get(id); ok {
		return landed(data, nil)
	}
	fl := &flight{id: id, ctx: ctx, at: at, done: make(chan struct{})}
	f.flights[id] = fl
	if len(f.pending[pack]) == 0 {
		f.order = append(f.order, pack)
	}
	f.pending[pack] = append(f.pending[pack], fl)
	f.dispatch()
	return fl
}

// dispatch starts a read for every pack with pending chunks and no read
// running, oldest first, while worker slots are free. f.mu is held.
func (f *chunkFetcher) dispatch() {
	for i := 0; i < len(f.order) && f.running < f.workers; {
		pack := f.order[i]
		if f.reading[pack] {
			i++
			continue
		}
		f.order = slices.Delete(f.order, i, i+1)
		wants := f.pending[pack]
		delete(f.pending, pack)
		f.reading[pack] = true
		f.running++
		f.reads.Add(1)
		go f.readPack(pack, wants)
	}
}

// readPack reads the chunks wants from the pack named pack in as few ranges
// as getMany needs, then lets the next read start.
func (f *chunkFetcher) readPack(pack string, wants []*flight) {
	defer f.reads.Done()
	ctx := wants[0].ctx
	slices.SortFunc(wants, func(a, b *flight) int { return cmp.Compare(a.at.offset, b.at.offset) })
	entries := make([]packEntry, len(wants))
	for i, fl := range wants {
		entries[i] = fl.at
	}
	next := 0
	// fn never fails, so getMany calls it once for every entry, in order.
	_ = f.store.getMany(ctx, pack, entries, func(_ packEntry, data []byte, err error) error {
		f.land(wants[next], data, err)
		next++
		return nil
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.reading, pack)
	f.running--
	f.dispatch()
}

func (f *chunkFetcher) land(fl *flight, data []byte, err error) {
	fl.data, fl.err = data, err
	if err == nil {
		f.cache.put(fl.id, data)
	}
	f.mu.Lock()
	delete(f.flights, fl.id)
	f.mu.Unlock()
	close(fl.done)
}

// wait returns once every pack read has ended. Called after the last stream
// is closed, when no read starts any more.
func (f *chunkFetcher) wait() { f.reads.Wait() }

// count reports one more chunk read, cache hits included, so progress tracks
// the chunk count the manifest recorded.
func (f *chunkFetcher) count() {
	f.countMu.Lock()
	defer f.countMu.Unlock()
	f.read++
	if f.onRead != nil {
		f.onRead(f.read)
	}
}

// wanted is one chunk of a stream, or the error that stopped the stream, plus
// the bytes it holds of the budget.
type wanted struct {
	fl   *flight
	held int64
	err  error
}

// ready waits until the last stream handed out has asked for all its chunks.
// A caller that opens streams ahead calls it first, so at most one stream
// waits for budget and the streams ahead are bounded by the budget alone.
func (f *chunkFetcher) ready(ctx context.Context) error {
	f.turnMu.Lock()
	turn := f.turn
	f.turnMu.Unlock()
	select {
	case <-turn:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reader returns the content of one file as a stream, fetching chunks in the
// background and yielding them in order.
//
// Streams must be read in the order reader handed them out, as a restore does
// with the files of a snapshot. The caller closes the reader, including when
// it stops early. Pack reads run under the ctx of the stream that wanted
// first, so the streams of one fetcher share ctx; closing a stream early
// cancels none of them.
func (f *chunkFetcher) reader(ctx context.Context, refs []chunkRef) *chunkStream {
	reads := ctx
	ctx, cancel := context.WithCancel(ctx)
	s := &chunkStream{
		pending: make(chan wanted, len(refs)+1), // one spare for the error slot
		cancel:  cancel,
		fetcher: f,
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
			s.pending <- wanted{err: ctx.Err()}
			return
		}
		for _, ref := range refs {
			if err := f.budget.acquire(ctx, ref.Length); err != nil {
				s.pending <- wanted{err: err}
				return
			}
			s.pending <- wanted{fl: f.want(reads, ref.ID), held: ref.Length}
		}
	}()

	return s
}

// chunkStream is the ordered reader over one file's chunks returned by
// chunkFetcher.reader.
type chunkStream struct {
	pending chan wanted
	cancel  context.CancelFunc
	fetcher *chunkFetcher
	current []byte
	err     error
}

func (s *chunkStream) Read(p []byte) (int, error) {
	for len(s.current) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		w, ok := <-s.pending
		if !ok {
			return 0, io.EOF
		}
		err := w.err
		if err == nil {
			<-w.fl.done
			err = w.fl.err
		}
		s.fetcher.budget.release(w.held)
		if err != nil {
			s.err = fmt.Errorf("ark: failed to fetch chunk: %w", err)
			return 0, s.err
		}
		s.fetcher.count()
		s.current = w.fl.data
	}
	n := copy(p, s.current)
	s.current = s.current[n:]
	return n, nil
}

// Close stops the stream from asking for more chunks and gives back the budget
// of those it did not read. It is safe to call more than once and after the
// stream is drained.
func (s *chunkStream) Close() error {
	s.cancel()
	for w := range s.pending {
		s.fetcher.budget.release(w.held)
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
