package ark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sync"
	"testing"

	"github.com/alexjoedt/ark/chunker"
)

// stubBackend is a minimal in-package Backend. An internal test cannot import
// arkmem, that would be an import cycle. It is mutex guarded because the
// Backend contract requires it: the lock refresh goroutine writes while the
// test reads.
type stubBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newStubBackend() *stubBackend { return &stubBackend{objects: make(map[string][]byte)} }

func stubKey(h Handle) string { return h.Type.String() + "/" + h.Name }

func (b *stubBackend) Put(_ context.Context, h Handle, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[stubKey(h)] = data
	return nil
}

func (b *stubBackend) Get(_ context.Context, h Handle) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[stubKey(h)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h.Name)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *stubBackend) Exists(_ context.Context, h Handle) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objects[stubKey(h)]
	return ok, nil
}

func (b *stubBackend) Delete(_ context.Context, h Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, stubKey(h))
	return nil
}

func (b *stubBackend) List(_ context.Context, t HandleType, fn func(BlobInfo) error) error {
	b.mu.Lock()
	objects := make(map[string][]byte, len(b.objects))
	maps.Copy(objects, b.objects)
	b.mu.Unlock()

	prefix := t.String() + "/"
	for k, data := range objects {
		if len(k) <= len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		if err := fn(BlobInfo{Name: k[len(prefix):], Size: int64(len(data))}); err != nil {
			return err
		}
	}
	return nil
}

func (b *stubBackend) Stat(_ context.Context, h Handle) (BlobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[stubKey(h)]
	if !ok {
		return BlobInfo{}, fmt.Errorf("%w: %s", ErrNotFound, h.Name)
	}
	return BlobInfo{Name: h.Name, Size: int64(len(data))}, nil
}

func (b *stubBackend) Close() error { return nil }

// TestOpenTakesChunkerParametersFromTheConfig is the check that a repository
// chunks with the parameters it was created with, not with whatever the code
// defaults happen to be in this build.
func TestOpenTakesChunkerParametersFromTheConfig(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()

	want := chunker.Config{MinSize: 1 << 19, AvgSize: 1 << 21, MaxSize: 1 << 23}
	if want == chunker.DefaultConfig() {
		t.Fatal("the test parameters equal the code defaults, pick different ones")
	}
	if err := Init(ctx, b, WithoutPassword(), WithChunkerConfig(want)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	repo, err := Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	want.Seed, want.IDKey = repo.key.ChunkerSeed, repo.key.IDKey
	if repo.chunker != want {
		t.Errorf("repo.chunker = %+v, want %+v", repo.chunker, want)
	}
}

// TestOpenDefaultRepositoryUsesTheWrittenDefaults pins that even the default
// path reads the numbers back from the config rather than calling
// chunker.DefaultConfig at backup time.
func TestOpenDefaultRepositoryUsesTheWrittenDefaults(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Rewrite the config with different, still valid parameters, as an older
	// build with other defaults would have written it.
	stored := chunkerParams{Min: 1 << 16, Avg: 1 << 19, Max: 1 << 21}
	cfg, err := loadRepositoryConfig(ctx, b)
	if err != nil {
		t.Fatalf("loadRepositoryConfig: %v", err)
	}
	cfg.Chunker = stored
	writeConfig(t, b, cfg)

	repo, err := Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	want := stored.toChunkerConfig()
	want.Seed, want.IDKey = repo.key.ChunkerSeed, repo.key.IDKey
	if repo.chunker != want {
		t.Errorf("repo.chunker = %+v, want the stored %+v", repo.chunker, want)
	}
}

// writeConfig replaces the config object of b.
func writeConfig(t *testing.T, b Backend, cfg repositoryConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	h := Handle{Type: ConfigFile, Name: configName}
	if putErr := b.Put(context.Background(), h, bytes.NewReader(data)); putErr != nil {
		t.Fatalf("write config: %v", putErr)
	}
}
