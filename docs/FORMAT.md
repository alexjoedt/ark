# Repository format

This describes what ark stores in a repository, format version 3. The byte layout is the contract
of the version: a build reads and writes exactly this. The reasons behind the layout are in
[docs/adr](adr/README.md) (0010 to 0013); what each object reveals to someone who can read the
storage is in [SECURITY.md](SECURITY.md).

## Version

The `config` object carries `version`. A build opens only its own version and refuses any other
with `ErrUnsupportedFormat`. Format version 2 stored one object per chunk and per tree node; ark
does not read it and does not migrate it. A version 2 repository has to be re-initialised: create
a new repository with `ark init` and back up again.

## Layout

```
config       plaintext JSON, authenticated through a hash sealed in the master key
keys/        key files: the master key sealed under the passphrase
datapacks/   packs of file content chunks
treepacks/   packs of directory nodes
dataindex/   index objects: which pack holds which chunk
treeindex/   index objects: the same for directory nodes
snapshots/   one sealed manifest per snapshot, named by a random id
locks/       sealed shared and exclusive locks
```

`config`, the key files, snapshot manifests and locks stay single objects, one per file. Chunks
and directory nodes are never stored alone: they live in packs, and the index finds them.

The directory and SFTP backends shard `datapacks/` and `treepacks/` one level deep by the first two
characters of the name, so a pack is `datapacks/3f/3f9a...`. That is at most 256 directories per
namespace, created when first needed. All other namespaces are flat. The sharding is a property of
those backends, not of the format: the memory backend and any other implementation of
`ark.Backend` store the same names.

## Packs

A pack is the sealed blobs in arrival order, then the sealed blob table, then the length of the
sealed table as 4 bytes little endian.

- A blob is one chunk or one directory node, sealed on its own like a single object, with the
  associated data `<namespace>/<name>`. A blob moved within a pack or into another does not open.
- The table has one entry per blob: its 32-byte name and its sealed length as 4 bytes little
  endian. Offsets follow from the order. The table is sealed with the data key and the associated
  data `<pack namespace>/<hex SHA-256 of the blob section>`, so it opens only behind the blobs it
  describes.
- A pack is named by the hex SHA-256 of all its bytes. `sha256sum` on the storage host therefore
  checks every pack against its name without ark and without the key:

  ```bash
  cd /srv/ark && sha256sum datapacks/*/* treepacks/*/* | awk '{ n=split($2,p,"/"); if ($1 != p[n]) print "damaged:", $2 }'
  ```

  An index object, a manifest or a lock is named differently and cannot be checked this way.
- A pack closes once it holds at least 16 MiB of blobs, so it overshoots by one blob at most. The
  last pack of a run is as short as it is. Offsets and lengths are 32 bits, so a pack and a blob
  stay below 4 GiB.
- Chunks go into `datapacks`, directory nodes into `treepacks`. Tree packs are fetched whole;
  chunks are read by range.

Chunk and node names are the hex HMAC-SHA256 of their plaintext under the repository's ID key;
the name of a pack is not keyed.

## Index objects

There is one index per blob type, `dataindex` for chunks and `treeindex` for directory nodes. An
index object is a header of record count and pack count, a table of 32-byte pack names, and one
record per blob: the 32-byte blob name, the number of its pack in the table, the offset in the
pack and the sealed length. Every number is 4 bytes little endian, a record is 44 bytes, and
records are sorted by blob name. The object is sealed like any single object and named by the keyed
hash of its content.

A backup writes index objects for the packs it stored and never merges them, so there is at least
one index object per run and type. An index object lists at least 50 000 blobs when it is
closed, and the blobs of one pack are never split across two objects. Prune merges them.

ark loads an index when the first lookup of its type needs it: a repository is opened without
reading any index object, an incremental backup with no changed file reads the tree index only.
The index is trusted: a lookup returns the pack, offset and length it lists and reads that range.
Two writers may store the same new blob, so a blob can be listed for two packs; a read takes the
copy in the pack with the smaller name, and prune removes the other.

## Order of writes

A backup and an import write packs, then synchronise, then the index objects that list them, then
synchronise, then the manifest, then synchronise. A manifest therefore never refers to blobs
without a durable index entry, and an index entry never points at bytes a power loss can still
undo. A long run does this in steps as it goes; an interrupted run loses at most the upload since
its last step. A pack that no index lists is left behind and removed by the next prune.

## Prune

Prune runs under the exclusive lock and works to a budget: at most 5% of the blob bytes may be
unused when it ends. Packs without a referenced blob are deleted. Packs that hold both referenced
and unreferenced blobs are rewritten, those with the largest unused share first, until the budget
holds; a tree pack with an unreferenced node is always rewritten. A rewrite copies the sealed bytes
of the referenced blobs into new packs without opening them. Then it writes the new index, deletes
the old index objects and last the old packs, with a synchronisation between the steps. Prune
deletes nothing when a snapshot references a blob that no index lists.

## Check and repairing the index

`Check` verifies every snapshot and compares the index with the packs that are stored. It reports as
errors a pack the index lists that is gone, and a pack whose own table disagrees with the index; as
warnings it reports a pack no index lists and an index object that does not open. While the index is
intact it opens no pack table, so a changed table in a pack whose blobs no snapshot reads is found
by `sha256sum` on the pack, not by `Check`.

The index can be rebuilt from the packs. `Repository.RebuildIndex` reads every pack whole, checks
that the pack is named by the hash of its bytes, writes a new index from the tables and deletes the
old index objects. A pack whose table does not open, or whose name is not the hash of its bytes, is
named in the result and takes nothing from it; the new index keeps what the old one listed for that
pack, except for blobs that a pack which opens holds as well. A rebuild needs the repository to
itself, like prune, and it downloads every pack. It is a library function; there is no `ark`
command for it yet.

## Everything else

The master key, the key files, the config, snapshot manifests, locks and the sealing of every
object are as described in [SECURITY.md](SECURITY.md#format).
