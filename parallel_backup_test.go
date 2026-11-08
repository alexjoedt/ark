package ark_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
)

// concSource yields dirs and files in a fixed order. With concurrent set it is
// an ark.ConcurrentSource whose opens may run after fn returned; each open
// then runs hook first, which can block, fail or cancel.
type concSource struct {
	name       string
	concurrent bool
	files      []concFile
	hook       func(i int) error
}

type concFile struct {
	path    string
	content []byte
}

func (s *concSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: ark.SourceTypeFile}
}

func (s *concSource) ConcurrentOpen() bool { return s.concurrent }

func (s *concSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	for _, dir := range []string{"a", "b"} {
		if err := fn(ark.FileInfo{Path: dir, Type: ark.FileTypeDirectory, Mode: 0o755}, nil); err != nil {
			return err
		}
	}
	for i, f := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info := ark.FileInfo{Path: f.path, Size: int64(len(f.content)), Mode: 0o644, Type: ark.FileTypeRegular}
		err := fn(info, func() (io.ReadCloser, error) {
			if s.hook != nil {
				if err := s.hook(i); err != nil {
					return nil, err
				}
			}
			return io.NopCloser(bytes.NewReader(f.content)), nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func concFiles(n int) []concFile {
	rng := rand.New(rand.NewPCG(1, 2))
	files := make([]concFile, n)
	for i := range files {
		data := make([]byte, rng.IntN(64<<10))
		for j := range data {
			data[j] = byte(rng.Uint32())
		}
		files[i] = concFile{path: fmt.Sprintf("%s/f%03d", []string{"a", "b"}[i%2], i), content: data}
	}
	return files
}

func openConcRepo(t *testing.T, workers int) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(workers))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestParallelBackupMatchesSequential(t *testing.T) {
	ctx := context.Background()
	const workers = 4
	repo := openConcRepo(t, workers)
	files := concFiles(40)

	// The first file cannot be read before the fourth is opened: the readers
	// run in parallel and finish out of walk order.
	fourthOpened := make(chan struct{})
	par := &concSource{name: "par", concurrent: true, files: files, hook: func(i int) error {
		switch i {
		case 0:
			select {
			case <-fourthOpened:
			case <-time.After(5 * time.Second):
				return errors.New("file 3 was not opened while file 0 was pending")
			}
		case workers - 1:
			close(fourthOpened)
		}
		return nil
	}}
	parSnap, err := repo.Backup(ctx, par)
	if err != nil {
		t.Fatalf("parallel Backup: %v", err)
	}
	seqSnap, err := repo.Backup(ctx, &concSource{name: "seq", files: files})
	if err != nil {
		t.Fatalf("sequential Backup: %v", err)
	}

	parRoot, err := ark.RootTree(ctx, repo, parSnap.ID)
	if err != nil {
		t.Fatal(err)
	}
	seqRoot, err := ark.RootTree(ctx, repo, seqSnap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parRoot != seqRoot {
		t.Errorf("parallel root tree %s, sequential %s", parRoot, seqRoot)
	}
	ps, ss := parSnap.Stats, seqSnap.Stats
	if ps.FileCount != ss.FileCount || ps.TotalSize != ss.TotalSize || ps.ChunksTotal != ss.ChunksTotal {
		t.Errorf("parallel stats %+v, sequential %+v", ps, ss)
	}
	if ss.ChunksNew != 0 {
		t.Errorf("sequential run stored %d new chunks, want every chunk deduplicated", ss.ChunksNew)
	}
}

func TestParallelBackupStopsAllReaders(t *testing.T) {
	errOpen := errors.New("open failed")
	cases := map[string]struct {
		hook func(cancel context.CancelFunc) func(i int) error
		want error
	}{
		"open error": {want: errOpen, hook: func(context.CancelFunc) func(int) error {
			return func(i int) error {
				if i == 7 {
					return errOpen
				}
				return nil
			}
		}},
		"cancel": {want: context.Canceled, hook: func(cancel context.CancelFunc) func(int) error {
			var once sync.Once
			return func(i int) error {
				if i >= 7 {
					once.Do(cancel)
				}
				return nil
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := openConcRepo(t, 4)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := runtime.NumGoroutine()

			src := &concSource{name: "src", concurrent: true, files: concFiles(40), hook: tc.hook(cancel)}
			_, err := repo.Backup(ctx, src)
			if err == nil {
				t.Fatal("Backup succeeded although it was stopped")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Backup error %v, want %v", err, tc.want)
			}

			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if after := runtime.NumGoroutine(); after > before {
				t.Errorf("%d goroutines before the backup, %d after it was stopped", before, after)
			}
		})
	}
}

func TestBackupChunkErrorSameOnBothPaths(t *testing.T) {
	errOpen := errors.New("open failed")
	hook := func(i int) error {
		if i == 7 {
			return errOpen
		}
		return nil
	}
	msgs := map[bool]string{}
	for _, concurrent := range []bool{true, false} {
		src := &concSource{name: "src", concurrent: concurrent, files: concFiles(10), hook: hook}
		_, err := openConcRepo(t, 4).Backup(context.Background(), src)
		if !errors.Is(err, errOpen) {
			t.Fatalf("concurrent=%v: Backup error %v, want %v", concurrent, err, errOpen)
		}
		msgs[concurrent] = err.Error()
	}
	if msgs[true] != msgs[false] {
		t.Errorf("parallel error %q, sequential %q", msgs[true], msgs[false])
	}
}
