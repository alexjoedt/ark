// Package rangeread reads one byte range of an open backend object, the part
// of Backend.GetRange that the file-based backends share.
package rangeread

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
)

// File is what Read needs of an open object; [*os.File] and *sftp.File have it.
type File interface {
	io.ReaderAt
	Stat() (fs.FileInfo, error)
}

const (
	// unchecked is the longest range Read allocates without asking for the
	// object size first. Below it a Stat would cost a network backend one more
	// round trip per read; above it a wrong length could exhaust memory.
	unchecked = 4 << 20

	// packet is one sftp read request. pkg/sftp splits a longer ReadAt into
	// concurrent requests and reports a short reply to one of them as [io.EOF],
	// although only the end of the object is one; a ReadAt of at most one
	// packet repeats the request until the buffer is full.
	packet = 32 << 10
)

// Read returns exactly length bytes of f at offset. A range that ends past
// the object fails with an error wrapping [io.ErrUnexpectedEOF], whatever its
// length, and a zero length still checks offset against the object.
func Read(f File, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, errors.New("invalid range")
	}
	if offset > math.MaxInt64-length {
		return nil, io.ErrUnexpectedEOF
	}
	if length == 0 || length > unchecked {
		if err := inBounds(f, offset, length); err != nil {
			return nil, err
		}
	}
	if length > math.MaxInt {
		return nil, errors.New("range does not fit in memory")
	}

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	for n < len(buf) && (err == nil || errors.Is(err, io.EOF)) {
		end := min(n+packet, len(buf))
		var m int
		m, err = f.ReadAt(buf[n:end], offset+int64(n))
		if m < end-n && (err == nil || errors.Is(err, io.EOF)) {
			err = io.ErrUnexpectedEOF
		}
		n += m
	}
	if n < len(buf) {
		// A server may answer a read far past the end with an error of its own.
		if berr := inBounds(f, offset, length); errors.Is(berr, io.ErrUnexpectedEOF) {
			return nil, berr
		}
		return nil, err
	}
	return buf, nil
}

// inBounds reports whether the range lies within the regular file f.
func inBounds(f File, offset, length int64) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if size := info.Size(); offset+length > size {
		return fmt.Errorf("object has %d bytes: %w", size, io.ErrUnexpectedEOF)
	}
	return nil
}
