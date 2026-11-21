package ark

import (
	"bytes"
	"context"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/alexjoedt/ark/chunker"
)

// countingBufs stands in for the pool of a chunkWriter. It hands out a fresh
// buffer per get and overwrites a released one, so a reader that is late or a
// second release shows.
type countingBufs struct {
	t    *testing.T
	mu   sync.Mutex
	out  map[*byte]struct{}
	gets int
	puts int
}

func (c *countingBufs) on(w *chunkWriter) *chunkWriter {
	c.out = make(map[*byte]struct{})
	w.alloc, w.release = c.get, c.put
	return w
}

func (c *countingBufs) get(size int) []byte {
	b := make([]byte, size, size+1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	c.out[&b[:1][0]] = struct{}{}
	return b
}

func (c *countingBufs) put(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	key := &b[:1][0]
	if _, ok := c.out[key]; !ok {
		c.t.Errorf("a buffer was released that is not out: released twice, or not from the pool")
	}
	delete(c.out, key)
	for i := range b {
		b[i] = 0xff
	}
}

func (c *countingBufs) check(want int) {
	c.t.Helper()
	if c.gets != want || c.puts != want || len(c.out) != 0 {
		c.t.Errorf("%d buffers taken, %d released, %d still out, want %d taken and released",
			c.gets, c.puts, len(c.out), want)
	}
}

// TestChunkWriterReleasesEveryBufferOnce pins who gives a pooled chunk buffer
// back: the worker once the chunk is sealed, put itself for a chunk the run
// already holds. The stored chunks still read back, so nothing read a buffer
// after its release.
func TestChunkWriterReleasesEveryBufferOnce(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	session := testSession(newStubBackend(), codec, newMasterKey("repo"))
	// Named under the ID key of the store, so the verifying Get below accepts
	// the chunks.
	cfg := chunker.Config{MinSize: 64, AvgSize: 256, MaxSize: 1024, IDKey: [32]byte(session.chunks.space.idKey)}

	data := make([]byte, 32<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}

	bufs := &countingBufs{t: t}
	w := bufs.on(newChunkWriter(ctx, session.chunks, 4))
	cr := w.chunker(cfg)
	first, err := chunkFileData(w, cr, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("chunkFileData: %v", err)
	}
	// The same content again: every chunk takes the dedup return of put.
	second, err := chunkFileData(w, cr, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("chunkFileData: %v", err)
	}
	if err := w.wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(first) < 8 || len(second) != len(first) {
		t.Fatalf("%d and %d chunks, want the same number and at least 8", len(first), len(second))
	}
	bufs.check(2 * len(first))

	if err := session.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	for _, ref := range first {
		got, err := session.chunks.verifying().Get(ctx, ref.ID)
		if err != nil {
			t.Fatalf("Get %s: %v", ref.ID, err)
		}
		if !bytes.Equal(got, data[ref.Offset:ref.Offset+ref.Length]) {
			t.Fatalf("chunk at %d does not hold the source bytes", ref.Offset)
		}
	}
}

// TestChunkWriterReleasesBuffersOnErrors covers the paths that store nothing:
// a failed write, a put after it, and a put under a cancelled context.
func TestChunkWriterReleasesBuffersOnErrors(t *testing.T) {
	codec := newObjectCodec()
	defer codec.Close()
	session := testSession(newStubBackend(), codec, newMasterKey("repo"))

	t.Run("failed write", func(t *testing.T) {
		bufs := &countingBufs{t: t}
		w := bufs.on(newChunkWriter(context.Background(), session.chunks, 1))
		// Not a blob name, so putNamed fails in the worker.
		if err := w.put(chunker.Chunk{Hash: "no name", Data: w.alloc(3), Length: 3}); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := w.wait(); err == nil {
			t.Fatal("wait returned no error for a chunk without a valid name")
		}
		bufs.check(1)
		// The writer failed: put returns the error before it looks at the chunk.
		if err := w.put(chunker.Chunk{Hash: "other", Data: w.alloc(3), Length: 3}); err == nil {
			t.Fatal("put after a failed write returned no error")
		}
		bufs.check(2)
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		bufs := &countingBufs{t: t}
		const n = 200
		for i := range n {
			// A fresh writer per chunk: put either sees the cancelled context
			// or hands the chunk to a worker that does.
			w := bufs.on(newChunkWriter(ctx, session.chunks, 1))
			bufs.out = make(map[*byte]struct{})
			data := w.alloc(8)
			data[0] = byte(i)
			_ = w.put(chunker.Chunk{Hash: session.chunks.name(data), Data: data, Length: 8})
			_ = w.wait()
			if len(bufs.out) != 0 {
				t.Fatalf("chunk %d: buffer not released", i)
			}
		}
		bufs.check(n)
	})
}

// TestChunkBufsReuseByCapacity pins the pool itself: a buffer is less than
// twice its chunk, a released one serves the next chunk of its class, and a
// buffer the pool did not hand out is not taken in.
func TestChunkBufsReuseByCapacity(t *testing.T) {
	p := new(chunkBufs)
	for size, wantCap := range map[int]int{0: 1, 1: 1, 2: 2, 3: 4, 1000: 1024, 1024: 1024, 1025: 2048} {
		if b := p.get(size); len(b) != size || cap(b) != wantCap {
			t.Errorf("get(%d) has len %d cap %d, want cap %d", size, len(b), cap(b), wantCap)
		}
	}

	// A pool drops what it holds at will, so only a hit proves the reuse, and
	// under the race detector it drops at random.
	var reused bool
	for range 100 {
		b := p.get(600)
		b[0] = 1
		p.put(b)
		if c := p.get(1024); c[0] == 1 {
			reused = true
			break
		}
	}
	if !reused {
		t.Error("a released buffer never came back for a chunk of its class")
	}

	foreign := make([]byte, 600)
	foreign[0] = 2
	p.put(foreign)
	for range 100 {
		if b := p.get(600); cap(b) != 1024 || b[0] == 2 {
			t.Fatal("the pool handed out a buffer it did not make")
		}
	}
}
