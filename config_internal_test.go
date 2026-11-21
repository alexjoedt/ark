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
	if err := Init(ctx, b, NoPassword(), WithChunkerConfig(want)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	repo, err := Open(ctx, b, NoPassword())
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
	if err := Init(ctx, b, NoPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Rewrite the config with different, still valid parameters, as an older
	// build with other defaults would have written it, and rebind the key.
	stored := chunkerParams{Min: 1 << 16, Avg: 1 << 19, Max: 1 << 21}
	rebindConfig(t, b, func(cfg *repositoryConfig) { cfg.Chunker = stored })

	repo, err := Open(ctx, b, NoPassword())
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
// packs wrote it: format version 3, its key bound to that config. Its chunks
// and tree nodes are one object each, which this build does not read, so
// Open refuses it and names both versions.
func TestOpenRejectsTheFormatBeforePacks(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, NoPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rebindConfig(t, b, func(cfg *repositoryConfig) { cfg.Version = 3 })

	_, err := Open(ctx, b, NoPassword())
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Open = %v, want ErrUnsupportedFormat", err)
	}
	for _, want := range []string{"format version 3", "reads version 1"} {
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
	rebindConfigBytes(t, b, func(data []byte) []byte {
		cfg, err := parseRepositoryConfig(data)
		if err != nil {
			t.Fatalf("parseRepositoryConfig: %v", err)
		}
		change(&cfg)
		if data, err = json.Marshal(cfg); err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		return data
	})
}

// rebindConfigBytes is rebindConfig for a change to the stored bytes.
func rebindConfigBytes(t *testing.T, b Backend, change func([]byte) []byte) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(ctx, b, NoPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	data, err := readRepositoryConfig(ctx, b)
	if err != nil {
		t.Fatalf("readRepositoryConfig: %v", err)
	}
	data = change(data)
	if err := b.Put(ctx, Handle{Type: ConfigFile, Name: configName}, bytes.NewReader(data)); err != nil {
		t.Fatalf("write config: %v", err)
	}
	repo.key.ConfigHash = configHash(data)
	if _, err := putKeyFile(ctx, b, repo.codec, repo.key, NoPassword()); err != nil {
		t.Fatalf("putKeyFile: %v", err)
	}
	if err := b.Delete(ctx, Handle{Type: KeyFile, Name: repo.keyName}); err != nil {
		t.Fatalf("delete old key file: %v", err)
	}
}

// TestOpenRejectsAMasterKeyWithoutConfigHash pins that there is no legacy
// path: a master key that carries no config_hash, as an older format wrote
// it, does not open the repository and reports the older format.
func TestOpenRejectsAMasterKeyWithoutConfigHash(t *testing.T) {
	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, NoPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, NoPassword())
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
	resealMasterKey(t, b, repo, blob)

	if _, err := Open(ctx, b, NoPassword()); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Open = %v, want ErrUnsupportedFormat", err)
	}
}

// resealMasterKey replaces the key file of repo by one that seals blob.
func resealMasterKey(t *testing.T, b Backend, repo *Repository, blob []byte) {
	t.Helper()
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
	if err := b.Put(context.Background(), h, bytes.NewReader(kf)); err != nil {
		t.Fatalf("put key file: %v", err)
	}
}

// TestOpenRefusesUnknownKeys: the config and the master key are decoded
// strictly. A key this build does not know, in the object or in a nested one,
// is something a later build wrote for a reader to act on, so Open refuses
// the repository and names the key instead of dropping it. The key file
// header is a row of TestOpenRejectsUnknownKeyFiles. A master key sealed
// under a format byte this build does not know is refused the same way, not
// taken for a wrong password.
func TestOpenRefusesUnknownKeys(t *testing.T) {
	ctx := context.Background()
	later := func(data []byte, before string) []byte {
		return bytes.Replace(data, []byte(before), []byte(`"later":true,`+before), 1)
	}
	const laterKey = `key "later"`
	for name, tc := range map[string]struct {
		change func(t *testing.T, b Backend)
		names  string
	}{
		"config": {func(t *testing.T, b Backend) {
			rebindConfigBytes(t, b, func(data []byte) []byte { return later(data, `"version"`) })
		}, laterKey},
		"config chunker": {func(t *testing.T, b Backend) {
			rebindConfigBytes(t, b, func(data []byte) []byte { return later(data, `"min"`) })
		}, laterKey},
		"master key format byte": {func(t *testing.T, b Backend) {
			repo, err := Open(ctx, b, NoPassword())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer repo.Close()
			kf, err := readKeyFile(ctx, b, repo.keyName)
			if err != nil {
				t.Fatalf("readKeyFile: %v", err)
			}
			sealed, _ := hex.DecodeString(kf.Key)
			var wrap [32]byte
			h := Handle{Type: KeyFile, Name: repo.keyName}
			if sealed, err = retag(&wrap, objectAAD(h), sealed, 0x04); err != nil {
				t.Fatalf("retag: %v", err)
			}
			kf.Key = hex.EncodeToString(sealed)
			data, _ := json.Marshal(kf)
			if err := b.Put(ctx, h, bytes.NewReader(data)); err != nil {
				t.Fatalf("put key file: %v", err)
			}
		}, "0x04"},
		"master key": {func(t *testing.T, b Backend) {
			repo, err := Open(ctx, b, NoPassword())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer repo.Close()
			blob, err := json.Marshal(repo.key)
			if err != nil {
				t.Fatalf("marshal master key: %v", err)
			}
			resealMasterKey(t, b, repo, later(blob, `"data_key"`))
		}, laterKey},
	} {
		t.Run(name, func(t *testing.T) {
			b := newStubBackend()
			if err := Init(ctx, b, NoPassword()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			tc.change(t, b)

			_, err := Open(ctx, b, NoPassword())
			if !errors.Is(err, ErrUnsupportedFormat) || errors.Is(err, ErrCorruptConfig) {
				t.Fatalf("Open = %v, want ErrUnsupportedFormat alone", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("Open = %q, want it to name %s", err, tc.names)
			}
		})
	}
}

// TestDecodeStrict pins what the readers of config, key file, manifest and
// lock rely on: an unknown key is ErrUnsupportedFormat and named, at any
// depth, and input [json.Unmarshal] refuses stays refused, as a plain error.
// The unknown key is told from the text of encoding/json's error, so this
// test is what notices a Go release that words it differently.
func TestDecodeStrict(t *testing.T) {
	for in, unsupported := range map[string]bool{
		`{"version":1,"chunker":{"min":1}}`:           false,
		`{"version":1,"later":0}`:                     true,
		`{"version":1,"chunker":{"min":1,"later":0}}`: true,
	} {
		var cfg repositoryConfig
		err := decodeStrict("config", []byte(in), &cfg)
		if unsupported != errors.Is(err, ErrUnsupportedFormat) || (!unsupported && err != nil) {
			t.Errorf("%s: decodeStrict = %v, want ErrUnsupportedFormat: %v", in, err, unsupported)
		}
		if unsupported && !strings.Contains(err.Error(), `config has the key "later"`) {
			t.Errorf("%s: decodeStrict = %q, want it to name the object and the key", in, err)
		}
	}
	for _, in := range []string{`{"version":1}{}`, `{"version":1}}`, `{"version":"1"}`, `{"version":1`, ``} {
		var cfg repositoryConfig
		if err := decodeStrict("config", []byte(in), &cfg); err == nil || errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%q: decodeStrict = %v, want an error that is not ErrUnsupportedFormat", in, err)
		}
	}
}
