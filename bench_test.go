package ark_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
		if err := fn(info, bytes.NewReader(data)); err != nil {
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
	ctx := context.Background()
	backend := arkmem.New()
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

	b.Run("first", func(b *testing.B) {
		b.SetBytes(src.size())
		for b.Loop() {
			b.StopTimer()
			repo := benchRepo(b)
			b.StartTimer()
			if _, err := repo.Backup(ctx, src); err != nil {
				b.Fatalf("Backup: %v", err)
			}
			b.StopTimer()
			_ = repo.Close()
			b.StartTimer()
		}
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
