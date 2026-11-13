# Repository format

This describes what ark stores in a repository, format version 3. The byte layout is the contract
of the version: a build reads and writes exactly this. The reasons behind the layout are in
[docs/adr](adr/README.md) (0010 to 0015); what each object reveals to someone who can read the
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
lostpacks/   at most one sealed marker: an index load found a listed data pack gone or a data index object that does not open
```

`config`, the key files, snapshot manifests, locks and the marker stay single objects, one per
file. Chunks and directory nodes are never stored alone: they live in packs, and the index finds them.

The directory and SFTP backends shard `datapacks/` and `treepacks/` one level deep by the first two
characters of the name, so a pack is `datapacks/3f/3f9a...`. That is at most 256 directories per
namespace, created when first needed. All other namespaces are flat. The sharding is a property of
those backends, not of the format: the memory backend and any other implementation of
`ark.Backend` store the same names.

## Packs

A pack is the sealed blobs in arrival order, then the sealed blob table, then the length of the
sealed table as 4 bytes little endian.

- A blob is one chunk or one directory node, sealed on its own like a single object, with the
  associated data `chunks/<name>` for a chunk and `trees/<name>` for a directory node, `<name>`
  being the blob's hex name. No object is stored under `chunks` or `trees`: the associated data
  only binds a blob to its type and name, so the bytes of one blob read where the index lists
  another do not open.
- The table has one entry per blob: its 32-byte name and its sealed length as 4 bytes little
  endian. Offsets follow from the order. The table is sealed with the data key and the associated
  data `datapacks/<hex SHA-256 of the blob section>` or `treepacks/<...>` by the pack's namespace,
  so it opens only behind the blobs it describes.
- A pack is named by the hex SHA-256 of all its bytes. `sha256sum` on the storage host therefore
  checks every pack against its name without ark and without the key:

  ```bash
  cd /srv/ark && find datapacks treepacks -type f -exec sha256sum {} + |
    awk '{ n=split($2,p,"/"); if ($1 != p[n]) print "damaged:", $2 }'
  ```

  An index object, a manifest or a lock is named differently and cannot be checked this way.
- A pack closes once it holds at least 16 MiB of blobs, so it overshoots by one blob at most; a
  blob of 16 MiB or more gets a pack of its own. The last pack of a run is as short as it is.
- Offsets and lengths in the table and the index are 32 bits. The writer refuses a sealed blob of
  4 GiB or more, so the blob section of a pack it stores is below 32 MiB or one blob below 4 GiB;
  a pack whose table lists 4 GiB or more does not parse. The sealed table and its length come on
  top of the blob section.
- Chunks go into `datapacks`, directory nodes into `treepacks`. A tree pack is fetched whole when
  a node of it is first read and serves the nodes beside it from memory; chunks are read by range.

Chunk and node names are the hex HMAC-SHA256 of their plaintext under the repository's ID key;
the name of a pack is not keyed.

## Index objects

There is one index per blob type, `dataindex` for chunks and `treeindex` for directory nodes. An
index object is a header of record count and pack count, a table of 32-byte pack names, and one
record per blob: the 32-byte blob name, the number of its pack in the table, the offset in the
pack and the sealed length. Every number is 4 bytes little endian, a record is 44 bytes, and
records are sorted by blob name. The object is sealed like any single object and named by the keyed
hash of its content.

A backup writes index objects for the packs it stored: at a checkpoint, when a pack is stored
while 50 000 blobs wait for an index entry or 10 minutes have passed since the last one, and at
its end. A type of which a run stored no pack gets no index object, so a backup that stores
nothing new writes none. A backup never merges index objects. An object is closed once it lists
at least 50 000 blobs, the last one of each write is as short as it is, and the blobs of one pack
are never split across two objects. Prune writes the index of a type anew when a listed pack
goes or is rewritten, when an index object does not open, or when there are more objects than
one per 50 000 blobs listed.

ark loads an index when the first lookup of its type needs it: a repository is opened without
reading any index object, an incremental backup with no changed file reads the tree index only.
A load ends with one listing of the pack namespace, taken after the index objects were read, and a
lookup passes over what the index lists in a pack that listing lacks: such a blob counts as
missing, so a backup that reads its content stores it again. A listing that fails fails the load.
Otherwise a lookup returns the pack, offset and length the index lists and reads that range. Two
writers may store the same new blob, so a blob can be listed for two packs; a read takes the copy
in the stored pack with the smaller name. Prune does not remove such a second copy on its own: it
goes when prune deletes or rewrites the pack that holds it, see below.

## Lost-pack marker

A load of the data index that finds a listed pack gone, or a data index object that does not open,
stores the marker `lostpacks/<name>`, where
the name is the keyed hash of the string `lostpacks`, so it differs between repositories. It is
sealed like any single object over empty content; only its presence counts, and a marker that does
not open counts as present. A marker that is stored already is not written again. Backup and
import fail when they cannot store it; restore, export, verify, check, prune and an index rebuild
log the failure and go on. A prune dry run does not store it.

A backup that would take files unchanged from their parent asks for the marker with one request.
Without it the backup reads no data index, as before. With it the backup loads the data index and
reads again every file of which a chunk is in no stored pack. A failed request counts as a marker.
A backup whose own load of the data index finds the loss after it took files unread fails before it
stores a snapshot when one of those files has a chunk in no stored pack; the next backup reads them.

Only prune removes the marker, under the exclusive lock and after the new index is durable, once
every chunk a snapshot references is listed in a stored pack. An index rebuild never removes it: a
snapshot may still reference a chunk whose records it dropped. A build that predates the marker
ignores it.

## Order of writes

A backup and an import write packs, then synchronise, then the index objects that list them, then
synchronise, then the manifest, then synchronise. A manifest therefore never refers to blobs
without a durable index entry, and an index entry never points at bytes a power loss can still
undo. A long run does this in steps as it goes; an interrupted run loses at most the upload since
its last step. A pack that no index lists is left behind and removed by the next prune.

## Prune

Prune runs under the exclusive lock. It counts each referenced blob as live in one pack: of
several copies, the one in the stored pack with the smallest share of unreferenced bytes, of
several such the one in the pack with the smallest name. Every other blob byte is unused: blobs
no snapshot references and the other copies. Packs without a live blob are deleted, and so are
packs no index lists. A tree pack that holds an unreferenced node is always rewritten. Data packs
that hold an unreferenced blob are rewritten, those with the largest unused share first, until
their unused bytes are at most 5% of the blob bytes left after the prune. A pack whose only unused
bytes are second copies is never rewritten and its second copies do not count against the 5%, and
a pack the index lists that is gone is left for `Check` to report and counts nowhere; so the
unused bytes a prune reports can be more than 5%.

A rewrite copies the sealed bytes of the live blobs into new packs as they are, without sealing
them again, but opens each one first; so does prune with the copy that stays of a blob whose other
copy goes with its pack. A blob that does not open ends the prune with nothing deleted. Then prune
writes the new index, deletes the old index objects and last the old packs, with a
synchronisation between the steps. Prune deletes nothing when a snapshot references a blob that no
index lists.

## Check and repairing the index

`Check` verifies every snapshot: it reads every chunk and directory node a snapshot references
through the index and reports one that is missing or does not open as an error. It compares the
index with the packs that are stored: a pack the index lists that is gone is an error, a pack no
index lists and an index object that does not open are warnings. A pack in which a referenced blob
did not open is then read whole and its own table compared with the index; a table that lists that
blob elsewhere or not at all is an error, since there the index is wrong and the pack may be whole.
`Check` opens no other pack table, so a changed table in a pack whose referenced blobs all open, or
whose blobs no snapshot references, is found by `sha256sum` on the pack, not by `Check`.

The index can be rebuilt from the packs. `Repository.RebuildIndex` reads every pack whole, checks
that the pack is named by the hash of its bytes, writes a new index from the tables and deletes the
old index objects. A pack whose table does not open, or whose name is not the hash of its bytes, is
named in the result and takes nothing from it; the new index keeps what the old one listed for that
pack, except for blobs that a pack which opens holds as well. A rebuild needs the repository to
itself, like prune, and it downloads every pack. `ark repair index` runs it.

## Everything else

The master key, the key files, the config, snapshot manifests, locks and the sealing of every
object are as described in [SECURITY.md](SECURITY.md#format).
