# 0010: Pack object layout and blob binding

Status: accepted
Date: 2026-11-11

## Context

Every chunk and every tree node is one backend object. On a backend where an object costs a round
trip and an fsync that cost dominates: a backup of 40 000 small files over SFTP takes minutes where
restic takes seconds, and the files occupy three times their stored bytes on disk. Chunks and tree
nodes therefore move into pack objects of about 16 MiB. The byte layout of a pack is part of the
repository format, so it had to be fixed before the first stable release: where the description of
a pack lives, what a blob and the description are bound to, how a pack is named, and when one is
closed. restic, rustic and kopia all end a pack with a table of its blobs and keep file data and
tree data in separate packs.

## Decision

A pack is the sealed blobs in arrival order, then the sealed blob table, then the length of the
sealed table as 4 bytes little endian. The table has one entry per blob: the 32-byte name and the
sealed length as 4 bytes little endian. Offsets follow from the order.

Each blob is sealed on its own with the codec and the associated data `<namespace>/<name>` a single
chunk or tree object has, so a blob moved inside a pack or into another does not open, and moving
a blob between packs copies ciphertext without key work. The table is sealed with the data key and
the associated data `<pack namespace>/<hex SHA-256 of the blob section>`. The writer takes that sum
from the hash it runs over the pack anyway, so a pack is written in one pass, and a table placed
behind other blobs does not open.

A pack is named by the hex SHA-256 of all its bytes. Chunks go into data packs and tree nodes into
tree packs, two namespaces (`datapacks`, `treepacks`) with their own handle types. The directory
backends are to shard each one level deep by the first byte of the name, creating a directory on
first use.

A pack closes once it holds at least 16 MiB of blobs after an append, so it overshoots by one blob
at most. A blob of at least 16 MiB gets a pack of its own. The last pack of a run is written as
short as it is; nothing is carried over to the next run. Packs are assembled in memory, one open
pack per type; the goroutine that fills a pack uploads it, and at most four uploads are in flight
across both types. While all four are busy, the add that fills the next pack waits and holds back
the adds behind it, so full packs do not queue up: about 120 MiB at the peak, not configurable.

- Not a table at the front, because its content is known only after the last blob, and not a pack
  without a table, because then a lost index could not be rebuilt from the packs.
- Not blobs bound to their pack, because moving a live blob out of a mostly dead pack would have to
  open and seal it again.
- Not random pack names, because a name that is the hash of the bytes lets `sha256sum` on the
  storage host check a pack without ark and without the key.
- Not one pack namespace with a type per blob, because tree packs are fetched whole and chunks are
  read by range.
- Not padding and not a random assignment of blobs to packs: blob boundaries sit in the sealed
  table, an observer sees pack sizes and counts only, and sizes per namespace are an accepted leak.
- Not a magic number or version byte in the pack, because the repository format version covers the
  layout.
- Not a configurable pack size, because one fixed target keeps the memory bound and the format
  simple.

## Consequences

A backup writes a handful of objects instead of one per chunk. Reading a blob takes its pack,
offset and sealed length and one range read; the blob authenticates on its own
(`TestPackBlobsReadBackThroughOneRangeRead`). Opening a table needs the whole pack, since the
table is bound to the hash of the blob section: rebuilding an index from the packs downloads them.
Offsets and lengths are 32 bits, so a pack and a sealed blob stay below 4 GiB.

A flipped byte anywhere in a pack, a truncated pack and a table behind another pack's blobs fail
with `ErrCorruptChunk` for a data pack and `ErrCorruptTree` for a tree pack
(`TestPackTamperedBytesAreRejected`, `TestPackTableDoesNotOpenBehindOtherBlobs`). The size rule is
pinned by `TestPackWriterClosesPacksAtTheTargetSize`, the upload bound by
`TestPackWriterBoundsUploadsInFlight`, concurrent writers by `TestPackWriterConcurrentAdds`. The
unit is `pack.go`.
