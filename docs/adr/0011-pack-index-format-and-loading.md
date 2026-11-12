# 0011: Pack index format and loading

Status: accepted
Date: 2026-11-11

## Context

Once chunks and tree nodes sit in pack objects, something has to say which pack holds a blob and
where. That index is part of the repository format and it decides what an incremental backup
costs: today a backup of unchanged files reads the parent's tree nodes and nothing else, and an
index that has to be downloaded before every run would end that. restic and rustic keep a JSON
index of all blobs, load it at the start of every operation and look every blob of an unchanged
file up in it before they reuse the parent's list; both rely on a local cache to make that cheap.
ark has no local cache and is not getting one for this. Open were the object format, how many
index objects a backup writes and who merges them, when an index is loaded, what a reader does with
an index object that does not open, and how an index is restored when it is lost.

## Decision

There is one index per blob type, in its own namespace with its own handle type: `dataindex` for
chunks and `treeindex` for tree nodes.

An index object is a header of record count and pack count, a table of 32-byte pack names, and one
record per blob: the 32-byte blob name, the number of its pack in the table, the offset in the pack
and the sealed length. Every number is 4 bytes little endian, a record is 44 bytes, and records are
sorted by blob name. There is no type byte and no plaintext length. The object is sealed like any
single object, with `<namespace>/<name>` as associated data, and named by the keyed hash of its
content, as chunks and tree nodes are named.

An index object is closed once it lists at least 50 000 blobs after a pack was added, so the blobs
of one pack are never split across objects and a run that ends writes one short object. A backup
never merges index objects; prune is to write one merged set.

Each index is loaded lazily and whole. Opening a repository reads no index object. The first lookup
of a type lists its namespace once, loads every object and merges them into one table sorted by blob
name, which later lookups search by binary search; lookups that arrive during the load wait for it.
A writer loads each index once and never lists again. Every backup needs the tree index, because
walking the parent has to locate its tree nodes. The data index is needed only for a file that
changed, and for restore, check and prune: a completed parent is to be trusted without a lookup,
since its manifest exists only after its packs and index objects were durable, and blobs are
removed only under the exclusive lock.

The budget is at most 48 bytes of heap per blob in the loaded table and at most one second to load
and merge a million blobs from memory.

An index object that does not open is skipped with a warning. The blobs only it lists count as
missing: a backup stores them again, a restore that needs one fails. A record whose pack is gone
fails the read like a missing object does. A pack no index lists is garbage for prune.

Two writers may store the same new blob, so a blob can be listed in several packs. A read takes
the first copy of the merged table, which is the one in the pack with the smallest name, and does
not fall back to another.

A lost index is rebuilt from the packs, each of which carries its own blob table. That is to be a
library function, not a command.

- Not JSON, because a million blobs would cost several times the 44 bytes per record in download,
  parse time and memory, and the index is read by ark only.
- Not one index for both types, because every backup would then load the records of all chunks to
  find a few hundred tree nodes.
- Not a local cache of the index, because it is a second copy to keep consistent; a measurement has
  to ask for it first.
- Not a lookup of every blob of an unchanged file, because it would load the data index on every
  incremental backup. The price is that a damaged repository carries the damage into new snapshots
  until a check runs.
- Not a partial load by name prefix or by object, because a lookup of a random name needs every
  object anyway and the whole table of a type fits the budget.
- Not merging index objects during a backup, because deleting the merged objects is safe only under
  the exclusive lock, which a backup does not hold.
- Not a fallback to another copy of a duplicate, because a read path that depends on which copies
  exist is hard to test; a damaged first copy is a finding for check.

## Consequences

An incremental backup with no changed file reads the tree index and no data index object. A
repository of ten million chunks costs about 440 MB of memory and as much download whenever the
data index loads; that is accepted until a measurement asks for a cache. Index objects pile up, at
least one per backup and type, until prune merges them; they are loaded one after another.

The format is pinned by `TestIndexAnswersLookupsForEveryBlob` and
`TestIndexObjectClosesAtTheRecordCap`, lazy loading and the single load under concurrent first
lookups by `TestIndexLoadsLazilyAndOncePerType`, the duplicate rule by
`TestIndexDuplicateBlobResolvesToTheFirstCopy`. A changed, truncated or moved index object, and one
whose content does not describe itself, fails with `ErrCorruptIndex`
(`TestIndexTamperedObjectsAreRejected`) and is skipped by a load
(`TestIndexSkipsAnObjectThatDoesNotOpen`), while a backend error fails the lookup. A load reads such
an object a second time before it skips it, so one short read does not make its blobs missing for
the run (`TestIndexReadsAnObjectAgainBeforeSkippingIt`). An index that has loaded keeps what it
writes afterwards beside its table instead of listing again, and answers as an index that loads
later does (`TestIndexFindsWhatItWroteAfterLoading`).
`TestIndexRebuiltFromPacksEqualsTheWrittenOne` shows that the tables of the packs give the same
index; a pack that does not open or is not named by its hash is reported and describes nothing to
a rebuild. `Repository.RebuildIndex` then keeps what the old index lists in that pack, because one
changed byte makes the table of a pack unreadable and with it the place of every intact blob
beside the damaged one (`TestRebuildIndexKeepsWhatTheOldIndexKnowsOfADamagedPack`).
`BenchmarkIndexLoad` measures the budget on the memory backend, on its best run, and fails beyond
it. The loaded table keeps its records in 64 slices by the leading bits of the blob name, so that
loading never holds all records twice. The unit is `index.go`.
