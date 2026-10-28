package ark_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
)

// readConfig returns the raw config object of b.
func readConfig(t *testing.T, b ark.Backend) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), ark.Handle{Type: ark.ConfigFile, Name: "config"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return data
}

func TestInitWritesTheConfigSchema(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	before := time.Now().UTC().Add(-time.Second)

	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	data := readConfig(t, b)
	if data[0] != '{' {
		t.Errorf("config starts with %q, want plain JSON with no header byte", data[0])
	}

	// Field order is part of the format promise.
	wantOrder := []string{`"version"`, `"id"`, `"created_at"`, `"chunker"`, `"compression"`}
	pos := -1
	for _, field := range wantOrder {
		at := strings.Index(string(data), field)
		if at < 0 {
			t.Fatalf("config has no %s field: %s", field, data)
		}
		if at < pos {
			t.Errorf("field %s is out of order in %s", field, data)
		}
		pos = at
	}

	var cfg struct {
		Version     int            `json:"version"`
		ID          string         `json:"id"`
		CreatedAt   time.Time      `json:"created_at"`
		Chunker     map[string]int `json:"chunker"`
		Compression string         `json:"compression"`
	}
	if strings.Contains(string(data), `"encryption"`) {
		t.Errorf("config has an encryption field, the object format byte names the codec: %s", data)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	if len(cfg.ID) != 64 {
		t.Errorf("id = %q (len %d), want 32 random bytes in hex", cfg.ID, len(cfg.ID))
	}
	if cfg.CreatedAt.Before(before) || cfg.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("created_at = %v, want roughly now", cfg.CreatedAt)
	}
	want := chunker.DefaultConfig()
	if cfg.Chunker["min"] != want.MinSize || cfg.Chunker["avg"] != want.AvgSize || cfg.Chunker["max"] != want.MaxSize {
		t.Errorf("chunker = %v, want the chunker defaults %+v", cfg.Chunker, want)
	}
	if cfg.Compression != "zstd" {
		t.Errorf("compression = %q, want %q", cfg.Compression, "zstd")
	}
}

func TestInitGivesEachRepositoryItsOwnID(t *testing.T) {
	ctx := context.Background()
	seen := make(map[string]bool)
	for range 4 {
		b := arkmem.New()
		if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
			t.Fatalf("Init: %v", err)
		}
		repo, err := ark.Open(ctx, b, ark.WithoutPassword())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if seen[repo.ID()] {
			t.Fatalf("repository id %q was handed out twice", repo.ID())
		}
		seen[repo.ID()] = true
		repo.Close()
	}
}

func TestInitTwiceIsRefused(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	first := readConfig(t, b)

	if err := ark.Init(ctx, b, ark.WithoutPassword()); !errors.Is(err, ark.ErrAlreadyInitialized) {
		t.Errorf("second Init = %v, want ark.ErrAlreadyInitialized", err)
	}
	if got := readConfig(t, b); string(got) != string(first) {
		t.Error("the refused Init overwrote the existing config")
	}
}

func TestInitWithChunkerConfig(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	want := chunker.Config{MinSize: 1 << 19, AvgSize: 1 << 21, MaxSize: 1 << 23}

	if err := ark.Init(ctx, b, ark.WithoutPassword(), ark.WithChunkerConfig(want)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var cfg struct {
		Chunker map[string]int `json:"chunker"`
	}
	if err := json.Unmarshal(readConfig(t, b), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Chunker["min"] != want.MinSize || cfg.Chunker["avg"] != want.AvgSize || cfg.Chunker["max"] != want.MaxSize {
		t.Errorf("chunker = %v, want %+v", cfg.Chunker, want)
	}
}

func TestInitFillsZeroChunkerFields(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(
		ctx,
		b,
		ark.WithoutPassword(),
		ark.WithChunkerConfig(chunker.Config{AvgSize: 1 << 21}),
	); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var cfg struct {
		Chunker map[string]int `json:"chunker"`
	}
	if err := json.Unmarshal(readConfig(t, b), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	defaults := chunker.DefaultConfig()
	if cfg.Chunker["min"] != defaults.MinSize || cfg.Chunker["max"] != defaults.MaxSize {
		t.Errorf("chunker = %v, want the defaults for the zero fields", cfg.Chunker)
	}
	if cfg.Chunker["avg"] != 1<<21 {
		t.Errorf("avg = %d, want the value that was passed", cfg.Chunker["avg"])
	}
}

func TestInitRejectsInvalidChunkerConfig(t *testing.T) {
	ctx := context.Background()
	cases := map[string]chunker.Config{
		"min above avg": {MinSize: 1 << 22, AvgSize: 1 << 20, MaxSize: 1 << 24},
		"avg above max": {MinSize: 1 << 18, AvgSize: 1 << 24, MaxSize: 1 << 20},
		"negative min":  {MinSize: -1, AvgSize: 1 << 20, MaxSize: 1 << 22},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			b := arkmem.New()
			if err := ark.Init(ctx, b, ark.WithoutPassword(), ark.WithChunkerConfig(cfg)); err == nil {
				t.Fatal("Init = nil, want an error")
			}
			ok, err := b.Exists(ctx, ark.Handle{Type: ark.ConfigFile, Name: "config"})
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if ok {
				t.Error("a refused Init still wrote a config")
			}
		})
	}
}

func TestOpenRequiresAConfig(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()

	if _, err := ark.Open(ctx, b, ark.WithoutPassword()); !errors.Is(err, ark.ErrNotInitialized) {
		t.Errorf("Open on an empty backend = %v, want ark.ErrNotInitialized", err)
	}

	// Open must not create anything on the way out.
	ok, err := b.Exists(ctx, ark.Handle{Type: ark.ConfigFile, Name: "config"})
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Error("a failed Open wrote a config")
	}
}

func TestOpenRejectsUnknownFormats(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"future version":     `{"version":2,"id":"ab","created_at":"2026-09-10T00:00:00Z","chunker":{"min":1,"avg":2,"max":3},"compression":"zstd"}`,
		"missing version":    `{"id":"ab","created_at":"2026-09-10T00:00:00Z","chunker":{"min":1,"avg":2,"max":3},"compression":"zstd"}`,
		"unknown codec":      `{"version":1,"id":"ab","created_at":"2026-09-10T00:00:00Z","chunker":{"min":1,"avg":2,"max":3},"compression":"lz4"}`,
		"impossible chunker": `{"version":1,"id":"ab","created_at":"2026-09-10T00:00:00Z","chunker":{"min":9,"avg":2,"max":3},"compression":"zstd"}`,
		"not json":           `this is not a repository`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			b := arkmem.New()
			h := ark.Handle{Type: ark.ConfigFile, Name: "config"}
			if err := b.Put(ctx, h, strings.NewReader(body)); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := ark.Open(ctx, b, ark.WithoutPassword()); !errors.Is(err, ark.ErrUnsupportedFormat) {
				t.Errorf("Open = %v, want ark.ErrUnsupportedFormat", err)
			}
		})
	}
}

func TestOpenPropagatesContextCancellation(t *testing.T) {
	b := arkmem.New()
	if err := ark.Init(context.Background(), b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ark.Open(ctx, b, ark.WithoutPassword()); !errors.Is(err, context.Canceled) {
		t.Errorf("Open with a cancelled context = %v, want context.Canceled", err)
	}
}
