# 0012: Crash and concurrent-writer model for packs

Status: accepted
Date: 2026-11-11

## Context

With one object per chunk, "present" meant that the backend held an object of that name, and a
power loss could leave such an object torn. The answer was a marker per running backup and a
repair mode that read back every object a backup reused while any marker existed (see 0005).
Packs (see 0010) and the index (see 0011) change the question: a blob is present once an index
object lists it, several writers add packs and index objects at the same time, and a backup of
many hours must not lose all of its upload to one interruption. Open were the order in which a
writer makes its objects durable, what an interrupted run keeps, what markers and repair mode
become, what prune may delete, and what `Backend.Sync` has to promise for all of this to hold.

## Decision

A backup and an import write in this order: packs, Sync, the index objects that list them, Sync,
the manifest, Sync. The rule behind it: an index object is stored only after a Sync that followed
the Put of every pack it lists. An index entry therefore never points at bytes a power loss can
still undo, and a manifest never at blobs without a durable index entry (`packsession.go`).

A long run checkpoints. When a pack is stored and 50 000 blobs wait for an index object, or ten
minutes have passed since the last flush, the run syncs and stores index objects for the packs
stored so far. An interrupted run loses at most the upload since its last checkpoint: the next
run finds the listed blobs in the index and does not store them again. Within a run, a set of the
blobs it has put into packs covers what no index lists yet.

Index objects are stored under the context of the operation, never detached from it. A writer
that was stopped, or whose lock went stale and lost its context, cannot publish an index for packs
a prune has deleted in the meantime.

Markers and repair mode are removed, with their namespace. A pack that was cut short is listed by
no index, because its index object would have come after the Sync; nothing reads it and prune
deletes it. Check keeps its warning for a lock that does not open.

Two writers may store the same new blob. Both copies stay, readers take the first one of the
merged index, and prune decides later which pack keeps it.

Prune runs under the exclusive lock. It loads both indexes, walks every snapshot, and deletes
nothing when a snapshot references a blob no index lists: the error names `RebuildIndex`. Only then
do packs without a referenced blob, packs no index lists and index objects that do not open count
as garbage. It stores the complete new index, syncs, deletes the old index objects, syncs, and only
then deletes packs, with a barrier at the end. The Sync between the index and the packs is
required: without it a power loss could bring back an old index object whose packs are gone
(`gc.go`).

`Backend.Sync` covers every object visible at the repository location, whoever wrote it. arklocal
flushes the file system, arksftp makes each Put durable on its own, arkmem has nothing to flush. A
future backend has to keep this: a writer relies on one barrier for the packs of every writer its
index may come to list beside.

The repository format number becomes 3. A repository of version 2 is refused with
`ErrUnsupportedFormat`; there is no migration and no reading of the old layout.

This supersedes the part of 0005 about torn objects, markers and repair mode. The Sync barrier of
0005 stays.

- Not markers with packs, because a marker says "distrust what exists", and the index already says
  what exists. A second mechanism would only add a state to get wrong.
- Not an index object per pack, because a small backup would then double its object count, and the
  SFTP cost that packs remove is per object.
- Not an index written before its packs are synced, because a power loss could then leave an entry
  for bytes that never reached the disk, and every later backup would reuse it.
- Not a final index only, because a backup interrupted after ten hours would upload ten hours
  again.
- Not self-healing in backup, because verifying what the index lists would read the packs on every
  run. A damaged repository is a finding for check.

## Consequences

A killed or cancelled backup leaves packs behind and no state that later runs have to honour: no
marker, no repair mode, no extra reads after an unclean end. What it stored before its last
checkpoint is reused, the rest is deleted by the next prune. `TestDurabilityBarriers` and
`TestImportWritesInBackupOrder` pin the write order by replaying every Put and Sync,
`TestBackupCheckpointsItsIndex` and `TestSessionCheckpointsAtTheRecordCap` the two checkpoint
triggers and the reuse after an interrupt.

Prune depends on a complete index. `TestPruneAbortsWhenTheIndexHidesALiveBlob` cuts an index object
short and expects no delete, `TestPruneDeletesDeadAndUnlistedPacks` covers what goes and that the
index is rewritten without it. Until packs are repacked, a pack with one referenced blob stays
whole, so forgetting a snapshot frees only packs that are entirely unreferenced. 0013 adds the
repacking.

The index is trusted. Someone who can write to the backend can put back the index objects of a
pruned backup; a later backup of the same data then stores nothing and its snapshot does not
restore. It never restores other bytes, and check reports it:
`TestReplayedIndexObjectNeverRestoresOtherBytes`.

Old repositories do not open: `TestOpenRejectsTheFormatBeforePacks`.
