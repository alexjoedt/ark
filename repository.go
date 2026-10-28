package ark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// openConfig holds the options of an Open call. It is the caller's runtime
// configuration; the repository's own on-disk configuration is
// repositoryConfig in config.go.
type openConfig struct {
	logger      *slog.Logger
	concurrency int
	lockWait    time.Duration
}

// Option configures a Repository at Open time.
type Option func(*openConfig)

// WithLogger sets the logger for the repository.
func WithLogger(l *slog.Logger) Option {
	return func(c *openConfig) { c.logger = l }
}

// WithLockWait makes an operation retry a blocked lock for up to d instead of
// failing at once with ErrRepoLocked. The default is zero, no waiting.
func WithLockWait(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.lockWait = d
		}
	}
}

// WithConcurrency caps how many backend operations run at once. Values below
// one fall back to the default, twice the number of CPUs.
func WithConcurrency(n int) Option {
	return func(c *openConfig) {
		if n > 0 {
			c.concurrency = n
		}
	}
}

// Repository is the primary entry point for all backup operations.
type Repository struct {
	backend     Backend
	codec       *objectCodec
	chunks      *objectStore
	trees       *objectStore
	logger      *slog.Logger
	concurrency int
	lockWait    time.Duration

	id      string         // repository id from the config object
	key     *masterKey     // unsealed from the key file Open matched
	keyName string         // name of that key file
	chunker chunker.Config // chunker parameters of the config, keyed from key

	// clock is time.Now except in tests, which drive the lock staleness
	// rules through it.
	clock func() time.Time
}

// now is the repository's notion of the current time.
func (r *Repository) now() time.Time {
	if r.clock == nil {
		return time.Now()
	}
	return r.clock()
}

// Open opens an existing repository in b.
//
// It reads the config object first and returns ErrNotInitialized when the
// backend holds none, so Open never creates a repository; use Init for that.
// A config or key file naming a format this build does not read returns
// ErrUnsupportedFormat.
//
// Then it unseals the master key with pw before any other object is read. It
// returns ErrWrongPassword when no key file opens, ErrPasswordRequired for
// WithoutPassword on a protected repository and ErrNotPasswordProtected for a
// password on a repository created with WithoutPassword. Unsealing a password
// protected key file runs argon2id with 64 MiB of memory.
func Open(ctx context.Context, b Backend, pw Passphrase, opts ...Option) (*Repository, error) {
	if err := pw.validate(); err != nil {
		return nil, err
	}
	cfg := &openConfig{
		logger:      slog.Default(),
		concurrency: runtime.NumCPU() * 2,
	}
	for _, o := range opts {
		o(cfg)
	}

	rc, err := loadRepositoryConfig(ctx, b)
	if err != nil {
		return nil, err
	}

	codec := newObjectCodec()
	key, keyName, err := loadMasterKey(ctx, b, codec, rc.ID, pw)
	if err != nil {
		codec.Close()
		return nil, err
	}
	cc := rc.Chunker.toChunkerConfig()
	cc.Seed = key.ChunkerSeed
	cc.IDKey = key.IDKey
	return &Repository{
		backend:     b,
		codec:       codec,
		chunks:      newObjectStore(b, BlobFile, codec, key),
		trees:       newObjectStore(b, TreeFile, codec, key),
		logger:      cfg.logger,
		concurrency: cfg.concurrency,
		lockWait:    cfg.lockWait,
		id:          rc.ID,
		key:         key,
		keyName:     keyName,
		chunker:     cc,
	}, nil
}

// ChangePassword replaces the key file the repository was opened with by one
// sealed under pw, which may also be WithoutPassword. The master key stays,
// so no data is rewritten; for the same reason the change does not revoke
// access for anyone who could read the master key before, through the old
// password or a copy of the old key file.
//
// It takes the exclusive lock. The new key file is written before the old
// one is deleted: when the write fails the old password still opens the
// repository, when the delete fails both do and the error says so.
func (r *Repository) ChangePassword(ctx context.Context, pw Passphrase) error {
	if err := pw.validate(); err != nil {
		return err
	}
	ctx, lock, err := r.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer lock.release(ctx)

	name, err := putKeyFile(ctx, r.backend, r.codec, r.key, pw)
	if err != nil {
		return err
	}
	if err := r.backend.Delete(ctx, Handle{Type: KeyFile, Name: r.keyName}); err != nil {
		return fmt.Errorf(
			"ark: new key file %s written, failed to delete the old key file %s: %w",
			name,
			r.keyName,
			err,
		)
	}
	r.keyName = name
	return nil
}

// ID returns the repository id written by Init.
func (r *Repository) ID() string { return r.id }

