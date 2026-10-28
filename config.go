package ark

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// repoFormatVersion is the on-disk format version this build writes and is the
// only one it reads. Open rejects anything else with ErrUnsupportedFormat.
const repoFormatVersion = 1

// configName is the name of the single object in the config namespace.
const configName = "config"

// repoIDBytes is the length of the random repository id before hex encoding.
const repoIDBytes = 32

// compressionZstd is the only compression codec format v1 defines.
const compressionZstd = "zstd"

// repositoryConfig is the JSON object Init writes into the config namespace.
// It is plain JSON with no header byte, so it always starts with '{'.
//
// Field order is part of the format: the JSON is marshalled with Go's
// encoding/json in the order declared here.
type repositoryConfig struct {
	Version     int           `json:"version"`
	ID          string        `json:"id"`
	CreatedAt   time.Time     `json:"created_at"`
	Chunker     chunkerParams `json:"chunker"`
	Compression string        `json:"compression"`
}

// chunkerParams are the FastCDC boundaries a repository was created with.
// They are read back on Open and used for every backup into it, so a later
// change to the code defaults does not break deduplication.
type chunkerParams struct {
	Min int `json:"min"`
	Avg int `json:"avg"`
	Max int `json:"max"`
}

// toChunkerConfig converts the stored parameters into a chunker.Config.
func (p chunkerParams) toChunkerConfig() chunker.Config {
	return chunker.Config{MinSize: p.Min, AvgSize: p.Avg, MaxSize: p.Max}
}

// validate rejects parameters the chunker cannot work with.
func (p chunkerParams) validate() error {
	switch {
	case p.Min <= 0 || p.Avg <= 0 || p.Max <= 0:
		return fmt.Errorf("ark: chunker sizes must be positive, got min %d avg %d max %d", p.Min, p.Avg, p.Max)
	case p.Min > p.Avg:
		return fmt.Errorf("ark: chunker min %d is larger than avg %d", p.Min, p.Avg)
	case p.Avg > p.Max:
		return fmt.Errorf("ark: chunker avg %d is larger than max %d", p.Avg, p.Max)
	}
	return nil
}

// initConfig holds the options of an Init call.
type initConfig struct {
	chunker chunker.Config
}

// InitOption configures the repository Init writes. Options are frozen into
// the config object, so they apply to every later Open of that repository.
type InitOption func(*initConfig)

// WithChunkerConfig stores custom FastCDC boundaries in the repository config.
// Zero fields fall back to the chunker defaults. Seed and IDKey are ignored,
// the repository's master key supplies them. The parameters cannot be
// changed afterwards without breaking deduplication against existing chunks.
func WithChunkerConfig(c chunker.Config) InitOption {
	return func(cfg *initConfig) { cfg.chunker = c }
}

// Init creates a repository in b: a fresh master key sealed under pw in a key
// file, then the config object. pw is WithPassword or, explicitly,
// WithoutPassword. The password only unlocks the master key; without it the
// repository cannot be read, and nothing recovers it.
//
// It returns ErrAlreadyInitialized if b already holds a config, so a typo in a
// repository path cannot silently create a second repository next to the first.
// The config is written last, so a failed Init leaves no repository behind.
func Init(ctx context.Context, b Backend, pw Passphrase, opts ...InitOption) error {
	if err := pw.validate(); err != nil {
		return err
	}
	cfg := &initConfig{chunker: chunker.DefaultConfig()}
	for _, o := range opts {
		o(cfg)
	}

	defaults := chunker.DefaultConfig()
	params := chunkerParams{
		Min: cmp.Or(cfg.chunker.MinSize, defaults.MinSize),
		Avg: cmp.Or(cfg.chunker.AvgSize, defaults.AvgSize),
		Max: cmp.Or(cfg.chunker.MaxSize, defaults.MaxSize),
	}
	if err := params.validate(); err != nil {
		return err
	}

	h := Handle{Type: ConfigFile, Name: configName}
	exists, err := b.Exists(ctx, h)
	if err != nil {
		return fmt.Errorf("ark: failed to check for an existing repository: %w", err)
	}
	if exists {
		return ErrAlreadyInitialized
	}

	id := randomHex(repoIDBytes)
	codec := newObjectCodec()
	defer codec.Close()
	keyName, err := putKeyFile(ctx, b, codec, newMasterKey(id), pw)
	if err != nil {
		return err
	}

	data, err := json.Marshal(repositoryConfig{
		Version:     repoFormatVersion,
		ID:          id,
		CreatedAt:   time.Now().UTC(),
		Chunker:     params,
		Compression: compressionZstd,
	})
	if err != nil {
		return fmt.Errorf("ark: failed to encode the repository config: %w", err)
	}

	if putErr := b.Put(ctx, h, bytes.NewReader(data)); putErr != nil {
		// Best effort: an orphaned key file costs every later Open a KDF run.
		_ = b.Delete(context.WithoutCancel(ctx), Handle{Type: KeyFile, Name: keyName})
		return fmt.Errorf("ark: failed to write the repository config: %w", putErr)
	}
	return nil
}

// loadRepositoryConfig reads and validates the config object of b.
//
// It returns ErrNotInitialized when b holds no config and ErrUnsupportedFormat
// when the config names a format version this build does not read.
func loadRepositoryConfig(ctx context.Context, b Backend) (repositoryConfig, error) {
	rc, err := b.Get(ctx, Handle{Type: ConfigFile, Name: configName})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return repositoryConfig{}, ErrNotInitialized
		}
		return repositoryConfig{}, fmt.Errorf("ark: failed to read the repository config: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return repositoryConfig{}, fmt.Errorf("ark: failed to read the repository config: %w", err)
	}

	var cfg repositoryConfig
	if jsonErr := json.Unmarshal(data, &cfg); jsonErr != nil {
		return repositoryConfig{}, fmt.Errorf("%w: config is not valid JSON: %w", ErrUnsupportedFormat, jsonErr)
	}
	if cfg.Version != repoFormatVersion {
		return repositoryConfig{}, fmt.Errorf("%w: repository format version %d, this build reads version %d",
			ErrUnsupportedFormat, cfg.Version, repoFormatVersion)
	}
	if invalid := cfg.Chunker.validate(); invalid != nil {
		return repositoryConfig{}, fmt.Errorf("%w: %w", ErrUnsupportedFormat, invalid)
	}
	if cfg.Compression != compressionZstd {
		return repositoryConfig{}, fmt.Errorf("%w: unknown compression %q", ErrUnsupportedFormat, cfg.Compression)
	}
	return cfg, nil
}
