package ark

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// repoFormatVersion is the on-disk format version this build writes and is the
// only one it reads. Open rejects anything else with ErrUnsupportedFormat.
// The format is frozen with v1.1.0; later changes are additive or bump it.
const repoFormatVersion = 1

// configName is the name of the repository config object in the config
// namespace. The namespace holds one other object, the optional features.
const configName = "config"

// featuresName is the optional sealed object in the config namespace that
// names the features a repository requires. No build of version 1 writes it.
const featuresName = "features"

// knownFeatures are the feature names this build supports. Version 1 has none.
var knownFeatures = map[string]struct{}{}

type repositoryFeatures struct {
	Required []string `json:"required"`
}

// checkFeatures reads the features object, when there is one, and refuses a
// repository that requires a feature this build does not know. A tag that
// does not verify is ErrCorruptConfig. An object that authenticates but does
// not decode was written by a holder of the data key, so a later build, and
// is ErrUnsupportedFormat like the config.
func checkFeatures(ctx context.Context, b Backend, codec *objectCodec, key *masterKey) error {
	h := Handle{Type: ConfigFile, Name: featuresName}
	rc, err := b.Get(ctx, h)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, h.Name, err)
	}
	defer rc.Close()
	obj, err := readPlain(rc, "the features object")
	if err != nil {
		return err
	}
	data, err := codec.open((*[32]byte)(&key.DataKey), objectAAD(h), obj)
	if errors.Is(err, errSealBroken) {
		return fmt.Errorf("%w: features: %w", ErrCorruptConfig, err)
	}
	if err != nil {
		return fmt.Errorf("ark: failed to open %s object %s: %w", h.Type, h.Name, err)
	}
	var f repositoryFeatures
	if err := decodeStrict("features", data, &f); err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			return err
		}
		return fmt.Errorf("%w: features: %w", ErrUnsupportedFormat, err)
	}
	for _, name := range f.Required {
		if _, ok := knownFeatures[name]; !ok {
			return fmt.Errorf("%w: the repository requires the feature %q, a newer ark is needed",
				ErrUnsupportedFormat, name)
		}
	}
	return nil
}

// repoIDBytes is the length of the random repository id before hex encoding.
const repoIDBytes = 32

// repositoryConfig is the JSON object Init writes into the config namespace.
// It is plain JSON with no header byte, so it always starts with '{'. The
// master key carries the SHA-256 of the stored bytes (configHash), so Open
// rejects any change to them with ErrCorruptConfig.
//
// Field order is part of the format: the JSON is marshalled with Go's
// encoding/json in the order declared here.
type repositoryConfig struct {
	Version   int           `json:"version"`
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	Chunker   chunkerParams `json:"chunker"`
}

// chunkerParams are the FastCDC boundaries a repository was created with.
// They are read back on Open and used for every backup into it, so a later
// change to the code defaults does not break deduplication.
type chunkerParams struct {
	Min int `json:"min"`
	Avg int `json:"avg"`
	Max int `json:"max"`
}

// MaxChunkerMax is the largest chunker max a version 1 config may hold, so
// every reader can allocate a buffer of max bytes.
const MaxChunkerMax = 64 << 20

// toChunkerConfig converts the stored parameters into a chunker.Config.
func (p chunkerParams) toChunkerConfig() chunker.Config {
	return chunker.Config{MinSize: p.Min, AvgSize: p.Avg, MaxSize: p.Max}
}

