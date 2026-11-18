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
	// Exact is set when the call ran with WithInfoExact; only then are
	// BytesFreed and BytesUnused filled.
	Exact bool
	// BytesFreed is PruneResult.BytesFreed of a prune dry run: the stored
	// bytes no snapshot uses that a prune deletes.
	BytesFreed int64
	// BytesUnused is PruneResult.BytesUnused of a prune dry run: the stored
	// bytes no snapshot uses that a prune leaves in place. BytesFreed plus
	// BytesUnused is all the unused space.
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

// WithInfoExact makes Info run a prune dry run to fill BytesFreed and
// BytesUnused. That reads every snapshot's trees and every index, holds the
// exclusive lock like Prune and reports progress as a Prune does.
func WithInfoExact() InfoOption { return func(c *infoConfig) { c.exact = true } }

// Info reports how many snapshots the repository holds, how many bytes they
// cover and how much the repository stores per namespace. It lists every
// namespace and reads the snapshot manifests, no pack or index object. A
// manifest deleted while Info runs is left out. With WithInfoExact it adds
// the unused bytes a prune dry run finds.
func (r *Repository) Info(ctx context.Context, opts ...InfoOption) (*RepositoryInfo, error) {
	cfg := &infoConfig{}
	for _, o := range opts {
		o(cfg)
	}
	progress := nopProgress
	var ownLock string
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
		ownLock = lock.name
	}

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, err
	}
	info := &RepositoryInfo{Exact: cfg.exact, Snapshots: len(manifests)}
	for _, m := range manifests {
		info.TotalSize += m.Stats.TotalSize
	}
	if cfg.exact {
		res, err := r.runGC(ctx, manifests, true, progress)
		if err != nil {
			return nil, err
		}
		info.BytesFreed, info.BytesUnused = res.BytesFreed, res.BytesUnused
	}

	// Listed after the dry run, which may store the lost-pack marker; the
	// lock Info holds itself is left out.
	sizes := []struct {
		t   HandleType
		sum *int64
	}{
		{SnapshotFile, &info.Stored.Snapshots},
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
			if s.t == LockFile && b.Name == ownLock {
				return nil
			}
			*s.sum += b.Size
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.t, err)
		}
	}
	return info, nil
}
