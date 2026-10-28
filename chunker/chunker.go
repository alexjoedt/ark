// Package chunker implements content-defined chunking (CDC) using the FastCDC
// algorithm with gear hashing.
package chunker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math/bits"

	"golang.org/x/crypto/chacha20"
)

// Default chunk size parameters, the format v1 defaults.
// Min/Avg/Max define the FastCDC boundaries in bytes.
//
// The average decides how much a single edit costs on the next backup: one
// changed byte rewrites one chunk. A 1 MiB average is the same order restic
// and borg use. The maximum is the ceiling a restore cache has to budget for
// per worker, so it stays at four times the average rather than growing on
// its own.
const (
	DefaultMinChunk = 256 * 1024      // 256 KiB
	DefaultAvgChunk = 1024 * 1024     // 1 MiB
	DefaultMaxChunk = 4 * 1024 * 1024 // 4 MiB
)

// Config configures the FastCDC chunker.
//
// Seed and IDKey are secrets of the repository, so cut points and chunk names
// of a known file cannot be predicted without them. A repository fills them
// in from its master key; the zero values are valid and only fit tests.
type Config struct {
	MinSize int // Minimum chunk size in bytes
	AvgSize int // Target average chunk size in bytes
	MaxSize int // Maximum chunk size in bytes

	Seed  [32]byte // keys the gear table
	IDKey [32]byte // keys the HMAC-SHA256 that names each chunk
}

// DefaultConfig returns the default chunker configuration.
func DefaultConfig() Config {
	return Config{
		MinSize: DefaultMinChunk,
		AvgSize: DefaultAvgChunk,
		MaxSize: DefaultMaxChunk,
	}
}

// Chunk represents a single chunk produced by the chunker.
type Chunk struct {
	Start  uint64 // Byte offset of this chunk in the original stream
	Data   []byte // Raw chunk data
	Length int    // Same as len(Data)
	Hash   string // HMAC-SHA256 of Data under Config.IDKey, hex
}

// Chunker splits an input stream into variable-size chunks using FastCDC.
// It reads from an [io.Reader] and produces chunks via Next().
type Chunker struct {
	reader io.Reader
	config Config

	// Two-phase masking values derived from the target average size.
	// maskS is used before normalSize (favors larger chunks),
	// maskL is used after normalSize (favors smaller chunks).
	maskS uint64
	maskL uint64

	// normalSize is the threshold between the two masking phases.
	// Set to (avgSize - minSize) / 2 + minSize, centering the distribution.
	normalSize int

	gear *[256]uint64
	mac  hash.Hash // HMAC-SHA256 under Config.IDKey, reset per chunk

	buf []byte // read buffer (sized to maxSize)
	off int    // current offset into buf
	n   int    // number of valid bytes in buf
	eof bool   // true when reader is exhausted
	pos uint64 // absolute byte offset in the stream (start of next chunk)
}

// New creates a new FastCDC chunker that reads from r.
func New(r io.Reader, cfg Config) *Chunker {
	if cfg.MinSize <= 0 {
		cfg.MinSize = DefaultMinChunk
	}
	if cfg.AvgSize <= 0 {
		cfg.AvgSize = DefaultAvgChunk
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = DefaultMaxChunk
	}

	// Compute mask bits from the average chunk size.
	// The number of low bits to match determines the expected interval.
	// For avgSize = 4MB, that's 22 bits.
	avgBits := bits.Len(uint(cfg.AvgSize)) - 1
	// maskS has fewer bits set (harder to match) → longer runs → larger chunks.
	// maskL has more bits set (easier to match) → shorter runs → smaller chunks.
	// The difference is typically 2 bits.
	maskS := buildMask(avgBits + 1)
	maskL := buildMask(avgBits - 1)
	normalSize := cfg.MinSize + (cfg.AvgSize-cfg.MinSize)/2

	return &Chunker{
		reader:     r,
		config:     cfg,
		maskS:      maskS,
		maskL:      maskL,
		normalSize: normalSize,
		gear:       gearTable(&cfg.Seed),
		mac:        hmac.New(sha256.New, cfg.IDKey[:]),
		buf:        make([]byte, cfg.MaxSize),
	}
}

