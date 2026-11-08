package ark

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// SourceTypeStream identifies a snapshot of one byte stream, made by
// [NewStreamSource] and restored by [NewStreamSink].
const SourceTypeStream SourceType = "stream"

// StreamSource is a Source over one [io.Reader], such as the stdout of a
// database dump. See [NewStreamSource].
type StreamSource struct {
	name     string
	r        io.Reader
	consumed bool
}

// NewStreamSource returns a Source that yields exactly one regular file named
// name whose content is read from r until EOF; the length need not be known.
// The file has mode 0600, the process UID and GID and a zero ModTime, so every
// backup reads the whole stream again: there is no unchanged-file skip for
// streams, unchanged bytes are deduplicated by their chunks instead.
//
// name is also the snapshot's source name and must be a clean base name: not
// empty, no slash, not "." or "..". Walk rejects any other name. r is read
// once, by the first Walk; a later Walk fails instead of yielding an empty
// file. r is never closed; the caller owns it.
func NewStreamSource(name string, r io.Reader) *StreamSource {
	return &StreamSource{name: name, r: r}
}

// Meta names the snapshot after the stream.
func (s *StreamSource) Meta() SourceMeta {
	return SourceMeta{Name: s.name, Type: SourceTypeStream}
}

// Walk yields the stream as one regular file.
func (s *StreamSource) Walk(ctx context.Context, fn WalkFunc) error {
	if s.name == "" || s.name == "." || s.name == ".." || strings.Contains(s.name, "/") {
		return fmt.Errorf("ark: stream name %q is not a clean base name", s.name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.consumed {
		return fmt.Errorf("ark: stream %q was already read by an earlier Walk", s.name)
	}
	s.consumed = true
	info := FileInfo{
		Path: s.name,
		Mode: 0o600,
		UID:  os.Getuid(),
		GID:  os.Getgid(),
		Type: FileTypeRegular,
	}
	return fn(info, func() (io.ReadCloser, error) { return io.NopCloser(s.r), nil })
}

// StreamSink is a Sink that writes one regular file to an [io.Writer]. See
// [NewStreamSink].
type StreamSink struct {
	w    io.Writer
	file string
}

// NewStreamSink returns a Sink that writes the bytes of the one regular file
// of a [SourceTypeStream] snapshot to w. Directories are ignored; a second
// regular file, a symlink, a hardlink or any other entry fails the restore
// with an error naming it. w is never closed; the caller owns it.
func NewStreamSink(w io.Writer) *StreamSink {
	return &StreamSink{w: w}
}

// Open accepts stream snapshots only; each Open starts a new restore.
func (s *StreamSink) Open(_ context.Context, meta SourceMeta) error {
	if meta.Type != SourceTypeStream {
		return fmt.Errorf("%w: expected %q, got %q", ErrIncompatibleSourceType, SourceTypeStream, meta.Type)
	}
	s.file = ""
	return nil
}

// Receive copies the regular file to w.
func (s *StreamSink) Receive(_ context.Context, entry FileInfo, r io.Reader) error {
	switch {
	case entry.Type == FileTypeDirectory:
		return nil
	case entry.Type != FileTypeRegular:
		return fmt.Errorf("ark: stream sink: %s is a %s entry, want one regular file", entry.Path, entry.Type)
	case s.file != "":
		return fmt.Errorf("ark: stream sink: second regular file %s after %s", entry.Path, s.file)
	}
	s.file = entry.Path
	_, err := io.Copy(s.w, r)
	return err
}

// Close does nothing; w stays open.
func (s *StreamSink) Close() error { return nil }
