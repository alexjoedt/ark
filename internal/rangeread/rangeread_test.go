package rangeread_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"testing"
	"time"

	"github.com/alexjoedt/ark/internal/rangeread"
)

// file is an object in memory. A ReadAt longer than one sftp packet returns at
// most shortReply bytes and [io.EOF] when shortReply is set, as pkg/sftp does
// against a server that caps its read replies. A ReadAt far past the end fails
// like OpenSSH, with an error that is no EOF.
type file struct {
	data       []byte
	mode       fs.FileMode
	shortReply int
	stats      int
	longest    int
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.longest = max(f.longest, len(p))
	if off > 1<<40 {
		return 0, errors.New("bad message")
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if f.shortReply > 0 && len(p) > 32<<10 && n > f.shortReply {
		return f.shortReply, io.EOF
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) Stat() (fs.FileInfo, error) {
	f.stats++
	return info{f}, nil
}

type info struct{ f *file }

func (i info) Name() string       { return "object" }
func (i info) Size() int64        { return int64(len(i.f.data)) }
func (i info) Mode() fs.FileMode  { return i.f.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.f.mode.IsDir() }
func (i info) Sys() any           { return nil }

func payload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/251)
	}
	return p
}

func TestReadReturnsTheRange(t *testing.T) {
	data := payload(6 << 20)
	size := int64(len(data))
	for _, tc := range []struct {
		name           string
		shortReply     int
		offset, length int64
		stats          int
	}{
		{name: "one packet", offset: 100, length: 4 << 10},
		{name: "many packets", offset: 100, length: 1 << 20},
		{name: "many packets, short replies", shortReply: 16 << 10, offset: 100, length: 1 << 20},
		{name: "ends at the end, short replies", shortReply: 16 << 10, offset: size - 100_000, length: 100_000},
		{name: "above the unchecked length", offset: 1, length: 5 << 20, stats: 1},
		{name: "zero length", offset: 500, length: 0, stats: 1},
		{name: "zero length at the end", offset: size, length: 0, stats: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &file{data: data, shortReply: tc.shortReply}
			got, err := rangeread.Read(f, tc.offset, tc.length)
			if err != nil {
				t.Fatalf("Read(%d, %d): %v", tc.offset, tc.length, err)
			}
			if got == nil || !bytes.Equal(got, data[tc.offset:tc.offset+tc.length]) {
				t.Errorf("Read(%d, %d) returned the wrong bytes (%d of them)", tc.offset, tc.length, len(got))
			}
			if f.stats != tc.stats {
				t.Errorf("Read(%d, %d) called Stat %d times, want %d", tc.offset, tc.length, f.stats, tc.stats)
			}
		})
	}
}

func TestReadPastTheEndIsUnexpectedEOF(t *testing.T) {
	data := payload(100_000)
	size := int64(len(data))
	for _, tc := range []struct {
		name           string
		offset, length int64
	}{
		{"past the end", size - 10, 11},
		{"past the end, many packets", 10, size},
		{"starting past the end", size + 1, 5},
		{"zero length past the end", size + 1, 0},
		{"huge length", 10, 1 << 62},
		{"huge offset", 1 << 62, 5},
		{"overflowing sum", math.MaxInt64 - 5, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &file{data: data}
			got, err := rangeread.Read(f, tc.offset, tc.length)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("Read(%d, %d) = %d bytes, %v; want io.ErrUnexpectedEOF", tc.offset, tc.length, len(got), err)
			}
			if f.longest > 4<<20 {
				t.Errorf("Read(%d, %d) read into a %d byte buffer", tc.offset, tc.length, f.longest)
			}
		})
	}
}

func TestReadRejectsWhatIsNoRange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           fs.FileMode
		offset, length int64
	}{
		{"negative offset", 0, -1, 5},
		{"negative length", 0, 0, -1},
		{"zero length of a directory", fs.ModeDir, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &file{data: payload(100), mode: tc.mode}
			if got, err := rangeread.Read(f, tc.offset, tc.length); err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("Read(%d, %d) = %d bytes, %v; want an error other than io.ErrUnexpectedEOF",
					tc.offset, tc.length, len(got), err)
			}
		})
	}
}