// Reset points c at a new stream and keeps its buffer, so chunking many
// files does not allocate a MaxSize buffer per file.
func (c *Chunker) Reset(r io.Reader) {
	c.reader = r
	c.off, c.n, c.pos = 0, 0, 0
	c.eof = false
}

// Next returns the next chunk from the input stream.
// Returns [io.EOF] when the stream is exhausted.
func (c *Chunker) Next() (Chunk, error) {
	// Fill buffer if needed.
	if err := c.fill(); err != nil {
		return Chunk{}, err
	}
	if c.off >= c.n {
		return Chunk{}, io.EOF
	}

	remaining := c.n - c.off

	// If remaining data is at or below minimum, emit it all as the final chunk.
	if remaining <= c.config.MinSize {
		chunk := c.emit(remaining)
		return chunk, nil
	}

	// FastCDC cut-point detection with two-phase masking.
	// Start scanning after minSize bytes (no cut point before minimum).
	fp := uint64(0)
	i := c.config.MinSize

	// Cap scanning range to available data or maxSize.
	maxScan := min(remaining, c.config.MaxSize)

	normalEnd := min(c.normalSize, maxScan)

	tab := c.gear

	// Phase 1: Before normalSize, use maskS (harder to match → larger chunks).
	for ; i < normalEnd; i++ {
		fp = (fp << 1) + tab[c.buf[c.off+i]]
		if fp&c.maskS == 0 {
			return c.emit(i + 1), nil
		}
	}

	// Phase 2: After normalSize, use maskL (easier to match → smaller chunks).
	for ; i < maxScan; i++ {
		fp = (fp << 1) + tab[c.buf[c.off+i]]
		if fp&c.maskL == 0 {
			return c.emit(i + 1), nil
		}
	}

	// No cut point found within maxSize — emit at maxSize boundary.
	return c.emit(maxScan), nil
}

// emit creates a chunk from c.buf[c.off : c.off+size] and advances the offset.
func (c *Chunker) emit(size int) Chunk {
	start := c.pos
	data := make([]byte, size)
	copy(data, c.buf[c.off:c.off+size])
	c.off += size
	c.pos += uint64(size)

	c.mac.Reset()
	c.mac.Write(data)

	return Chunk{
		Start:  start,
		Data:   data,
		Length: size,
		Hash:   hex.EncodeToString(c.mac.Sum(nil)),
	}
}

// fill ensures the buffer has data to process. It shifts unprocessed data
// to the front and fills the rest from the reader.
func (c *Chunker) fill() error {
	if c.eof && c.off >= c.n {
		return io.EOF
	}

	// If there's still plenty of data, no need to fill.
	if c.n-c.off >= c.config.MaxSize {
		return nil
	}

	// Shift unprocessed data to front.
	if c.off > 0 {
		remaining := c.n - c.off
		copy(c.buf, c.buf[c.off:c.n])
		c.n = remaining
		c.off = 0
	}

	// Read more data from the reader.
	for !c.eof && c.n < len(c.buf) {
		nr, err := c.reader.Read(c.buf[c.n:])
		c.n += nr
		if err != nil {
			if err == io.EOF {
				c.eof = true
				break
			}
			return fmt.Errorf("chunker: read error: %w", err)
		}
	}

	return nil
}

// buildMask creates a bitmask with the specified number of low bits set.
// This is used for the gear-hash cut-point comparison.
func buildMask(b int) uint64 {
	if b >= 64 {
		return ^uint64(0)
	}
	if b <= 0 {
		return 0
	}
	return (1 << b) - 1
}

// gearTable returns the gear lookup table for seed: the first 2048 bytes of
// the ChaCha20 keystream with seed as key and a zero nonce, read as 256
// little-endian uint64.
func gearTable(seed *[32]byte) *[256]uint64 {
	var nonce [chacha20.NonceSize]byte
	c, _ := chacha20.NewUnauthenticatedCipher(seed[:], nonce[:]) // fails only on wrong sizes
	stream := make([]byte, 256*8)
	c.XORKeyStream(stream, stream)

	var tab [256]uint64
	for i := range tab {
		tab[i] = binary.LittleEndian.Uint64(stream[i*8:])
	}
	return &tab
}