// validate rejects parameters a version 1 config may not hold.
func (p chunkerParams) validate() error {
	switch {
	case p.Min <= 0 || p.Avg <= 0 || p.Max <= 0:
		return fmt.Errorf("ark: chunker sizes must be positive, got min %d avg %d max %d", p.Min, p.Avg, p.Max)
	case p.Min > p.Avg:
		return fmt.Errorf("ark: chunker min %d is larger than avg %d", p.Min, p.Avg)
	case p.Avg > p.Max:
		return fmt.Errorf("ark: chunker avg %d is larger than max %d", p.Avg, p.Max)
	case p.Avg&(p.Avg-1) != 0:
		return fmt.Errorf("ark: chunker avg %d is not a power of two", p.Avg)
	case p.Max > MaxChunkerMax:
		return fmt.Errorf("ark: chunker max %d is larger than %d", p.Max, MaxChunkerMax)
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
// file, then the config object. pw is NewPassword or, explicitly,
// NoPassword. The password only unlocks the master key; without it the
// repository cannot be read, and nothing recovers it.
//
// It returns ErrAlreadyInitialized if b already holds a config or a features
// object, so a typo in a repository path cannot silently create a second
// repository next to the first, and a features object an earlier repository
// left behind cannot make the new one fail every Open.
// The config is written last, so a failed Init leaves no repository behind.
func Init(ctx context.Context, b Backend, pw Password, opts ...InitOption) error {
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
	exists, err = b.Exists(ctx, Handle{Type: ConfigFile, Name: featuresName})
	if err != nil {
		return fmt.Errorf("ark: failed to check for an existing repository: %w", err)
	}
	if exists {
		return fmt.Errorf("%w: the config namespace holds a features object of an earlier repository",
			ErrAlreadyInitialized)
	}

	id := randomHex(repoIDBytes)
	data, err := json.Marshal(repositoryConfig{
		Version:   repoFormatVersion,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Chunker:   params,
	})
	if err != nil {
		return fmt.Errorf("ark: failed to encode the repository config: %w", err)
	}

	mk := newMasterKey(id)
	mk.ConfigHash = configHash(data)
	codec := newObjectCodec()
	defer codec.Close()
	keyName, err := putKeyFile(ctx, b, codec, mk, pw)
	if err != nil {
		return err
	}

	if putErr := b.Put(ctx, h, bytes.NewReader(data)); putErr != nil {
		// Best effort: an orphaned key file costs every later Open a KDF run.
		_ = b.Delete(context.WithoutCancel(ctx), Handle{Type: KeyFile, Name: keyName})
		return fmt.Errorf("ark: failed to write the repository config: %w", putErr)
	}
	if err := b.Sync(ctx); err != nil {
		return fmt.Errorf("ark: failed to sync the new repository: %w", err)
	}
	return nil
}

// configHash is the hex SHA-256 of the config bytes as stored. It covers the
// raw bytes, not a canonical form: only Init writes the config, so any
// difference is a tamper.
func configHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readRepositoryConfig returns the stored config bytes of b, unauthenticated.
// It returns ErrNotInitialized when b holds no config.
func readRepositoryConfig(ctx context.Context, b Backend) ([]byte, error) {
	rc, err := b.Get(ctx, Handle{Type: ConfigFile, Name: configName})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotInitialized
		}
		return nil, fmt.Errorf("ark: failed to read the repository config: %w", err)
	}
	defer rc.Close()

	return readPlain(rc, "the repository config")
}

// maxPlainObject caps the bytes read of a key file, the config or the features
// object, which are
// read before anything authenticates them. ark writes under 1 KiB of either.
const maxPlainObject = 64 << 10

// readPlain reads what, an unauthenticated object, and refuses one above
// maxPlainObject as ErrUnsupportedFormat without holding more than the cap.
func readPlain(r io.Reader, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxPlainObject+1))
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s: %w", what, err)
	}
	if len(data) > maxPlainObject {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrUnsupportedFormat, what, maxPlainObject)
	}
	return data, nil
}

// errFormatVersion reports a repository of format version v.
func errFormatVersion(v int) error {
	return fmt.Errorf("%w: repository format version %d, this build reads version %d",
		ErrUnsupportedFormat, v, repoFormatVersion)
}

// parseRepositoryConfig decodes and validates config bytes. A config this
// build cannot use is ErrUnsupportedFormat.
func parseRepositoryConfig(data []byte) (repositoryConfig, error) {
	var cfg repositoryConfig
	if jsonErr := decodeStrict("config", data, &cfg); jsonErr != nil {
		if errors.Is(jsonErr, ErrUnsupportedFormat) {
			return repositoryConfig{}, jsonErr
		}
		return repositoryConfig{}, fmt.Errorf("%w: config is not valid JSON: %w", ErrUnsupportedFormat, jsonErr)
	}
	if cfg.Version != repoFormatVersion {
		return repositoryConfig{}, errFormatVersion(cfg.Version)
	}
	if invalid := cfg.Chunker.validate(); invalid != nil {
		return repositoryConfig{}, fmt.Errorf("%w: %w", ErrUnsupportedFormat, invalid)
	}
	cfg.CreatedAt = cfg.CreatedAt.UTC()
	return cfg, nil
}

// decodeStrict decodes the JSON object data, which the message calls what,
// into v. A key v has no field for is ErrUnsupportedFormat and named in the
// message: a later build wrote it, and this one cannot know what the key
// asks of a reader. Config, key file, manifest and lock are read with it.
// After an error v holds nothing to use.
func decodeStrict(what string, data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			return fmt.Errorf("%s has data after its JSON value", what)
		}
		return nil
	}
	// encoding/json has no type for an unknown key, only a text that may
	// change. So the text decides nothing: what this decode refuses and one
	// without the option accepts has such a key.
	if json.Unmarshal(data, v) != nil {
		return err //nolint:wrapcheck // the caller names the object
	}
	if key, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fmt.Errorf("%w: %s has the key %s, which this build does not know", ErrUnsupportedFormat, what, key)
	}
	return fmt.Errorf("%w: %s has a key this build does not know: %w", ErrUnsupportedFormat, what, err)
}
