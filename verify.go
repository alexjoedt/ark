package ark

import (
	"context"
	"errors"
	"fmt"
)

func (r *Repository) runVerify(ctx context.Context, snapshotID string) (*VerifyResult, error) {
	logger := r.logger.With("component", "verify", "snapshot", snapshotID)

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{SnapshotID: snapshotID, OK: true}

	logger.InfoContext(ctx, "verifying snapshot", "chunks", m.Stats.ChunksTotal)

	// Streams the tree like restore does, holding one chunk at a time. The
	// set of ids already seen is hashes only, so it grows with the chunk
	// count and not with the size of the data. Both stores recompute the
	// name of every object they read, so a tree that opens but does not match
	// its name fails the walk.
	seen := make(map[string]struct{})
	chunks := r.chunks.verifying()
	err = walkSnapshot(ctx, r.trees.verifying(), m, pathFilter{}, func(fe fileEntry) error {
		for _, ref := range fe.Chunks {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if _, done := seen[ref.ID]; done {
				continue
			}
			seen[ref.ID] = struct{}{}
			if err := verifyChunk(ctx, chunks, ref.ID, result); err != nil {
				return err
			}
		}
		return nil
	})
	// verifyChunk records chunk failures, so a broken object here is a tree.
	if errors.Is(err, errSealBroken) || errors.Is(err, errNameMismatch) {
		return result, fmt.Errorf("%w: %w", ErrCorruptTree, err)
	}
	if err != nil {
		return result, err
	}

	if result.OK {
		logger.InfoContext(ctx, "verification passed", "verified", result.ChunksChecked)
	} else {
		logger.WarnContext(ctx, "verification found issues",
			"verified", result.ChunksChecked,
			"missing", len(result.MissingChunks),
			"corrupt", len(result.CorruptChunks),
		)
	}
	return result, nil
}

// verifyChunk records one chunk as checked, missing or corrupt. Only a backend
// failure that says nothing about the chunk itself is returned as an error.
func verifyChunk(ctx context.Context, chunks *objectStore, id string, result *VerifyResult) error {
	exists, err := chunks.Exists(ctx, id)
	if err != nil {
		return fmt.Errorf("verify: failed to check chunk %s: %w", id, err)
	}
	if !exists {
		result.MissingChunks = append(result.MissingChunks, id)
		result.OK = false
		return nil
	}

	// A chunk that fails its tag or does not hash to its name is corrupt.
	//nolint:nilerr // a corrupt chunk is a result, not an error
	if _, err := chunks.Get(ctx, id); err != nil {
		result.CorruptChunks = append(result.CorruptChunks, id)
		result.OK = false
		return nil
	}
	result.ChunksChecked++
	return nil
}