// Backup walks src and stores its entries as a new snapshot.
//
// A cancelled backup leaves orphan chunks and tree objects behind, no
// snapshot: the manifest is written last, so the snapshot never becomes
// visible. The orphans stay until Prune sweeps them, and a later backup of
// the same data reuses them.
func (r *Repository) Backup(ctx context.Context, src Source, opts ...BackupOption) (*Snapshot, error) {
	o := &backupOptions{progress: nopProgress}
	for _, opt := range opts {
		opt(o)
	}
	if o.progress == nil {
		o.progress = nopProgress
	}

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	if o.parent != "" {
		parent, resolveErr := r.resolveID(ctx, o.parent)
		if resolveErr != nil {
			return nil, resolveErr
		}
		o.parent = parent
	}
	m, err := r.runBackup(ctx, src, *o)
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// Restore streams the snapshot into sink. id is a full snapshot id or a
// unique prefix.
func (r *Repository) Restore(ctx context.Context, id string, sink Sink, opts ...RestoreOption) (*RestoreResult, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	o := &restoreOptions{progress: nopProgress}
	for _, opt := range opts {
		opt(o)
	}
	if o.progress == nil {
		o.progress = nopProgress
	}
	stats, err := r.runRestore(ctx, id, sink, *o)
	if err != nil {
		return nil, err
	}
	return &RestoreResult{FilesRestored: stats.FilesRestored, BytesRestored: stats.BytesRestored}, nil
}

// resolveID turns a full snapshot id or a unique prefix into the full id.
// It returns ErrNotFound when nothing matches and ErrAmbiguousID, naming the
// candidates, when more than one does.
func (r *Repository) resolveID(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty snapshot id", ErrNotFound)
	}
	// An exact hit costs one Stat; only a prefix needs the full listing.
	if _, err := r.backend.Stat(ctx, Handle{Type: SnapshotFile, Name: id}); err == nil {
		return id, nil
	}

	var matches []string
	err := r.backend.List(ctx, SnapshotFile, func(info BlobInfo) error {
		if strings.HasPrefix(info.Name, id) {
			matches = append(matches, info.Name)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("ark: failed to list snapshots: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%w: %s matches %s", ErrAmbiguousID, id, strings.Join(matches, ", "))
	}
}

// Snapshots returns all snapshots matching filter, newest first.
func (r *Repository) Snapshots(ctx context.Context, filter SnapshotFilter) ([]*Snapshot, error) {
	if len(filter.IDs) > 0 {
		// Resolve into a copy: the caller's slice stays untouched.
		resolved := make([]string, len(filter.IDs))
		for i, id := range filter.IDs {
			full, err := r.resolveID(ctx, id)
			if err != nil {
				return nil, err
			}
			resolved[i] = full
		}
		filter.IDs = resolved
	}

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, err
	}

	var out []*Snapshot
	for _, m := range manifests {
		s := manifestToSnapshot(m)
		if !filter.Match(s) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Snapshot returns one snapshot. id is a full snapshot id or a unique prefix.
func (r *Repository) Snapshot(ctx context.Context, id string) (*Snapshot, error) {
	id, err := r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// deleteSnapshot removes one manifest. Its objects stay in the CAS until
// Prune sweeps them, which is why this is not part of the public surface:
// callers go through Forget.
func (r *Repository) deleteSnapshot(ctx context.Context, id string) error {
	if err := r.backend.Delete(ctx, Handle{Type: SnapshotFile, Name: id}); err != nil {
		return fmt.Errorf("ark: failed to delete snapshot %s: %w", id, err)
	}
	return nil
}

// Forget removes the snapshots that policy does not keep among those matching
// filter. Chunks stay until Prune.
//
// A policy without a positive rule removes every match, so it is only
// accepted together with filter.IDs; otherwise Forget returns ErrEmptyPolicy
// and deletes nothing.
//
// Removing named ids without a policy reads only those manifests, and one
// that fails with ErrCorruptManifest is still removed, reported with its ID
// alone. This is how a corrupt manifest leaves the repository.
func (r *Repository) Forget(
	ctx context.Context,
	policy ForgetPolicy,
	filter SnapshotFilter,
	opts ...ForgetOption,
) (*ForgetResult, error) {
	if policy.keepsNothing() && len(filter.IDs) == 0 {
		return nil, ErrEmptyPolicy
	}

	cfg := &forgetConfig{}
	for _, o := range opts {
		o(cfg)
	}

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	var result *ForgetResult
	if policy.keepsNothing() {
		result, err = r.forgetNamed(ctx, filter)
	} else {
		var snapshots []*Snapshot
		snapshots, err = r.Snapshots(ctx, filter)
		if err == nil {
			result = applyForgetPolicy(snapshots, policy)
		}
	}
	if err != nil {
		return nil, err
	}
	result.DryRun = cfg.dryRun
	if cfg.dryRun {
		return result, nil
	}

	for _, s := range result.Remove {
		if err := r.deleteSnapshot(ctx, s.ID); err != nil {
			r.logger.WarnContext(ctx, "forget: failed to delete snapshot", "id", s.ID, "error", err)
		}
	}
	return result, nil
}

// forgetNamed selects the snapshots in filter.IDs for removal, loading only
// their manifests. The other filter fields still narrow the healthy ones.
func (r *Repository) forgetNamed(ctx context.Context, filter SnapshotFilter) (*ForgetResult, error) {
	rest := filter
	rest.IDs = nil
	result := &ForgetResult{}
	seen := make(map[string]bool, len(filter.IDs))
	for _, id := range filter.IDs {
		full, err := r.resolveID(ctx, id)
		if err != nil {
			return nil, err
		}
		if seen[full] {
			continue
		}
		seen[full] = true
		m, err := r.loadManifest(ctx, full)
		if errors.Is(err, ErrCorruptManifest) {
			result.Remove = append(result.Remove, &Snapshot{ID: full})
			continue
		}
		if err != nil {
			return nil, err
		}
		if s := manifestToSnapshot(m); rest.Match(s) {
			result.Remove = append(result.Remove, s)
		}
	}
	sortNewestFirst(result.Remove)
	return result, nil
}

// pruneConfig holds configuration for a Prune call.
type pruneConfig struct{ dryRun bool }

// PruneOption configures a Prune call.
type PruneOption func(*pruneConfig)

// WithPruneDryRun makes Prune report what would be removed without deleting
// anything.
func WithPruneDryRun() PruneOption { return func(c *pruneConfig) { c.dryRun = true } }

// Prune removes CAS objects that no snapshot references.
// Pass WithPruneDryRun to report what would be removed without making changes.
func (r *Repository) Prune(ctx context.Context, opts ...PruneOption) (*PruneResult, error) {
	cfg := &pruneConfig{}
	for _, o := range opts {
		o(cfg)
	}

	ctx, lock, err := r.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	return r.runGC(ctx, cfg.dryRun)
}

// Export writes a portable tar.gz archive of one snapshot to w. id is a full
// snapshot id or a unique prefix.
func (r *Repository) Export(ctx context.Context, id string, w io.Writer) error {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return err
	}
	return r.runExport(ctx, id, w)
}

// Import reads a portable tar.gz archive from reader and re-indexes it.
func (r *Repository) Import(ctx context.Context, reader io.Reader) (*Snapshot, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	m, err := r.runImport(ctx, reader)
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// Verify checks one snapshot: manifest, tree and every chunk.
// id is a full snapshot id or a unique prefix.
func (r *Repository) Verify(ctx context.Context, id string) (*VerifyResult, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.runVerify(ctx, id)
}

// Check verifies every snapshot in the repository.
func (r *Repository) Check(ctx context.Context) (*CheckResult, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	// Names, not manifests: a manifest that does not open is a finding of the
	// check, not a reason to stop it.
	var ids []string
	err = r.backend.List(ctx, SnapshotFile, func(info BlobInfo) error {
		ids = append(ids, info.Name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to list snapshots: %w", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	result := &CheckResult{SnapshotsChecked: len(ids)}
	for _, id := range ids {
		vr, err := r.runVerify(ctx, id)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: %v", id, err))
			continue
		}
		for _, c := range vr.MissingChunks {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: missing chunk %s", id, c))
		}
		for _, c := range vr.CorruptChunks {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: corrupt chunk %s", id, c))
		}
	}
	return result, nil
}

// Walk calls fn for every entry of a snapshot in tree order, parents before
// children, without materialising the list. id is a full snapshot id or a
// unique prefix. An error from fn ends the walk and is returned.
//
// Directory paths carry a trailing slash, the way tar names them; every other
// entry carries the plain path a Source yielded.
func (r *Repository) Walk(ctx context.Context, id string, fn func(FileInfo) error) error {
	id, err := r.resolveID(ctx, id)
	if err != nil {
		return err
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return err
	}
	if m.RootTree == "" {
		return nil
	}
	return walkTree(ctx, r.trees, m.RootTree, func(e fileEntry) error { return fn(e.FileInfo) })
}

// Close releases any resources held by the repository (and the backend).
func (r *Repository) Close() error {
	r.codec.Close()
	return r.backend.Close()
}

// Match returns true if the snapshot matches all non-zero fields of f.
// IDs are compared verbatim; Snapshots resolves prefixes before calling this.
func (f SnapshotFilter) Match(s *Snapshot) bool {
	if len(f.IDs) > 0 && !slices.Contains(f.IDs, s.ID) {
		return false
	}
	if f.Name != "" && s.SourceName != f.Name {
		return false
	}
	if f.Type != "" && s.SourceType != f.Type {
		return false
	}
	if f.Host != "" && s.Host != f.Host {
		return false
	}
	if !f.Before.IsZero() && !s.CreatedAt.Before(f.Before) {
		return false
	}
	if !f.After.IsZero() && !s.CreatedAt.After(f.After) {
		return false
	}
	if len(f.Tags) > 0 {
		tagSet := make(map[string]struct{}, len(s.Tags))
		for _, t := range s.Tags {
			tagSet[t] = struct{}{}
		}
		for _, ft := range f.Tags {
			if _, ok := tagSet[ft]; !ok {
				return false
			}
		}
	}
	return true
}
