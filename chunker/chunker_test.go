package chunker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"testing"

	"golang.org/x/crypto/chacha20"
)

func TestGearTableIsTheChaCha20Keystream(t *testing.T) {
	seed := [32]byte{1, 2, 3}
	var nonce [chacha20.NonceSize]byte
	c, err := chacha20.NewUnauthenticatedCipher(seed[:], nonce[:])
	if err != nil {
		t.Fatal(err)
	}
	stream := make([]byte, 2048)
	c.XORKeyStream(stream, stream)

	tab := gearTable(&seed)
	if tab[0] != binary.LittleEndian.Uint64(stream) || tab[255] != binary.LittleEndian.Uint64(stream[2040:]) {
		t.Error("gear table is not the little-endian ChaCha20 keystream of the seed")
	}
}

// cuts returns the chunk ends and names cfg produces for data.
func cuts(t *testing.T, cfg Config, data []byte) ([]uint64, []string) {
	t.Helper()
	c := New(nil, cfg)
	c.Reset(sliceReader(data))
	var ends []uint64
	var names []string
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			return ends, names
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ends = append(ends, ch.Start+uint64(ch.Length))
		names = append(names, ch.Hash)

		mac := hmac.New(sha256.New, cfg.IDKey[:])
		mac.Write(ch.Data)
		if ch.Hash != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("chunk at %d is not named by the HMAC of its data", ch.Start)
		}
	}
}

func sliceReader(b []byte) io.Reader { return &reader{b: b} }

type reader struct{ b []byte }

func (r *reader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestCutPointsAndNamesFollowTheKeys(t *testing.T) {
	data := make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	base := Config{MinSize: 2 << 10, AvgSize: 8 << 10, MaxSize: 32 << 10}

	a := base
	a.Seed, a.IDKey = [32]byte{1}, [32]byte{2}
	b := base
	b.Seed, b.IDKey = [32]byte{3}, [32]byte{4}

	endsA, namesA := cuts(t, a, data)
	endsA2, _ := cuts(t, a, data)
	endsB, namesB := cuts(t, b, data)

	if !slices.Equal(endsA, endsA2) {
		t.Error("the same seed cut the same data differently")
	}
	if slices.Equal(endsA, endsB) {
		t.Error("two seeds cut the data at the same points")
	}
	for _, n := range namesB {
		if slices.Contains(namesA, n) {
			t.Errorf("chunk name %s appears under both keys", n)
		}
	}
}

// TestDataBelongsToTheCallerUnlessAllocIsSet pins the ownership of Data:
// by default every chunk has memory of its own that later calls of Next leave
// alone, and with SetAlloc the chunk is the buffer alloc returned.
func TestDataBelongsToTheCallerUnlessAllocIsSet(t *testing.T) {
	data := make([]byte, 256<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	cfg := Config{MinSize: 2 << 10, AvgSize: 8 << 10, MaxSize: 32 << 10}

	c := New(&reader{b: data}, cfg)
	var chunks []Chunk
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	var joined []byte
	for _, chunk := range chunks {
		joined = append(joined, chunk.Data...)
	}
	if !slices.Equal(joined, data) {
		t.Error("chunks kept after later Next calls no longer hold the stream")
	}

	buf := make([]byte, cfg.MaxSize)
	var calls int
	c = New(&reader{b: data}, cfg)
	c.SetAlloc(func(size int) []byte {
		calls++
		return buf[:size]
	})
	for i, want := range chunks {
		chunk, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if &chunk.Data[0] != &buf[0] || !slices.Equal(chunk.Data, want.Data) || chunk.Hash != want.Hash {
			t.Fatalf("chunk %d is not the buffer from alloc with the same content and name", i)
		}
	}
	if calls != len(chunks) {
		t.Errorf("alloc ran %d times for %d chunks", calls, len(chunks))
	}
}

// BenchmarkChunker measures cut point detection and the chunk HMAC over
// random data with the default bounds.
func BenchmarkChunker(b *testing.B) {
	data := make([]byte, 64<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	c := New(nil, Config{Seed: [32]byte{1}, IDKey: [32]byte{2}})

	b.SetBytes(int64(len(data)))
	for b.Loop() {
		c.Reset(sliceReader(data))
		for {
			if _, err := c.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatalf("Next: %v", err)
			}
		}
	}
}
