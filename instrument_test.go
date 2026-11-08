package ark_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/arktest"
)

func TestInstrumentedBackendConformance(t *testing.T) {
	t.Run("arkmem", func(t *testing.T) {
		arktest.RunBackendTests(t, func(*testing.T) ark.Backend {
			return ark.Instrument(arkmem.New())
		})
	})
	t.Run("arklocal", func(t *testing.T) {
		arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
			b, err := arklocal.New(t.TempDir())
			if err != nil {
				t.Fatalf("arklocal.New: %v", err)
			}
			return ark.Instrument(b)
		})
	})
}

// tally is what tallyBackend saw of one method.
type tally struct{ calls, errors, bytes int64 }

// tallyBackend counts calls, errors and bytes per method as the backend sees
// them, and can fail the next chunk Put or the next chunk Get halfway through
// its stream.
type tallyBackend struct {
	ark.Backend

	mu sync.Mutex
	m  map[string]*tally

	closes            atomic.Int64
	failPut, failRead atomic.Bool
}

func newTallyBackend() *tallyBackend {
	return &tallyBackend{Backend: arkmem.New(), m: make(map[string]*tally)}
}

func (b *tallyBackend) note(method string, n int64, err error, call bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.m[method]
	if t == nil {
		t = &tally{}
		b.m[method] = t
	}
	if call {
		t.calls++
	}
	if err != nil {
		t.errors++
	}
	t.bytes += n
}

func (b *tallyBackend) get(method string) tally {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.m[method]; t != nil {
		return *t
	}
	return tally{}
}

func (b *tallyBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err == nil {
		if h.Type == ark.BlobFile && b.failPut.CompareAndSwap(true, false) {
			err = errInjected
		} else {
			err = b.Backend.Put(ctx, h, bytes.NewReader(data))
		}
	}
	b.note("Put", int64(len(data)), err, true)
	return err
}

func (b *tallyBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	rc, err := b.Backend.Get(ctx, h)
	b.note("Get", 0, err, true)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	var r io.Reader = bytes.NewReader(data)
	if h.Type == ark.BlobFile && b.failRead.CompareAndSwap(true, false) {
		r = io.MultiReader(bytes.NewReader(data[:len(data)/2]), errorReader{})
	}
	return &tallyReader{r: r, b: b}, nil
}

func (b *tallyBackend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	ok, err := b.Backend.Exists(ctx, h)
	b.note("Exists", 0, err, true)
	return ok, err
}

func (b *tallyBackend) Delete(ctx context.Context, h ark.Handle) error {
	err := b.Backend.Delete(ctx, h)
	b.note("Delete", 0, err, true)
	return err
}

func (b *tallyBackend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	err := b.Backend.List(ctx, t, fn)
	b.note("List", 0, err, true)
	return err
}

func (b *tallyBackend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	info, err := b.Backend.Stat(ctx, h)
	b.note("Stat", 0, err, true)
	return info, err
}

func (b *tallyBackend) Sync(ctx context.Context) error {
	err := b.Backend.Sync(ctx)
	b.note("Sync", 0, err, true)
	return err
}

func (b *tallyBackend) Close() error {
	b.closes.Add(1)
	return b.Backend.Close()
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errInjected }

// tallyReader adds the bytes read to Get and counts the first read error as a
// Get error.
type tallyReader struct {
	r      io.Reader
	b      *tallyBackend
	failed bool
}

func (r *tallyReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	var noted error
	if err != nil && !errors.Is(err, io.EOF) && !r.failed {
		r.failed = true
		noted = err
	}
	r.b.note("Get", int64(n), noted, false)
	return n, err
}

func (r *tallyReader) Close() error { return nil }

func TestInstrumentedBackendCountsWhatTheBackendSaw(t *testing.T) {
	ctx := context.Background()
	tb := newTallyBackend()
	ib := ark.Instrument(tb)
	if err := ark.Init(ctx, ib, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, ib, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	src := multiSource{name: "inst", dirs: []string{"d"}, files: map[string]string{}}
	for i := range 8 {
		p := fmt.Sprintf("d/f%d", i)
		src.files[p] = strings.Repeat(fmt.Sprintf("file %d line\n", i), 2000+i*500)
		src.order = append(src.order, p)
	}

	stop := make(chan struct{})
	polled := make(chan error, 1)
	go func() {
		defer close(polled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for name, m := range methodStats(ib.Stats()) {
				if m.Errors > m.Calls {
					polled <- fmt.Errorf("%s: %d errors > %d calls", name, m.Errors, m.Calls)
					return
				}
			}
		}
	}()

	tb.failPut.Store(true)
	if _, err := repo.Backup(ctx, src); !errors.Is(err, errInjected) {
		t.Fatalf("Backup with failing Put: err = %v, want the injected error", err)
	}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	tb.failRead.Store(true)
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}); err == nil {
		t.Fatal("Restore with a failing chunk read succeeded")
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for p, want := range src.files {
		if sink.written[p] != want {
			t.Fatalf("restored %s differs", p)
		}
	}

	close(stop)
	if err := <-polled; err != nil {
		t.Fatalf("concurrent Stats: %v", err)
	}

	for name, m := range methodStats(ib.Stats()) {
		want := tb.get(name)
		got := tally{calls: m.Calls, errors: m.Errors, bytes: m.Bytes}
		if got != want {
			t.Errorf("%s: wrapper %+v, backend saw %+v", name, got, want)
		}
		if m.Calls > 0 && m.Duration <= 0 {
			t.Errorf("%s: %d calls but no duration", name, m.Calls)
		}
	}
	stats := ib.Stats()
	if stats.Put.Errors == 0 || stats.Get.Errors == 0 {
		t.Errorf("Put errors %d, Get errors %d, want the injected ones counted", stats.Put.Errors, stats.Get.Errors)
	}
	if stats.Put.Bytes == 0 || stats.Get.Bytes == 0 {
		t.Errorf("Put bytes %d, Get bytes %d, want both counted", stats.Put.Bytes, stats.Get.Bytes)
	}

	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := tb.closes.Load(); n != 1 {
		t.Fatalf("wrapped backend closed %d times, want 1", n)
	}
}

func methodStats(s ark.BackendStats) map[string]ark.MethodStats {
	return map[string]ark.MethodStats{
		"Put": s.Put, "Get": s.Get, "Exists": s.Exists, "Delete": s.Delete,
		"List": s.List, "Stat": s.Stat, "Sync": s.Sync,
	}
}
