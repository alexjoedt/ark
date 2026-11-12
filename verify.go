package ark

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// chunkVerdict is what a verify found out about one chunk.
type chunkVerdict uint8

const (
	chunkChecked chunkVerdict = iota + 1
	chunkMissing
	chunkCorrupt
)

// runVerify checks one snapshot and reports PhaseFetch events to progress,
// CurrentPath set to the snapshot ID and the chunk counters at the references
// verified so far: one for the chunks that were settled without a read, when
// there are any, and one per data pack it read. Callers emit the events that
// open and close their operation.
//
// settled, when set, holds the verdict on every chunk an earlier call read
// and takes those of this one: a check passes one map to the verify of every
// snapshot, so a chunk several snapshots share is read once.
func (r *Repository) runVerify(
	ctx context.Context,
	snapshotID string,
	chunks, trees *objectStore,
	settled map[[sha256.Size]byte]chunkVerdict,
	progress ProgressFunc,
) (*VerifyResult, error) {
	logger := r.logger.With("component", "verify", "snapshot", snapshotID)

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{SnapshotID: snapshotID, OK: true}

	logger.InfoContext(ctx, "verifying snapshot", "chunks", m.Stats.ChunksTotal)

	// The tree is walked first and only counts the references to every
	// chunk: 40 bytes per chunk, whatever the size of the data. The tree
	// store recomputes the name of every node it reads, so a tree that opens
	// but does not match its name fails the walk.
	refs := make(map[[sha256.Size]byte]int)
	total := 0
	err = walkSnapshot(ctx, trees.verifying(), m, pathFilter{}, func(fe fileEntry) error {
		for _, ref := range fe.Chunks {
			blob, ok := parseName(ref.ID)
			if !ok {
				return fmt.Errorf("verify: %s references a chunk under %q, which is no chunk name", fe.Path, ref.ID)
			}
			refs[blob]++
			total++
		}
		return ctx.Err()
	})
	if err == nil {
		err = verifyChunks(ctx, chunks, refs, settled, result, func(done int) {
			progress(ProgressEvent{Phase: PhaseFetch, CurrentPath: snapshotID, ChunksDone: done, ChunksTotal: total})
		})
	}
	// verifyChunks records chunk failures, so an error here wrapping
	// ErrCorruptTree is a tree that does not open or match its name. A run
	// that did not finish verified nothing past where it stopped.
	if err != nil {
		result.OK = false
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

// verifyChunks reads every chunk of refs, which counts the references to
// each, and records it as checked, missing or corrupt: one that fails its tag
// or does not hash to its name is corrupt. A chunk settled holds a verdict on
// is not read again, and every verdict found is added to settled unless it is
// nil. The rest is read pack by pack, in the order of the pack names. progress
// is called with the number of references settled so far: once for those that
// needed no read, when there are any, then after each pack. Only a failure
// that says nothing about a chunk is returned as an error.
func verifyChunks(
	ctx context.Context,
	chunks *objectStore,
	refs map[[sha256.Size]byte]int,
	settled map[[sha256.Size]byte]chunkVerdict,
	result *VerifyResult,
	progress func(done int),
) error {
	done := 0
	record := func(blob [sha256.Size]byte, v chunkVerdict) {
		switch v {
		case chunkChecked:
			result.ChunksChecked++
		case chunkMissing:
			result.MissingChunks = append(result.MissingChunks, hex.EncodeToString(blob[:]))
		case chunkCorrupt:
			result.CorruptChunks = append(result.CorruptChunks, hex.EncodeToString(blob[:]))
		}
		if settled != nil {
			settled[blob] = v
		}
		done += refs[blob]
	}
	// A chunk no index lists, or whose pack is gone, is missing. An index
	// that does not load says nothing about any chunk, also when the failure
	// wraps ErrNotFound: an index object that was listed and is gone.
	wanted := make(map[[sha256.Size]byte][]packEntry)
	for blob := range refs {
		if v, ok := settled[blob]; ok {
			record(blob, v)
			continue
		}
		pack, e, ok, err := chunks.index.find(ctx, blob)
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		if !ok {
			record(blob, chunkMissing)
			continue
		}
		wanted[pack] = append(wanted[pack], e)
	}
	if done > 0 {
		progress(done)
	}
	store := chunks.verifying()
	for _, pack := range slices.SortedFunc(maps.Keys(wanted), compareNames) {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller's own cancellation
		}
		entries := wanted[pack]
		slices.SortFunc(entries, func(a, b packEntry) int { return cmp.Compare(a.offset, b.offset) })
		err := store.getMany(ctx, hex.EncodeToString(pack[:]), entries, func(e packEntry, _ []byte, err error) error {
			switch {
			case err == nil:
				record(e.name, chunkChecked)
			case errors.Is(err, store.space.corruptErr):
				record(e.name, chunkCorrupt)
			case errors.Is(err, ErrNotFound):
				record(e.name, chunkMissing)
			default:
				return fmt.Errorf("verify: failed to read chunk %x: %w", e.name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		progress(done)
	}
	slices.Sort(result.MissingChunks)
	slices.Sort(result.CorruptChunks)
	result.OK = len(result.MissingChunks)+len(result.CorruptChunks) == 0
	return nil
}

// checkPacks compares the index of s with the packs that are stored. A pack
// the index lists that is gone is an error naming what only it held, which is
// lost; its records are passed over by every lookup until a prune or a
// rebuild drops them. Such a pack whose every blob a stored pack holds as
// well, a pack no index lists and an index object that does not open are
// warnings.
func (s *objectStore) checkPacks(ctx context.Context, result *CheckResult) error {
	space := s.space
	t, err := s.index.load(ctx)
	if err != nil {
		return err
	}
	for _, name := range t.unreadable {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s object %s does not open and is skipped: what only it lists counts as missing; "+
				"a prune removes it, an index rebuild lists the packs again", space.index, name))
	}

	stored := make([]bool, len(t.packs))
	var unlisted []string
	err = s.backend.List(ctx, space.packs, func(info BlobInfo) error {
		if i, listed := t.pack(info.Name); listed {
			stored[i] = true
		} else {
			unlisted = append(unlisted, info.Name)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ark: failed to list %s objects: %w", space.packs, err)
	}
	if len(unlisted) > 0 {
		slices.Sort(unlisted)
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s objects no index lists: %s; a backup that is running or did not finish leaves such packs "+
				"and a prune removes them, an index rebuild lists them when a snapshot misses what they hold",
			space.packs, nameList(unlisted)))
	}

	// A blob of a lost pack that a stored pack holds as well is not lost.
	lost := make(map[uint32][]string)
	if slices.Contains(stored, false) {
		for _, records := range &t.records {
			for _, rec := range records {
				if !stored[rec.pack] && !slices.ContainsFunc(t.copies(rec.name), func(c indexRecord) bool {
					return stored[c.pack]
				}) {
					lost[rec.pack] = append(lost[rec.pack], hex.EncodeToString(rec.name[:]))
				}
			}
		}
	}
	for i, pack := range t.packs {
		switch {
		case stored[i]:
		case len(lost[uint32(i)]) > 0:
			result.Errors = append(result.Errors, fmt.Sprintf(
				"%s object %x is listed by the index and not stored, %s objects no stored pack holds are lost: %s",
				space.packs, pack, space.blobs, nameList(lost[uint32(i)])))
		default:
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s object %x is listed by the index and not stored; a stored pack holds every %s object "+
					"it listed, a prune or an index rebuild drops what the index says about it",
				space.packs, pack, space.blobs))
		}
	}
	return nil
}

// checkSuspects reads the packs in which a blob did not open and reports
// those whose own table lists the blob elsewhere or not at all: there the
// index is wrong and the pack may be whole. A pack whose table does not open
// says nothing, its blobs were reported when they were read; one that cannot
// be read now is a warning beside them. Only the caller's cancellation is
// returned as an error.
func (s *objectStore) checkSuspects(ctx context.Context, result *CheckResult) error {
	space := s.space
	for _, pack := range slices.Sorted(maps.Keys(s.suspects)) {
		whole, err := readWhole(ctx, s.backend, Handle{Type: space.packs, Name: pack})
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err() //nolint:wrapcheck // the caller's own cancellation
		}
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s object %s: its own table was not compared with the index: %v", space.packs, pack, err))
			continue
		}
		entries, err := space.parse(whole)
		if err != nil {
			continue
		}
		own := make(map[[sha256.Size]byte]packEntry, len(entries))
		for _, e := range entries {
			own[e.name] = e
		}
		var off []string
		for _, e := range s.suspects[pack] {
			if own[e.name] != e {
				off = append(off, hex.EncodeToString(e.name[:]))
			}
		}
		if len(off) == 0 {
			continue
		}
		slices.Sort(off)
		// A table is bound to its blobs, not to the name of its pack, so it
		// opens under any name. A rebuild reads only packs named by their
		// hash: for any other it keeps what the index says.
		advice := "an index rebuild restores the index from the packs"
		if sum := sha256.Sum256(whole); hex.EncodeToString(sum[:]) != pack {
			advice = fmt.Sprintf("it is not named by its hash but holds the pack %x, "+
				"so an index rebuild takes nothing from it", sum)
		}
		result.Errors = append(result.Errors, fmt.Sprintf(
			"%s object %s: its own table disagrees with the index about %s objects %s; %s",
			space.packs, pack, space.blobs, nameList(slices.Compact(off)), advice))
	}
	return nil
}

// nameList names the first of names and counts the rest, so one finding about
// a pack of thousands of blobs stays one line.
func nameList(names []string) string {
	const show = 3
	if len(names) <= show {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:show], ", "), len(names)-show)
}
