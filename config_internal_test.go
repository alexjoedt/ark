package ark

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
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

func (b *stubBackend) GetRange(_ context.Context, h Handle, offset, length int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[stubKey(h)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h.Name)
	}
	if offset < 0 || length < 0 || offset > int64(len(data)) || length > int64(len(data))-offset {
		return nil, io.ErrUnexpectedEOF
	}
	return bytes.Clone(data[offset : offset+length]), nil
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

func (b *stubBackend) Sync(context.Context) error { return nil }

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
	// build with other defaults would have written it, and rebind the key.
	stored := chunkerParams{Min: 1 << 16, Avg: 1 << 19, Max: 1 << 21}
	rebindConfig(t, b, func(cfg *repositoryConfig) { cfg.Chunker = stored })

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

// TestOpenRejectsTheFormatBeforePacks opens a repository as the build before
// packs wrote it: format version 2, its key bound to that config. Its chunks
// and tree nodes are one object each, which this build does not read, so
// Open refuses it and names both versions.
func TestOpenRejectsTheFormatBeforePacks(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rebindConfig(t, b, func(cfg *repositoryConfig) { cfg.Version = 2 })

	_, err := Open(ctx, b, WithoutPassword())
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Open = %v, want ErrUnsupportedFormat", err)
	}
	for _, want := range []string{"format version 2", "reads version 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open = %q, want it to name %q", err, want)
		}
	}
}

// rebindConfig rewrites the config object of b with change applied and
// replaces its key file by one bound to the new bytes, as Init would have
// written them.
func rebindConfig(t *testing.T, b Backend, change func(*repositoryConfig)) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	data, err := readRepositoryConfig(ctx, b)
	if err != nil {
		t.Fatalf("readRepositoryConfig: %v", err)
	}
	cfg, err := parseRepositoryConfig(data)
	if err != nil {
		t.Fatalf("parseRepositoryConfig: %v", err)
	}
	change(&cfg)
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := b.Put(ctx, Handle{Type: ConfigFile, Name: configName}, bytes.NewReader(data)); err != nil {
		t.Fatalf("write config: %v", err)
	}
	repo.key.ConfigHash = configHash(data)
	if _, err := putKeyFile(ctx, b, repo.codec, repo.key, WithoutPassword()); err != nil {
		t.Fatalf("putKeyFile: %v", err)
	}
	if err := b.Delete(ctx, Handle{Type: KeyFile, Name: repo.keyName}); err != nil {
		t.Fatalf("delete old key file: %v", err)
	}
}

// TestOpenRejectsAMasterKeyWithoutConfigHash pins that there is no legacy
// path: a master key that carries no config_hash, as format version 1 wrote
// it, does not open the repository and reports the older format.
func TestOpenRejectsAMasterKeyWithoutConfigHash(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	blob, err := json.Marshal(repo.key)
	if err != nil {
		t.Fatalf("marshal master key: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("unmarshal master key: %v", err)
	}
	if _, ok := fields["config_hash"]; !ok {
		t.Fatal("master key has no config_hash field")
	}
	delete(fields, "config_hash")
	if blob, err = json.Marshal(fields); err != nil {
		t.Fatalf("marshal master key: %v", err)
	}
	var wrap [32]byte
	h := Handle{Type: KeyFile, Name: repo.keyName}
	kf, err := json.Marshal(keyFile{
		Version: keyFileVersion,
		KDF:     kdfNone,
		Key:     hex.EncodeToString(repo.codec.seal(&wrap, objectAAD(h), blob)),
	})
	if err != nil {
		t.Fatalf("marshal key file: %v", err)
	}
	if err := b.Put(ctx, h, bytes.NewReader(kf)); err != nil {
		t.Fatalf("put key file: %v", err)
	}

	if _, err := Open(ctx, b, WithoutPassword()); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Open = %v, want ErrUnsupportedFormat", err)
	}
}
