# Repository format

This page describes what ark stores in a repository, format version 1. The byte layout is the
contract of the version: a build reads and writes exactly this. What each object reveals to
someone who can read the storage is in [Security](SECURITY.md).

For the ideas without the bytes, see [How ark works](concepts.md).

## Version

The `config` object carries `version`.

- A build opens only its own version. It refuses any other with `ErrUnsupportedFormat`; the
  message names the version found and the version the build reads.
- Version 1 is the first stable format and is frozen with `v1.1.0`. Later changes are additive or
  come with a version bump.
- ark doesn't migrate an older format. Create a new repository with `ark init` and back up again.

## Layout

```
config       plaintext JSON, authenticated through a hash sealed in the master key
keys/        key files: the master key sealed under the password
datapacks/   packs of file content chunks
treepacks/   packs of directory nodes
dataindex/   index objects: which pack holds which chunk
treeindex/   index objects: the same for directory nodes
snapshots/   one sealed manifest per snapshot, named by a random ID
locks/       sealed shared and exclusive locks
lostpacks/   at most one sealed marker, see below
```

- `config`, the key files, the manifests, the locks, and the marker are single objects, one per
  file.
- Chunks and directory nodes are never stored alone. They live in packs, and the index finds them.
- A manifest holds the source type, source name, host, tags, comment, statistics, and the labels
  under the key `source_meta`. See [Labels](labels.md) and [Series](series.md).
- A lock is stale once its recorded time is 30 minutes old, or at once when it's from this host
  and its process is gone.

### Sealed objects

Every sealed object has one layout: `format byte | 24-byte nonce | XChaCha20-Poly1305(payload)`.
The format byte is `0x02` when the payload is the zstd frame of the content, and `0x03` when it is
the content as it is. A reader opens both, for every object. The associated data is the format
byte followed by `<namespace>/<name>`, for example `snapshots/<id>` for a manifest, so an object
moved to another name or relabelled with the other codec fails on its tag.

Index objects, manifests, locks, and the marker are sealed this way with the data key under their
own name. The `key` field of a key file is sealed the same way under the key derived from the
password, with the associated data `keys/<name>`. The `config` isn't sealed.

### Sharding

The directory and SFTP backends shard `datapacks/` and `treepacks/` one level deep by the first two
characters of the name. A pack is `datapacks/3f/3f9a...`. That makes at most 256 directories per
namespace, created when first needed. All other namespaces are flat.

Sharding is a property of those backends, not of the format. The memory backend and any other
implementation of `ark.Backend` store the same names.

## Packs

A pack has three parts, in this order:

1. the sealed blobs, in arrival order
2. the sealed blob table
3. the length of the sealed table, 4 bytes little endian

