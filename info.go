package ark

import (
	"context"
	"fmt"
)

// RepositoryInfo is what Info reports about a repository.
type RepositoryInfo struct {
	// Snapshots counts the snapshots in the repository.
	Snapshots int
	// TotalSize sums Stats.TotalSize over the snapshots. It is not
	// deduplicated: a file in ten snapshots counts ten times.
	TotalSize int64
	// Stored is the size of the stored objects per namespace, as the
	// backend lists them.
	Stored StoredSizes
	// Exact is set when the call ran with WithInfoExact; only then is
	// BytesUnused filled.
	Exact bool
	// BytesUnused is PruneResult.BytesUnused of a prune dry run: the stored
	// bytes no snapshot uses that a prune would leave in place.
	BytesUnused int64
}

// StoredSizes is the size of the stored objects of a repository per
// namespace, in bytes as stored.
type StoredSizes struct {
	DataPacks int64 // the packs that hold file content
	TreePacks int64 // the packs that hold directory trees
	Indexes   int64 // the index objects of both pack kinds
	Snapshots int64 // the snapshot manifests
	Keys      int64 // the key files
	Other     int64 // config, locks and the lost-pack marker
}

// Total returns the size of every stored object.
func (s StoredSizes) Total() int64 {
	return s.DataPacks + s.TreePacks + s.Indexes + s.Snapshots + s.Keys + s.Other
}

type infoConfig struct{ exact bool }

// InfoOption configures an Info call.
type InfoOption func(*infoConfig)

// WithInfoExact makes Info run a prune dry run to fill BytesUnused. That
// reads every snapshot's trees and every index, holds the exclusive lock like
// Prune and reports progress as a Prune does.
func WithInfoExact() InfoOption { return func(c *infoConfig) { c.exact = true } }

// Info reports how many snapshots the repository holds, how many bytes they
// cover and how much the repository stores per namespace. It lists every
// namespace and reads the snapshot manifests, no pack or index object. With
// WithInfoExact it adds the unused bytes a prune dry run finds.
func (r *Repository) Info(ctx context.Context, opts ...InfoOption) (*RepositoryInfo, error) {
	cfg := &infoConfig{}
	for _, o := range opts {
		o(cfg)
	}
	progress := nopProgress
	if cfg.exact {
		progress = r.observer(OperationPrune, nil)
		progress(ProgressEvent{Phase: PhaseLoad})
		var lock *repoLock
		var err error
		ctx, lock, err = r.acquire(ctx, true)
		if err != nil {
			return nil, err
		}
		defer lock.release(ctx)
	}

	info := &RepositoryInfo{Exact: cfg.exact}
	err := r.backend.List(ctx, SnapshotFile, func(b BlobInfo) error {
		m, err := r.loadManifest(ctx, SnapshotID(b.Name))
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", b.Name, err)
		}
		info.Snapshots++
		info.TotalSize += m.Stats.TotalSize
		info.Stored.Snapshots += b.Size
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	sizes := []struct {
		t   HandleType
		sum *int64
	}{
		{DataPackFile, &info.Stored.DataPacks},
		{TreePackFile, &info.Stored.TreePacks},
		{DataIndexFile, &info.Stored.Indexes},
		{TreeIndexFile, &info.Stored.Indexes},
		{KeyFile, &info.Stored.Keys},
		{ConfigFile, &info.Stored.Other},
		{LockFile, &info.Stored.Other},
		{LostPackFile, &info.Stored.Other},
	}
	for _, s := range sizes {
		err := r.backend.List(ctx, s.t, func(b BlobInfo) error {
			*s.sum += b.Size
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.t, err)
		}
	}

	if cfg.exact {
		res, err := r.runGC(ctx, true, progress)
		if err != nil {
			return nil, err
		}
		info.BytesUnused = res.BytesUnused
	}
	return info, nil
}
