package ark

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

var _ Backend = (*InstrumentedBackend)(nil)

// InstrumentedBackend wraps a Backend and counts calls, errors, durations and
// bytes per method. It adds no dependency: an embedder reads Stats and feeds
// the numbers to its own metrics system. Create it with [Instrument] and pass
// it to [Init] or [Open] like any Backend.
type InstrumentedBackend struct {
	b Backend

	put, get, getRange, exists, del, list, stat, sync methodCounters
}

// BackendStats is a snapshot of the counters of an [InstrumentedBackend], one
// entry per Backend method. Close is not counted.
type BackendStats struct {
	Put, Get, GetRange, Exists, Delete, List, Stat, Sync MethodStats
}

// MethodStats holds the counters of one Backend method. They only grow.
//
// A call counts once it returns. A Get adds its Duration, Bytes and error once
// its reader is closed, so its Duration covers reading the object and an error
// while reading counts as a Get error; a reader that is never closed shows in
// Calls only. Errors counts calls that returned a non-nil error, including
// ErrNotFound and an error a List callback returned. Duration is the sum over
// all calls, so concurrent calls can make it exceed wall time. Bytes is set
// for Put (bytes the backend read from the caller), Get (bytes the caller
// read from the object) and GetRange (bytes returned) only.
type MethodStats struct {
	Calls    int64
	Errors   int64
	Duration time.Duration
	Bytes    int64
}

// Instrument returns b wrapped in an [InstrumentedBackend] with zeroed
// counters. Closing the wrapper closes b.
func Instrument(b Backend) *InstrumentedBackend {
	return &InstrumentedBackend{b: b}
}

// Stats returns the current counters. It is safe to call from any goroutine,
// also while operations run; Errors never exceeds Calls in the snapshot.
func (ib *InstrumentedBackend) Stats() BackendStats {
	return BackendStats{
		Put:      ib.put.snapshot(),
		Get:      ib.get.snapshot(),
		GetRange: ib.getRange.snapshot(),
		Exists:   ib.exists.snapshot(),
		Delete:   ib.del.snapshot(),
		List:     ib.list.snapshot(),
		Stat:     ib.stat.snapshot(),
		Sync:     ib.sync.snapshot(),
	}
}

// Put forwards to the wrapped backend and counts the bytes it reads from r.
func (ib *InstrumentedBackend) Put(ctx context.Context, h Handle, r io.Reader) error {
	start := time.Now()
	cr := &countingReader{r: r}
	err := ib.b.Put(ctx, h, cr)
	ib.put.record(start, cr.n, err)
	return err
}

// Get forwards to the wrapped backend. The call counts when Get returns; its
// Duration, Bytes and read error are added when the returned reader is closed.
func (ib *InstrumentedBackend) Get(ctx context.Context, h Handle) (io.ReadCloser, error) {
	start := time.Now()
	rc, err := ib.b.Get(ctx, h)
	if err != nil {
		ib.get.record(start, 0, err)
		return nil, err
	}
	ib.get.calls.Add(1)
	return &instrumentedReader{rc: rc, c: &ib.get, start: start}, nil
}

// GetRange forwards to the wrapped backend and counts the bytes it returns.
func (ib *InstrumentedBackend) GetRange(ctx context.Context, h Handle, offset, length int64) ([]byte, error) {
	start := time.Now()
	data, err := ib.b.GetRange(ctx, h, offset, length)
	ib.getRange.record(start, int64(len(data)), err)
	return data, err
}

// Exists forwards to the wrapped backend.
func (ib *InstrumentedBackend) Exists(ctx context.Context, h Handle) (bool, error) {
	start := time.Now()
	ok, err := ib.b.Exists(ctx, h)
	ib.exists.record(start, 0, err)
	return ok, err
}

// Delete forwards to the wrapped backend.
func (ib *InstrumentedBackend) Delete(ctx context.Context, h Handle) error {
	start := time.Now()
	err := ib.b.Delete(ctx, h)
	ib.del.record(start, 0, err)
	return err
}

// Sync forwards to the wrapped backend.
func (ib *InstrumentedBackend) Sync(ctx context.Context) error {
	start := time.Now()
	err := ib.b.Sync(ctx)
	ib.sync.record(start, 0, err)
	return err
}

// List forwards to the wrapped backend. Its Duration includes the time fn
// takes.
func (ib *InstrumentedBackend) List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error {
	start := time.Now()
	err := ib.b.List(ctx, t, fn)
	ib.list.record(start, 0, err)
	return err
}

// Stat forwards to the wrapped backend.
func (ib *InstrumentedBackend) Stat(ctx context.Context, h Handle) (BlobInfo, error) {
	start := time.Now()
	info, err := ib.b.Stat(ctx, h)
	ib.stat.record(start, 0, err)
	return info, err
}

// Close closes the wrapped backend.
func (ib *InstrumentedBackend) Close() error {
	return ib.b.Close()
}

type methodCounters struct {
	calls, errors, nanos, bytes atomic.Int64
}

// record counts calls before errors and snapshot loads them in reverse, so a
// snapshot never shows more errors than calls.
func (c *methodCounters) record(start time.Time, n int64, err error) {
	c.calls.Add(1)
	c.finish(start, n, err)
}

// finish adds the rest of a call whose Calls was already counted.
func (c *methodCounters) finish(start time.Time, n int64, err error) {
	d := time.Since(start)
	if err != nil {
		c.errors.Add(1)
	}
	c.nanos.Add(int64(d))
	c.bytes.Add(n)
}

func (c *methodCounters) snapshot() MethodStats {
	s := MethodStats{
		Errors:   c.errors.Load(),
		Duration: time.Duration(c.nanos.Load()),
		Bytes:    c.bytes.Load(),
	}
	s.Calls = c.calls.Load()
	return s
}

type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// WriteTo keeps the [io.WriterTo] and [io.ReaderFrom] fast paths of r and the
// destination that wrapping r would otherwise hide from [io.Copy].
func (cr *countingReader) WriteTo(w io.Writer) (int64, error) {
	n, err := io.Copy(w, cr.r)
	cr.n += n
	return n, err
}

// instrumentedReader finishes its Get on the first Close, with the bytes read
// and the first read error other than [io.EOF].
type instrumentedReader struct {
	rc     io.ReadCloser
	c      *methodCounters
	start  time.Time
	n      int64
	err    error
	closed bool
}

func (r *instrumentedReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *instrumentedReader) WriteTo(w io.Writer) (int64, error) {
	n, err := io.Copy(w, r.rc)
	r.n += n
	if err != nil && r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *instrumentedReader) Close() error {
	err := r.rc.Close()
	if r.closed {
		return err
	}
	r.closed = true
	recErr := r.err
	if recErr == nil {
		recErr = err
	}
	r.c.finish(r.start, r.n, recErr)
	return err
}
