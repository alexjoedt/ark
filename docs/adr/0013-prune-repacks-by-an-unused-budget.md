# 0013: Prune repacks by a repository-wide unused budget

Status: accepted
Date: 2026-11-11

## Context

With packs (see 0010) a dead blob shares its pack with live ones. A prune that only deletes packs
without any referenced blob (see 0012) frees nothing when a forgotten snapshot shared its packs
with a kept one, which is the normal case. Prune therefore has to copy live blobs into new packs.
Open were which packs it rewrites, so that a small forget does not rewrite the repository; in which
order it writes and deletes, so that a kill at any point loses no live blob; what it does with a
blob stored twice; and what it reports.

## Decision

Prune works to a budget: at most 5% of the blob bytes in the repository may be unused when it
ends. Unused are blobs no snapshot references and second copies of a blob. Packs without a live
blob and packs no index lists are deleted. Of the data packs that hold both live and unreferenced
blobs, prune rewrites those with the largest unused share first and stops once the budget holds. A
tree pack with an unreferenced node is always rewritten, because every walk fetches tree packs
whole. The value is fixed and there is no cap on what one run rewrites (`gc.go`).

A rewrite copies the sealed bytes of the live blobs through the pack writer of a backup. Before a
blob is copied its seal is opened and the plaintext discarded: no decompression, no rehash. A blob
that does not open ends the prune before anything is deleted, so damage is never copied into a
pack the index vouches for. The packs stored until then are listed in the index first, so the next
prune counts their blobs as copied and does not store them again.

The order is: new packs, Sync, the index, Sync, the old index objects deleted, Sync, the old packs
deleted, Sync. The copying lists its packs like a backup does, at its checkpoints and when it
ends, and only then is the complete index written from memory and every older index object
deleted, those of the copying included. The index is rewritten in full, and only when a pack went
or more index objects exist than a rewrite would leave.

A referenced blob listed in several packs counts as live in the pack with the smallest share of
unreferenced bytes, in the one with the smallest name among equals. The output of an interrupted
rewrite holds no unreferenced blob, so it wins over the pack it was copied from, and the next
prune deletes that pack without reading it again. What it reads first is the copy that stays:
before a pack with a second copy of a referenced blob is deleted, the seal of the copy the blob
counts in is opened, as for a blob that is copied, and one that does not open ends the prune with
nothing deleted. That copy is the last one afterwards, and nothing has read it since it was
stored.

Second copies in a pack that also holds an unreferenced blob count toward the budget and go when
the pack is rewritten. A pack whose only unused bytes are second copies is never selected, and
those bytes do not count against the budget: no rewrite frees them, and counted they would have
every partly dead pack rewritten at every prune. They are reported as unused.

A pack that is listed but no longer stored is not selected and its bytes are not counted: prune
cannot copy from it, its entries stay in the index, and check reports it. A blob it lists counts
as live in it only when no stored pack holds the blob.

`PruneResult` gains `PacksDeleted`, `BytesRepacked` and `BytesUnused`. `BytesFreed` is the size of
the deleted packs less the bytes copied, `BlobsRemoved` the index entries that went less the blobs
copied. A dry run computes all of them from the same plan and writes nothing.

- Not a threshold per pack, because the cost that matters is the unused share of the whole
  repository: a per-pack rule either rewrites many packs for little gain or leaves a repository of
  many slightly dead packs far above any bound.
- Not a configurable budget, because no measurement asks for another value yet.
- Not a cap on the bytes rewritten per run, because an interrupted rewrite keeps its finished
  packs and the next prune continues from them.
- Not merging small live packs, because a pack count has not been shown to cost anything.
- Not rewriting only the index objects that mention a touched pack, because the full rewrite is
  one code path with the rebuild and also merges what backups left; its upload is about 44 bytes
  per blob.
- Not copying without opening the seal, because a rewrite is the last moment the damaged blob can
  still be told apart from its neighbours cheaply; afterwards the old pack is gone.

## Consequences

Forgetting snapshots frees their space at the next prune, within 5% of the blob bytes that remain:
`TestPruneRepacksUntilTheUnusedBytesFitTheBudget` forgets half of the snapshots and checks which
packs are rewritten, which stay and what is reported; `TestPruneAlwaysRewritesAPartlyDeadTreePack`
covers the tree rule. A prune after a large forget rewrites up to the live share of every selected
pack in one run and reports it as progress.

A kill leaves a state the next prune finishes. During the copying: new packs that are listed, with
their blobs stored twice, or unlisted, which are deleted. During the index rewrite: entries listed
twice with every listed pack stored. During the pack deletes: old packs no index lists.
`TestPruneFinishesAnInterruptedRepack` interrupts before and inside the rewrite and expects the
old packs deleted without a new pack, `TestPruneCountsABlobStoredTwiceOnce` pins the rule for
second copies, `TestPruneWritesInAnOrderAKillCannotBreak` the order and its Syncs, and
`kill_test.go` kills prune at random points.

A damaged live blob in a pack that prune wants to rewrite stops every prune until it is dealt
with: `TestPruneAbortsOnABlobThatDoesNotOpen`. Each of those prunes stores at most what the one
before had not copied yet: `TestPruneAbortedRepackIsNotCopiedAgain`. A damaged copy that prune
would keep while it deletes the other stops it the same way, and `RebuildIndex` then lists the
blob in the pack that opens: `TestPruneReadsTheCopyItKeepsBeforeItDeletesTheOther`. The prune
after an interrupted rewrite therefore reads the copied blobs once more, from the new packs. A
lost pack stops nothing, and a blob it shares with a stored pack stays in that one:
`TestPruneLeavesALostPackToCheck`, `TestPruneCountsALostCopyOnlyWithoutAStoredOne`.

Second copies alone can keep a repository above the budget, without making prune rewrite the
packs that are within it: `TestPruneSecondCopiesDoNotSelectOtherPacks`. They come from two
writers storing the same new blob at the same time, which is rare, and they go when their pack is
rewritten for another reason.
