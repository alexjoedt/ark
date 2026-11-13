# 0014: Index records of packs that are not stored are passed over

Status: partly superseded by 0015
Date: 2026-11-12

## Context

The index was trusted (see 0011 and 0012): a lookup returned the pack an index object listed and
never asked whether that pack was stored. A pack that went missing while its index object stayed
therefore broke more than the snapshots that needed it. Every later backup of the same data found
its blobs listed, stored nothing and wrote a snapshot that did not restore. The same happened when
someone with write access put back the index objects of a pruned backup, and a blob stored in two
packs resolved to the pack with the smaller name also when that pack was the lost one. Check
reported all three, but nothing short of a rebuild of the index stopped new backups from being
written broken.

## Decision

A load of an index lists the pack namespace once, after it has read the index objects, and marks
the packs its table lists that the listing lacks. A lookup passes over the records of a marked
pack: a blob listed only there counts as missing, so a backup that reads its content stores it
again, and a blob listed in a stored pack as well resolves to that one, of several to the one with
the smallest name (`index.go`).

The listing is taken after the index objects were read, never before. An index object is stored
only once a Sync covers its packs (see 0012), so every pack of an object the load read was stored
before that object and shows in a listing taken later. Taken first, the listing would lack the
packs of an index object another backup stored in between, and the load would pass over blobs that
are stored.

A listing that fails fails the load, and the operation with it. Nothing is passed over on a guess:
a record is ignored only for a pack that a complete listing lacks. The index objects already read
are kept, so the next lookup only lists the packs again. A backend's `List` returns an error for a
walk it did not finish: arklocal treats only an absent namespace as empty, not a directory that
vanished while it walked.

The records stay in the table. Check reports a listed pack that is not stored as an error naming
the pack and the blobs no stored pack holds, which are lost, and as a warning naming the pack when
a stored pack holds each of its blobs, after a backup stored them again. Prune still asks whether
any index object lists a referenced chunk, stored or not, leaves the records of a lost pack that
holds the only copy of a referenced blob for Check to report and does the rest of its work (see
0013); it drops the records of a lost pack once no snapshot needs what only that pack held, which
is the case after a backup stored the blobs again. A rebuild of the index lists stored packs only
and so drops them at once.

What an index writes after its load is kept beside the loaded table and is not held against the
listing: those are the packs of the run itself.

No window leaves a record that passes the listing and dangles within the run, as long as the locks
hold. Packs are deleted by prune alone, under the exclusive lock, and every operation that looks
blobs up holds a shared lock from before its load to its end. A pack that a concurrent backup
stores after the listing is not in the table that was loaded. A pack deleted between the reading
of the index objects and the listing is passed over, which is what is wanted. A pack removed
behind the lock, by hand or by a fault of the storage, after the listing was taken is as it was
before this decision: reads of it fail with `ErrNotFound`, the snapshot written by that run may
reference it, and Check reports it.

- Not verifying the packs a backup deduplicates against, because that reads pack bytes on every
  run (see 0012). The listing costs one request per index load and reads no pack.
- Not listing the packs before the index objects, because of the window described above.
- Not treating a failed listing as "every pack is stored", because that is the old behaviour
  exactly when the backend is in doubt, and not as "no pack is stored", because a backup would then
  store everything again.
- Not removing the records at load, because Check would lose the finding and a backup would have
  to hold the exclusive lock to rewrite the index.
- Not stopping prune on a referenced chunk that is listed only in a lost pack, because one lost
  pack would then block every prune until the snapshots that need it are forgotten (see 0013).
  A referenced tree listed only in a lost tree pack still stops it: without the tree the prune
  does not know which chunks it references. Its error says the pack is lost, since a rebuild
  cannot bring the tree back, and the prune runs once the snapshots Check names are forgotten.

This supersedes two parts of 0011, that a record whose pack is gone fails the read and that a read
does not fall to another copy, and the part of 0012 that says the index is trusted and that backup
does not heal.

## Consequences

A backup after a lost pack stores what it reads again, and its snapshot restores:
`TestBackupStoresAgainWhatALostPackHeld`. Older snapshots that reference the same blobs restore
again too, since their lookups end at the new copies. Replayed index objects of a pruned backup no
longer make a later backup store nothing: `TestReplayedIndexObjectIsPassedOver`. A blob in two
packs restores from the stored one: `TestRestoreReadsTheStoredCopyOfAChunkInTwoPacks`.

The repair reached only what a backup reads: a file that a backup takes as unchanged from its
parent keeps the parent's chunk references without a lookup (see 0003 and 0011). 0015 closes that
gap with a marker once an index load has seen the loss, which only a prune removes.

Every index load costs one more request, a listing of the pack namespace, and none when the index
lists no pack; an unchanged backup still reads no data index and so lists no data packs:
`TestIndexLoadsLazilyAndOncePerType`, `TestIndexPassesOverAPackThatIsNotStored`. On a backend
that shards the pack namespace into directories the listing reads each of them.
`TestIndexListsThePacksAfterItReadTheIndex` pins the order of the two listings and
`TestIndexLoadFailsWhenThePacksCannotBeListed` that a failed listing fails the lookup.

A backend's `List` now decides what a backup stores again: one that returns a part of a namespace
without an error makes stored packs look lost for that run, whose reads fail and whose backup
stores their blobs a second time. No stored blob is deleted for it.

Prune is unchanged for data packs: `TestScenarioCheckCoversEverySnapshot`,
`TestPruneLeavesALostPackToCheck`; on a tree of a lost pack it stops as before, with an error that
says so: `TestPruneStopsOnATreeOfALostPack`. A read of an object listed only in a pack that is not
stored fails with `ErrNotFound` and says that, apart from one that no index lists.
