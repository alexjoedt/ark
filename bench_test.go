package ark_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// benchFiles is the benchmark data set, built once: 256 files of 16 to
// 128 KiB in 16 directories and six of 4 MiB, about 42 MiB. Every other file
// is random and does not compress, the rest is text from a small vocabulary.
var benchFiles = sync.OnceValue(func() map[string][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	vocab := make([][]byte, 1000)
	for i := range vocab {
		w := make([]byte, 3+rng.IntN(7))
		for j := range w {
			w[j] = 'a' + byte(rng.IntN(26))
		}
		vocab[i] = w
	}
	content := func(i, size int) []byte {
		if i%2 == 0 {
			b := make([]byte, size)
			for j := range b {
				b[j] = byte(rng.Uint32())
			}
			return b
		}
		var buf bytes.Buffer
		for buf.Len() < size {
			buf.Write(vocab[rng.IntN(len(vocab))])
			buf.WriteByte(' ')
		}
		return buf.Bytes()[:size]
	}

	files := make(map[string][]byte)
	for i := range 256 {
		files[fmt.Sprintf("d%02d/f%03d", i%16, i)] = content(i, (16+rng.IntN(112))<<10)
	}
	for i := range 6 {
		files[fmt.Sprintf("big%d", i)] = content(i, 4<<20)
	}
	return files
})

// benchSource yields benchFiles, its directories first.
type benchSource struct{ files map[string][]byte }

func (benchSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "bench", Type: ark.SourceTypeFile}
}

func (s benchSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 16 {
		dir := ark.FileInfo{Path: fmt.Sprintf("d%02d", i), Type: ark.FileTypeDirectory, Mode: 0o755, ModTime: mtime}
		if err := fn(dir, nil); err != nil {
			return err
		}
	}
	for path, data := range s.files {
		info := ark.FileInfo{Path: path, Type: ark.FileTypeRegular, Mode: 0o644, Size: int64(len(data)), ModTime: mtime}
		if err := fn(info, openReader(bytes.NewReader(data))); err != nil {
			return err
		}
	}
	return nil
}

func (s benchSource) size() int64 {
	var n int64
	for _, data := range s.files {
		n += int64(len(data))
	}
	return n
}

// benchRepo opens a fresh repository on arkmem, so the numbers are the
// library's CPU cost without disk or network.
func benchRepo(b *testing.B) *ark.Repository {
	b.Helper()
	return benchRepoOn(b, arkmem.New())
}

// benchRepoOn opens a fresh repository on backend.
func benchRepoOn(b *testing.B, backend ark.Backend) *ark.Repository {
	b.Helper()
	ctx := context.Background()
	if err := ark.Init(ctx, backend, ark.WithoutPassword()); err != nil {
		b.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, backend, ark.WithoutPassword(), ark.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	return repo
}

// BenchmarkBackup measures a first backup into an empty repository and a
// second backup of unchanged data, which chunks and hashes everything again
// but stores nothing.
func BenchmarkBackup(b *testing.B) {
	ctx := context.Background()
	src := benchSource{files: benchFiles()}

	first := func(b *testing.B, backend func() ark.Backend) {
		b.SetBytes(src.size())
		for b.Loop() {
			b.StopTimer()
			repo := benchRepoOn(b, backend())
			b.StartTimer()
			if _, err := repo.Backup(ctx, src); err != nil {
				b.Fatalf("Backup: %v", err)
			}
			b.StopTimer()
			_ = repo.Close()
			b.StartTimer()
		}
	}
	b.Run("first", func(b *testing.B) {
		first(b, func() ark.Backend { return arkmem.New() })
	})
	// The instrumented run shows the wrapper's overhead: two clock reads and a
	// few atomics per backend call, about 640 calls here, so well below 1% of
	// "first". On a host without a vDSO clock (clocksource acpi_pm) every
	// clock read is a syscall of several microseconds and the gap grows to
	// about 1%.
	b.Run("first-instrumented", func(b *testing.B) {
		first(b, func() ark.Backend { return ark.Instrument(arkmem.New()) })
	})

	b.Run("unchanged", func(b *testing.B) {
		repo := benchRepo(b)
		defer repo.Close()
		if _, err := repo.Backup(ctx, src); err != nil {
			b.Fatalf("Backup: %v", err)
		}
		b.SetBytes(src.size())
		for b.Loop() {
			if _, err := repo.Backup(ctx, src); err != nil {
				b.Fatalf("Backup: %v", err)
			}
		}
	})
}

// BenchmarkRestore measures streaming a snapshot into a sink that discards
// the bytes: reading, opening and decompressing every chunk.
func BenchmarkRestore(b *testing.B) {
	ctx := context.Background()
	src := benchSource{files: benchFiles()}
	repo := benchRepo(b)
	defer repo.Close()
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		b.Fatalf("Backup: %v", err)
	}

	b.SetBytes(src.size())
	for b.Loop() {
		if _, err := repo.Restore(ctx, snap.ID, discardSink{}); err != nil {
			b.Fatalf("Restore: %v", err)
		}
	}
}

// BenchmarkIndexLoad is the budget of the on-demand index: the first lookup
// of a repository of a million blobs, 16 to a pack as 1 MiB chunks fill one,
// loads and merges 20 index objects in at most a second and keeps at most 48
// bytes of heap per blob.
func BenchmarkIndexLoad(b *testing.B) {
	const (
		blobs, perPack = 1_000_000, 16
		maxHeapPerBlob = 48
		maxLoad        = time.Second
	)
	ctx := context.Background()
	repo := benchRepo(b)
	if err := ark.WriteSyntheticIndex(ctx, repo, blobs, perPack); err != nil {
		b.Fatalf("write the index: %v", err)
	}

	var before, after runtime.MemStats
	var heap float64
	for b.Loop() {
		b.StopTimer()
		runtime.GC()
		runtime.ReadMemStats(&before)
		b.StartTimer()

		n, table, err := ark.LoadDataIndex(ctx, repo)

		b.StopTimer()
		if err != nil || n != blobs {
			b.Fatalf("loaded %d blobs, want %d: %v", n, blobs, err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		heap = max(heap, float64(after.HeapAlloc)-float64(before.HeapAlloc))
		runtime.KeepAlive(table)
		b.StartTimer()
	}

	perBlob := heap / blobs
	b.ReportMetric(perBlob, "heap-B/blob")
	if perBlob > maxHeapPerBlob {
		b.Errorf("the loaded index keeps %.1f B of heap per blob, the budget is %d", perBlob, maxHeapPerBlob)
	}
	if load := b.Elapsed() / time.Duration(b.N); load > maxLoad {
		b.Errorf("loading and merging took %v, the budget is %v", load, maxLoad)
	}
}
