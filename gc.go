package ark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// prunePlan is what a prune does to the packs and the index of one blob
// type.
type prunePlan struct {
	store *objectStore
	table *indexTable
	// referenced marks the packs of table.packs that hold a referenced blob
	// and stay. The index is written from them when rewrite is set.
	referenced []bool
	rewrite    bool
	// stored is every pack in the namespace. remove names those of them that
	// no index lists or that hold no referenced blob, with the number of
	// blobs the index lists in each: none for a pack it does not list.
	stored []BlobInfo
	remove map[string]int
}

// runGC deletes the packs without a referenced blob and the packs no index
// lists, and rewrites the index without them. Nothing is deleted before
// every snapshot was walked and every blob it references was found in the
// index: a prune that trusted an incomplete index would delete packs whose
// index object was lost.
//
// The order is what a kill at any point leaves readable: the complete new
// index, Sync, the old index objects, Sync, then the packs. Until the last
// step every listed pack is stored; after a kill in it the index lists
// nothing that is gone, and the next prune removes the packs left over.
func (r *Repository) runGC(ctx context.Context, dryRun bool, progress ProgressFunc) (*PruneResult, error) {
	logger := r.logger.With("component", "gc")

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: failed to list manifests: %w", err)
	}

	// A snapshot whose tree cannot be read stops the prune: an incomplete
	// live set would delete its objects.
	chunks, trees := r.stores()
	liveTrees := make(map[string]struct{})
	liveChunks := make(map[string]struct{})
	for _, m := range manifests {
		if m.RootTree == "" {
			continue
		}
		if err := collectLiveObjects(ctx, trees, m.RootTree, liveTrees, liveChunks); err != nil {
			return nil, fmt.Errorf("gc: snapshot %s: %w", m.ID, notIndexedHint(err))
		}
	}
	for name := range liveChunks {
		_, _, ok, err := chunks.index.lookup(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		if !ok {
			err := fmt.Errorf("%w: chunk %s: %w", ErrNotFound, name, errNotIndexed)
			return nil, fmt.Errorf("gc: %w", notIndexedHint(err))
		}
	}

	plans := []*prunePlan{{store: chunks}, {store: trees}}
	scanned := 0
	for i, live := range []map[string]struct{}{liveChunks, liveTrees} {
		if err := plans[i].plan(ctx, live); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		scanned += len(plans[i].stored)
	}

	result := &PruneResult{DryRun: dryRun}
	ev := ProgressEvent{Phase: PhaseSweep, ChunksTotal: scanned}
	progress(ev)
	if !dryRun {
		if err := r.rewriteIndexes(ctx, plans); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
	}
	for _, p := range plans {
		if err := p.sweep(ctx, r, dryRun, result, func() {
			ev.ChunksDone++
			progress(ev)
		}); err != nil {
			return nil, err
		}
	}
	if !dryRun {
		if err := r.backend.Sync(ctx); err != nil {
			return nil, fmt.Errorf("gc: failed to sync deletions: %w", err)
		}
	}

	logger.InfoContext(ctx, "gc complete",
		"scanned", scanned,
		"live", len(liveChunks)+len(liveTrees),
		"deleted", result.BlobsRemoved,
		"freed_bytes", result.BytesFreed,
		"dry_run", dryRun,
	)
	ev.Phase = PhaseFinalize
	progress(ev)
	return result, nil
}

// notIndexedHint says what to do about a referenced object no index lists.
func notIndexedHint(err error) error {
	if !errors.Is(err, errNotIndexed) {
		return err
	}
	return fmt.Errorf("%w: a snapshot references it, so an index object was lost or does not open; "+
		"nothing was deleted, RebuildIndex restores the index from the packs", err)
}