**Blobs.** A blob is one chunk or one directory node, sealed on its own like a single object (see
[Sealed objects](#sealed-objects)). Its associated data is the format byte followed by
`chunks/<name>` for a chunk and `trees/<name>` for a directory node, where `<name>` is the blob's
hex name. No object is stored under `chunks` or `trees`. The associated data only binds a blob to
its codec, type and name, so the bytes of one blob, read where the index lists another, don't open.

**Table.** The table has one entry per blob: its 32-byte name and its sealed length as 4 bytes
little endian. Offsets follow from the order. The table is sealed with the data key and, behind
its format byte, the associated data `datapacks/<hex SHA-256 of the blob section>`, or
`treepacks/<...>`, by the pack's namespace. It therefore opens only behind the blobs it describes.

**Names.** A pack is named by the hex SHA-256 of all its bytes. Chunk and node names are the hex
HMAC-SHA256 of their plaintext under the repository's ID key. The name of a pack isn't keyed.

**Size.** A pack closes once it holds at least 16 MiB of blobs, so it overshoots by at most one
blob. A blob of 16 MiB or more gets a pack of its own. The last pack of a run is as short as it
is.

**Limits.** Offsets and lengths in the table and the index are 32 bits. The writer refuses a
sealed blob of 4 GiB or more. The blob section of a stored pack is therefore below 32 MiB, or one
blob below 4 GiB. A pack whose table lists 4 GiB or more doesn't parse. The sealed table and its
length come on top of the blob section.

**Reads.** Chunks go into `datapacks`, directory nodes into `treepacks`. A tree pack is fetched
whole when one of its nodes is first read, and serves the nodes beside it from memory. Chunks are
read by range.

### Check packs without ark

Because a pack is named by the hash of its bytes, `sha256sum` on the storage host checks every pack
against its name, without ark and without the key:

```bash
cd /srv/ark && find datapacks treepacks -type f -exec sha256sum {} + |
  awk '{ n=split($2,p,"/"); if ($1 != p[n]) print "damaged:", $2 }'
```

An index object, a manifest, or a lock is named differently and can't be checked this way.

## Index objects

There is one index per blob type: `dataindex` for chunks and `treeindex` for directory nodes.

An index object consists of:

1. a header with the record count and the pack count
2. a table of 32-byte pack names
3. one record per blob: the 32-byte blob name, the number of its pack in the table, the offset in
   the pack, and the sealed length

Every number is 4 bytes little endian. A record is 44 bytes. Records are sorted by blob name. The
object is sealed like any single object and named by the keyed hash of its content.

### When index objects are written

A backup writes index objects for the packs it stored:

- at a checkpoint: when a pack is stored while 50 000 blobs wait for an index entry, or 10 minutes
  have passed since the last checkpoint
- at its end

Further rules:

- A type of which a run stored no pack gets no index object. A backup that stores nothing new
  writes none.
- A backup never merges index objects.
- An object is closed once it lists at least 50 000 blobs. The last one of each write is as short
  as it is.
- The blobs of one pack are never split across two objects.

Prune writes the index of a type anew when a listed pack goes or is rewritten, when an index
object doesn't open, or when there are more objects than one per 50 000 blobs listed.

### How the index is loaded

ark loads an index when the first lookup of its type needs it. A repository is opened without
reading any index object, and an incremental backup with no changed file reads only the tree
index.

- A load ends with one listing of the pack namespace, taken after the index objects were read.
- A lookup passes over what the index lists in a pack that the listing lacks. Such a blob counts
  as missing, so a backup that reads its content stores it again.
- A listing that fails fails the load.
- Otherwise, a lookup returns the pack, offset, and length that the index lists, and reads that
  range.

### Blobs in two packs

Two writers may store the same new blob, so a blob can be listed for two packs. A read takes the
copy in the stored pack with the smaller name. Prune doesn't remove the second copy on its own. It
goes when prune deletes or rewrites the pack that holds it, see [Prune](#prune).

## Lost-pack marker

The marker tells every later backup that data went missing. It exists because a backup normally
trusts its parent and reads no data index.

### When the marker is stored

A load of the data index stores the marker when it finds a listed pack gone, or a data index
object that doesn't open.

- The object is `lostpacks/<name>`. The name is the keyed hash of the string `lostpacks`, so it
  differs between repositories.
- It's sealed like any single object, over empty content. Only its presence counts. A marker that
  doesn't open counts as present.
- A marker that is already stored is neither put again nor synchronized. The writer that put it
  synchronized it.
- Backup and import fail when they can't store it. Restore, export, verify, check, prune, and an
  index rebuild log the failure and go on.
- A prune dry run stores it too, and writes nothing else.

### What a backup does with it

A backup that would take files unchanged from its parent proceeds as follows:

1. It asks for the marker with one request. A failed request counts as a marker.
2. Without a marker, it reads no data index.
3. With a marker, it loads the data index and reads again every file of which a chunk is in no
   stored pack.

The walk doesn't wait for the backup's own load of the data index. Two more rules cover what the
walk passed before the load finished:

- Once the load has finished and found the loss, the backup reads again every file that the walk
  reaches afterward and that has a chunk in no stored pack.
- A backup that took files unread and unchecked fails before it stores a snapshot when one of
  those files has a chunk in no stored pack and the backup didn't store that chunk again. The next
  backup reads them.

Unless its own load found the loss, such a backup asks for the marker a second time before it
stores its trees. When the marker is there, it loads the data index, or lists the data packs again
if it loaded the index before, and then looks those files up.

### When the marker is removed

Only prune removes the marker: under the exclusive lock, after the new index is durable, and once
every chunk that a snapshot references is listed in a stored pack.

An index rebuild never removes it. A snapshot may still reference a chunk whose records the
rebuild dropped.

## Order of writes

A backup and an import write in this order, and synchronize after each step:

1. packs
2. the index objects that list them
3. the manifest

A manifest therefore never refers to blobs without a durable index entry, and an index entry never
points at bytes that a power loss can still undo.

A long run does this in steps as it goes. An interrupted run loses at most the upload since its
last step. A pack that no index lists is left behind and removed by the next prune.

## Prune

Prune runs under the exclusive lock.

### What counts as unused

Prune counts each referenced blob as live in one pack. Of several copies, the live one is in the
stored pack with the smallest share of unreferenced bytes; of several such packs, in the one with
the smallest name. Every other blob byte is unused: blobs that no snapshot references, and the
other copies.

### What is deleted and rewritten

- Packs without a live blob are deleted.
- Packs that no index lists are deleted.
- A tree pack that holds an unreferenced node is always rewritten.
- Data packs that hold an unreferenced blob are rewritten, those with the largest unused share
  first, until their unused bytes are at most 5% of the blob bytes left after the prune.

The unused bytes that a prune reports can be more than 5%, for two reasons:

- A pack whose only unused bytes are second copies is never rewritten, and its second copies
  don't count against the 5%.
- A pack that the index lists and that is gone is left for `Check` to report and counts nowhere.

### How a rewrite works

1. Prune copies the sealed bytes of the live blobs into new packs as they are, without sealing
   them again, but it opens each one first. It does the same with the copy that stays of a blob
   whose other copy goes with its pack. A blob that doesn't open ends the prune with nothing
   deleted.
2. It writes the new index.
3. It deletes the old index objects.
4. It deletes the old packs.

Prune synchronizes between the steps. It deletes nothing when a snapshot references a blob that
no index lists.

## Check

`Check` verifies every snapshot. It reads every chunk and directory node that a snapshot
references through the index, and compares the index with the packs that are stored.

| Finding | Reported as |
|---|---|
| a referenced blob that is missing or doesn't open | error |
| a pack that the index lists and that is gone | error |
| a pack whose own table lists a referenced blob elsewhere, or not at all | error |
| a pack that no index lists | warning |
| an index object that doesn't open | warning |

The third row needs an explanation. A pack in which a referenced blob didn't open is read whole,
and its own table is compared with the index. If they disagree, the index is wrong and the pack
may be intact.

`Check` opens no other pack table. A changed table in a pack whose referenced blobs all open, or
whose blobs no snapshot references, is found by `sha256sum` on the pack, not by `Check`.

## Repairing the index

The index can be rebuilt from the packs. `Repository.RebuildIndex` does it, and `ark repair index`
runs it:

1. It reads every pack whole.
2. It checks that the pack is named by the hash of its bytes.
3. It writes a new index from the tables.
4. It deletes the old index objects.

A pack whose table doesn't open, or whose name isn't the hash of its bytes, is named in the result
and contributes nothing. For that pack, the new index keeps what the old one listed, except for
blobs that a pack which opens holds as well.

A rebuild needs the repository to itself, like prune, and it downloads every pack.

## Everything else

The master key, the key files, the config, the manifests, the locks, and the sealing of every
object are described in [Security](SECURITY.md#format).
