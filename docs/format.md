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
config/      config: plaintext JSON, authenticated through a hash sealed in the master key
             features: optional sealed JSON, the features the repository requires
keys/        key files: the master key sealed under the password
datapacks/   packs of file content chunks
treepacks/   packs of directory nodes
dataindex/   index objects: which pack holds which chunk
treeindex/   index objects: the same for directory nodes
snapshots/   one sealed manifest per snapshot, named by a random ID
locks/       sealed shared and exclusive locks
lostpacks/   at most one sealed marker, see below
```

- `config`, `features`, the key files, the manifests, the locks, and the marker are single
  objects, one per file.
- Chunks and directory nodes are never stored alone. They live in packs, and the index finds them.
- A snapshot ID is 128 random bits as 32 lowercase hex characters. It says nothing about the
  snapshot's series or time.

### Sharding

The directory and SFTP backends shard `datapacks/` and `treepacks/` one level deep by the first two
characters of the name. A pack is `datapacks/3f/3f9a...`. That makes at most 256 directories per
namespace, created when first needed. All other namespaces are flat.

Sharding is a property of those backends, not of the format. The memory backend and any other
implementation of `ark.Backend` store the same names.

## Sealed objects

A sealed object is these bytes, in this order:

| Bytes | Content |
|---|---|
| 1 | the format byte |
| 24 | the nonce, random for every object |
| rest | the XChaCha20-Poly1305 ciphertext of the payload, its 16-byte tag at the end |

- Under format byte `0x02`, the payload is one zstd frame of the content.
- Under format byte `0x03`, the payload is the content as it is. The writer picks it when the
  zstd frame wouldn't be smaller.
- The byte `0x01` is retired. No object of version 1 carries it, and no later codec is ever
  assigned to it.
- The associated data is the format byte followed by `<namespace>/<name>` in ASCII, for example
  `snapshots/<id>` for a manifest. An object moved to another name or relabelled with the other
  codec fails on its tag.
- A reader opens both format bytes, for every object. It checks the tag first and judges the byte
  after it.

Index objects, manifests, locks, the lost-pack marker, and `features` are sealed with the data key
under their own name. The `key` field of a key file is sealed under the wrap key, with the
associated data `keys/<name>`. The config isn't sealed. Blobs and pack tables have their own
associated data, see [Packs](#packs).

## JSON objects

`config`, `features`, key files, the master key, manifests, and locks are JSON objects. The
tables below name every key a version 1 build writes or reads. A key marked optional is left out
when it's empty or zero.

### Times

- A writer stores every time in these objects as an RFC 3339 string in UTC, with up to nine
  fractional digits, for example `2026-11-21T18:35:07.123456789Z`.
- A reader accepts any offset and takes the instant. It keeps no zone.

### What a reader refuses

A reader refuses what a later build may have written with `ErrUnsupportedFormat`. It never skips
such an object and never reports it as damaged.

| Input | Where |
|---|---|
| a format byte other than `0x02` or `0x03`, on an object whose tag verifies | every sealed object |
| a key the reader has no field for | every JSON object on this page |
| a `version` other than 1 | config, key file, manifest |
| a `kdf` other than `argon2id` or `none` | key file |
| with `argon2id`, a `time`, `memory`, or `threads` outside the range its row gives, or a `salt` that is empty or isn't hex | key file |
| a `chunker` size below 1, a `min` above `avg`, or an `avg` above `max` | config |
| a name in `required` | features |
| a name that isn't 32 lowercase hex characters | manifest, once its tag verifies |

- Keys match without regard to case, as Go's `encoding/json` does. A later version never adds a
  key that differs from an existing one only in case.
- An object can also fail to decode: it isn't valid JSON, a value has another type, or data
  follows its JSON value. A config, key file, master key, or features object like that is
  `ErrUnsupportedFormat` too, the sealed ones once their tag verifies. Such a manifest is
  `ErrCorruptManifest`. Such a lock is passed over, like one whose tag doesn't verify.
- An object whose tag doesn't verify is damaged, not unsupported. It gets the error of its
  namespace, for example `ErrCorruptManifest`.
- A lock that a reader refuses stops every operation that would take a lock. A lock whose tag
  doesn't verify is passed over.

### Config

`config/config` is plain JSON, not sealed, with no byte before it. Its bytes are written once, by
`Init`, with the keys in this order and no whitespace. The SHA-256 of these bytes is the master
key's `config_hash`, so a changed byte fails `Open` with `ErrCorruptConfig`.

| Key | Type | Optional | Content |
|---|---|---|---|
| `version` | number | no | the format version, 1 |
| `id` | string, 64 lowercase hex | no | the repository ID, 32 random bytes |
| `created_at` | string, time | no | when `Init` ran |
| `chunker` | object | no | the chunk boundaries, see [Chunker](#chunker) |

### Chunker

The `chunker` object of the config. Every backup into the repository cuts chunks with these sizes,
in bytes.

| Key | Type | Optional | Content |
|---|---|---|---|
| `min` | number | no | the smallest chunk, except at the end of a file |
| `avg` | number | no | the target size |
| `max` | number | no | the largest chunk |

### Key file

A key file is plain JSON, named by 16 random bytes as 32 lowercase hex characters. `Open` tries at
most 20 of them.

| Key | Type | Optional | Content |
|---|---|---|---|
| `version` | number | no | 1 |
| `kdf` | string | no | `argon2id`, or `none` for a repository without a password |
| `time` | number | yes, absent with `none` | argon2id passes, 1 to 16 |
| `memory` | number | yes, absent with `none` | argon2id memory in KiB, at least 8 per thread, at most 1 GiB |
| `threads` | number | yes, absent with `none` | argon2id lanes, at least 1 |
| `salt` | string, lowercase hex | yes, absent with `none` | 16 random bytes |
| `key` | string, lowercase hex | no | the sealed [master key](#master-key) |

The wrap key is the 32-byte argon2id key of the password and the salt. With `none` it's 32 zero
bytes.

### Master key

The JSON inside a key file's `key`. Every key file of a repository seals the same master key.

| Key | Type | Optional | Content |
|---|---|---|---|
| `data_key` | string, 64 lowercase hex | no | the key every other sealed object is sealed with |
| `id_key` | string, 64 lowercase hex | no | the HMAC key of chunk, node, index, and marker names |
| `chunker_seed` | string, 64 lowercase hex | no | the key of the chunk boundaries |
| `repository_id` | string, 64 lowercase hex | no | the config's `id` |
| `config_hash` | string, 64 lowercase hex | no | the SHA-256 of the config bytes |
| `created` | string, time | no | when the master key was made |
| `label` | string | yes | free text; no build of version 1 writes it |

A master key whose tag doesn't verify is `ErrWrongPassword`. One that opens with a `config_hash`
that is empty or isn't the hash of the config bytes fails `Open` with `ErrCorruptConfig`.

### Manifest

`snapshots/<id>` is a sealed JSON object. `<id>` is the snapshot ID.

| Key | Type | Optional | Content |
|---|---|---|---|
| `version` | number | no | 1 |
| `id` | string, 32 lowercase hex | no | the snapshot ID, the object's name |
| `parent_id` | string, 32 lowercase hex | yes | the snapshot the backup compared its files against |
| `created_at` | string, time | no | the snapshot time |
| `source_name` | string | no | the source name, see [Series](series.md) |
| `source_type` | string | no | the source type: `file`, `stream`, `docker-volume`, or the name a custom source gives |
| `labels` | object of strings | yes | the labels, see [Labels](labels.md) |
| `excludes` | array of strings | yes | the exclude patterns of the source |
| `tags` | array of strings | yes | the tags |
| `host` | string | no | the host name |
| `comment` | string | yes | the comment |
| `root_tree` | string, 64 lowercase hex | yes | the name of the root directory node; every build writes it, and a snapshot without it has no entries |
| `stats` | object | no | the counters, see [Statistics](#statistics) |

### Statistics

The `stats` object of a manifest. Sizes are in bytes. The deduplication and compression ratios
follow from the counters and aren't stored.

| Key | Type | Optional | Content |
|---|---|---|---|
| `total_files` | number | no | regular files in the snapshot |
| `total_dirs` | number | no | directories in the snapshot |
| `skipped` | number | no | entries the source offered and the snapshot leaves out |
| `files_unchanged` | number | yes | files taken from the parent without being read |
| `total_size` | number | no | the content size of the regular files |
| `stored_size` | number | no | the sealed size of the chunks this snapshot stored first |
| `new_chunks_raw_size` | number | no | the content size of those chunks |
| `chunks_total` | number | no | chunk references of all files |
| `chunks_new` | number | no | distinct chunks this snapshot stored first |
| `chunks_reused` | number | no | `chunks_total` minus `chunks_new` |
| `duration` | string | no | the run time in Go's duration syntax, for example `1m2.5s` |

### Lock

`locks/<name>` is a sealed JSON object. `<name>` is 26 characters: a millisecond timestamp and 80
random bits, both in lowercase Crockford base32.

| Key | Type | Optional | Content |
|---|---|---|---|
| `time` | string, time | no | when the lock was taken or last refreshed |
| `exclusive` | boolean | no | true for an exclusive lock, false for a shared one |
| `hostname` | string | no | the host of the holder |
| `username` | string | no | the user of the holder, empty when unknown |
| `pid` | number | no | the process ID of the holder |
| `uid` | number | no | the user ID of the holder |
| `gid` | number | no | the group ID of the holder |

A lock is stale once its `time` is 30 minutes old, or at once when its `hostname` is this host and
its process is gone.

### Features

`config/features` is a sealed JSON object. It's optional, and a repository without it requires
nothing. No build of version 1 writes it. A later release writes it when it changes what a whole
repository means, so every earlier build refuses that repository.

| Key | Type | Optional | Content |
|---|---|---|---|
| `required` | array of strings | no | the features a reader must know to open the repository |

- `Open` reads it with one `Get` after the master key is unsealed.
- A name the build doesn't know refuses the repository with `ErrUnsupportedFormat`. Version 1
  knows no name, so any name refuses.
- A tag that doesn't verify is `ErrCorruptConfig`.
- `Init` refuses a backend that holds a `features` object with `ErrAlreadyInitialized`.

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

**Table.** The table starts with the SHA-256 of the blob section, 32 bytes. One entry per blob
follows: its 32-byte name and its sealed length as 4 bytes little endian. Offsets follow from the
order. The table is sealed with the data key and, behind its format byte, the associated data
`datapacks/table` or `treepacks/table`, by the pack's namespace. That is the same for every pack
of a namespace, so the table opens from the tail of a pack alone: the last 4 bytes give its
length, the bytes before them are the table.

A reader that holds the whole pack hashes the blob section and refuses a table that carries
another hash. A reader that holds only the tail can't make that comparison. For such a reader, a
table moved in from another pack of the repository opens. It can't lead the reader to content
other than the blob a name stands for: a blob is bound to its own name and not to the table, so
each blob it lists that this pack doesn't also hold at that place fails to open.

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

A pack whose table doesn't open or carries the hash of other blobs, or whose name isn't the hash
of its bytes, is named in the result and contributes nothing. For that pack, the new index keeps
what the old one listed, except for blobs that a pack which opens holds as well.

A rebuild needs the repository to itself, like prune, and it downloads every pack. The format
doesn't require that: a table opens from the tail of its pack (see [Packs](#packs)).