// plan loads the index and lists the packs of p's namespace, and decides
// what goes: live names every referenced blob of the type. A pack stays
// while the index lists a referenced blob in it, also when that is a second
// copy of the blob.
func (p *prunePlan) plan(ctx context.Context, live map[string]struct{}) error {
	x := p.store.index
	t, err := x.load(ctx)
	if err != nil {
		return err
	}
	p.table = t

	p.referenced = make([]bool, len(t.packs))
	blobs := make([]int, len(t.packs))
	var name [2 * sha256.Size]byte
	for _, records := range &t.records {
		for _, rec := range records {
			blobs[rec.pack]++
			hex.Encode(name[:], rec.name[:])
			if _, ok := live[string(name[:])]; ok {
				p.referenced[rec.pack] = true
			}
		}
	}
	dead := make(map[string]int)
	kept := 0
	for i, pack := range t.packs {
		if p.referenced[i] {
			kept += blobs[i]
			continue
		}
		dead[hex.EncodeToString(pack[:])] = blobs[i]
	}
	// One object per indexMaxRecords blobs is what a rewrite leaves at most.
	p.rewrite = len(dead) > 0 || len(t.unreadable) > 0 ||
		len(t.objects) > (kept+indexMaxRecords-1)/indexMaxRecords

	p.remove = make(map[string]int)
	err = p.store.backend.List(ctx, x.space.packs, func(info BlobInfo) error {
		p.stored = append(p.stored, info)
		lost, isDead := dead[info.Name]
		if _, listed := t.pack(info.Name); isDead || !listed {
			p.remove[info.Name] = lost
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ark: failed to list %s objects: %w", x.space.packs, err)
	}
	return nil
}

// indexSet is the index of one blob type as a prune or a rebuild leaves it:
// packs returns what the new index objects list, old names the objects they
// replace.
type indexSet struct {
	space *packSpace
	packs func() []indexedPack
	old   []string
}

// rewriteIndexes stores the index of the packs that stay and deletes the
// index objects it replaces. An object that does not open goes last, with
// the packs.
func (r *Repository) rewriteIndexes(ctx context.Context, plans []*prunePlan) error {
	var sets []indexSet
	for _, p := range plans {
		if !p.rewrite {
			continue
		}
		old := slices.DeleteFunc(slices.Clone(p.table.objects), func(name string) bool {
			return slices.Contains(p.table.unreadable, name)
		})
		sets = append(sets, indexSet{
			space: p.store.space,
			packs: func() []indexedPack { return p.table.listed(p.referenced) },
			old:   old,
		})
	}
	return r.replaceIndexes(ctx, sets)
}

// replaceIndexes stores the index objects of every set and then deletes the
// objects they replace, each step made durable before the next: at any point
// a kill leaves an index that lists every blob the new one does.
func (r *Repository) replaceIndexes(ctx context.Context, sets []indexSet) error {
	if len(sets) == 0 {
		return nil
	}
	written := make([][]string, len(sets))
	for i, set := range sets {
		// A fresh index: one that is loaded would merge what it writes into
		// its table, which nothing reads here.
		names, err := newPackIndex(r.backend, set.space, r.logger).write(ctx, set.packs())
		if err != nil {
			return err
		}
		written[i] = names
	}
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("failed to sync the new index: %w", err)
	}
	for i, set := range sets {
		for _, name := range set.old {
			// An object named like a new one is the new one: the name is
			// the hash of the content.
			if slices.Contains(written[i], name) {
				continue
			}
			h := Handle{Type: set.space.index, Name: name}
			if err := r.backend.Delete(ctx, h); err != nil {
				return fmt.Errorf("failed to delete %s object %s: %w", h.Type, name, err)
			}
		}
	}
	// Without this barrier a power loss could bring an old index object
	// back after the packs it lists were deleted.
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("failed to sync the index: %w", err)
	}
	return nil
}

// runRebuildIndex replaces the index of both blob types with what the stored
// packs say about themselves.
func (r *Repository) runRebuildIndex(ctx context.Context) (*RebuildIndexResult, error) {
	result := &RebuildIndexResult{}
	var sets []indexSet
	chunks, trees := r.stores()
	for _, s := range []*objectStore{chunks, trees} {
		old, err := s.index.load(ctx)
		if err != nil {
			return nil, fmt.Errorf("rebuild index: %w", err)
		}
		packs, unreadable, err := s.index.scanPacks(ctx)
		if err != nil {
			return nil, fmt.Errorf("rebuild index: %w", err)
		}
		// A pack that does not open describes none of its blobs, and no later
		// rebuild could: what the old index says about it is all that still
		// locates the blobs in it that are intact.
		keep := make([]bool, len(old.packs))
		for _, name := range unreadable {
			if i, listed := old.pack(name); listed {
				keep[i] = true
			}
		}
		packs = append(packs, withoutCopies(old.listed(keep), packs)...)

		result.PacksIndexed += len(packs)
		for _, p := range packs {
			result.BlobsIndexed += len(p.entries)
		}
		result.UnreadablePacks = append(result.UnreadablePacks, unreadable...)
		sets = append(sets, indexSet{
			space: s.space,
			packs: func() []indexedPack { return packs },
			old:   old.objects,
		})
	}
	if err := r.replaceIndexes(ctx, sets); err != nil {
		return nil, fmt.Errorf("rebuild index: %w", err)
	}
	r.logger.InfoContext(ctx, "index rebuilt",
		"packs", result.PacksIndexed,
		"blobs", result.BlobsIndexed,
		"unreadable_packs", len(result.UnreadablePacks),
	)
	return result, nil
}

// withoutCopies returns kept, the packs that do not open with what the old
// index lists in them, without the blobs a pack of readable holds as well. A
// lookup resolves a blob in several packs to the pack with the smallest name,
// so an entry kept for a damaged pack would hide the copy that still opens.
func withoutCopies(kept, readable []indexedPack) []indexedPack {
	copied := make(map[[sha256.Size]byte]bool)
	for _, p := range kept {
		for _, e := range p.entries {
			copied[e.name] = false
		}
	}
	if len(copied) == 0 {
		return kept
	}
	for _, p := range readable {
		for _, e := range p.entries {
			if _, ok := copied[e.name]; ok {
				copied[e.name] = true
			}
		}
	}
	for i := range kept {
		kept[i].entries = slices.DeleteFunc(kept[i].entries, func(e packEntry) bool { return copied[e.name] })
	}
	// A pack left without a blob is no longer listed: Prune removes it.
	return slices.DeleteFunc(kept, func(p indexedPack) bool { return len(p.entries) == 0 })
}

// sweep deletes the packs of p.remove and the index objects that do not
// open, and adds what it removed to result. It calls scanned once per stored
// pack. A pack that fails to delete stays for the next prune, which finds it
// listed nowhere.
func (p *prunePlan) sweep(
	ctx context.Context, r *Repository, dryRun bool, result *PruneResult, scanned func(),
) error {
	space := p.store.space
	for _, info := range p.stored {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller's own cancellation
		}
		blobs, remove := p.remove[info.Name]
		if remove && !dryRun {
			if err := r.backend.Delete(ctx, Handle{Type: space.packs, Name: info.Name}); err != nil {
				r.logger.WarnContext(ctx, "gc: failed to delete an unreferenced pack",
					"namespace", space.packs.String(), "name", info.Name, "error", err)
				remove = false
			}
		}
		if remove {
			result.BlobsRemoved += blobs
			result.BytesFreed += info.Size
		}
		scanned()
	}
	if dryRun {
		return nil
	}
	for _, name := range p.table.unreadable {
		if err := r.backend.Delete(ctx, Handle{Type: space.index, Name: name}); err != nil {
			r.logger.WarnContext(ctx, "gc: failed to delete an index object that does not open",
				"namespace", space.index.String(), "name", name, "error", err)
		}
	}
	return nil
}
