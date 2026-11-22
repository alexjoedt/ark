# Repository format

This page describes what ark stores in a repository, format version 1. The byte layout is the
contract of the version: a build reads and writes exactly this. What each object reveals to
someone who can read the storage is in [Security](SECURITY.md).

For the ideas without the bytes, see [How ark works](concepts.md).

Fixed inputs and outputs for the wrap key, sealing, names, packs, and index objects are on
[Test vectors](format/vectors.md).

## Version

The `config` object carries `version`.

- A build opens only its own version. It refuses any other with `ErrUnsupportedFormat`; the
  message names the version found and the version the build reads.
- Version 1 is the first stable format and is frozen with `v1.1.0`. Later changes are additive or
  come with a version bump.
- ark doesn't migrate an older format. Create a new repository with `ark init` and back up again.

## Errors

The errors on this page are the sentinels of ark's Go package. Another implementation needs no
names, only the classes they fall into, because each class asks a reader for something else:

| Class | Errors | What a reader does |
|---|---|---|
| unsupported | `ErrUnsupportedFormat` | stops: a later build wrote it. Never skip, repair, or delete it. |
| damaged | `ErrCorruptConfig`, `ErrCorruptManifest`, `ErrCorruptChunk`, `ErrCorruptTree`, `ErrCorruptIndex` | reports damage to that object |
| missing | `ErrMissingChunk`, `ErrMissingTree` | reports a blob the index doesn't place in a stored pack; an unindexed pack may still hold it |
| no password | `ErrPasswordRequired` | asks for a password: the repository has one |
| wrong password | `ErrWrongPassword` | asks for another password |
| needs no password | `ErrNotPasswordProtected` | opens without a password: the repository has none |
| locked | `ErrRepoLocked` | waits and tries again, or gives up: another operation holds a conflicting lock |

A tag that doesn't verify is never unsupported: only a holder of the key can write something a
later build reads. It is damage, or a wrong password for the master key.

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
- A directory node is binary, not JSON. Its layout is on a page of its own:
  [Tree node](format/tree-node.md).
- A snapshot ID is 128 random bits as 32 lowercase hex characters. It says nothing about the
  snapshot's series or time.

### Sharding

The directory and SFTP backends shard `datapacks/` and `treepacks/` one level deep by the first two
characters of the name. A pack is `datapacks/3f/3f9a...`. That makes at most 256 directories per
namespace, created when first needed. All other namespaces are flat.

Sharding is a property of those backends, not of the format. The memory backend and any other
implementation of `ark.Backend` store the same names.

The same two backends write an object to a temporary file in its directory and rename it into
place. The temporary file's name starts with `.ark-tmp-`, and a write that was cut off leaves it
behind. Their listings pass over such names, and so does a reader of their layout. No object of the
format has a name that starts with `.`.

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
- An object shorter than 41 bytes, the format byte, nonce and tag, doesn't open.

Every key is the 32 raw bytes that its 64 hex characters in the [master key](#master-key) decode
to, never the hex text: `data_key` is the XChaCha20-Poly1305 key, `id_key` the HMAC-SHA256 key.

The zstd frame follows RFC 8878:

- A writer writes exactly one frame, with no dictionary. A content checksum is optional; a reader
  verifies it when the frame carries one. A writer keeps the window at most 8 MiB, the size RFC
  8878 asks every decoder to support. ark reads windows up to 512 MiB.
- A frame whose tag verifies and that doesn't decompress, with a dictionary, a larger window, or
  bytes that aren't a frame, is `ErrUnsupportedFormat`, like an unknown format byte.
- ark also reads several frames in a row and skippable frames. No writer of version 1 writes
  them, and another reader may refuse them.
- The codec is the writer's choice. ark writes zstd at its default level and falls back to raw,
  but another writer may use any level, or raw for every object. No name depends on the codec:
  names hash the content, not the sealed bytes.

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

### Spelling

A writer writes:

- UTF-8 JSON, every key exactly as a table below spells it, in lower case, and each key once
- no `null`: a key without a value is left out when its row marks it optional, else it holds the
  zero value of its type
- every number as a decimal integer, with no fraction and no exponent
- hex in lower case

A string may escape any character. ark escapes what Go's `encoding/json` escapes by default:
besides `"`, `\` and control characters, `<`, `>`, `&`, U+2028, and U+2029 as `\u003c`, `\u003e`,
`\u0026`, `\u2028`, and `\u2029`. It writes invalid UTF-8 in a string as U+FFFD. Whitespace
between tokens is allowed.

ark reads more than that, because it decodes with Go's `encoding/json`:

- a key in another case, `Version` for `version`. A later version never adds a key that differs
  from an existing one only in case.
- a key twice: the last one counts
- `null`: the field keeps its zero value
- a string that isn't valid UTF-8: each invalid byte reads as U+FFFD
- upper case hex in `data_key`, `id_key`, and `chunker_seed` of the master key, and in `key` of a
  key file. A `salt` in upper case is refused, see [What a reader refuses](#what-a-reader-refuses).

No writer of version 1 writes any of these. Another reader may refuse them with
`ErrUnsupportedFormat`.

### What a reader refuses

A reader refuses what a later build may have written with `ErrUnsupportedFormat`. It never skips
such an object and never reports it as damaged.

| Input | Where |
|---|---|
| a format byte other than `0x02` or `0x03`, on an object whose tag verifies | every sealed object |
| a key the reader has no field for | every JSON object on this page |
| a `version` other than 1 | config, key file, manifest |
| a `kdf` other than `argon2id` or `none` | key file |
| with `argon2id`, a `time`, `memory`, or `threads` outside the range its row gives, or a `salt` that isn't 16 bytes in lowercase hex | key file |
| a `chunker` size below 1, a `min` above `avg`, an `avg` above `max`, an `avg` that isn't a power of two, or a `max` above 67108864 (64 MiB) | config |
| a name in `required` | features |
| a name that isn't 32 lowercase hex characters | manifest, once its tag verifies |

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
`Init`. The SHA-256 of these bytes as stored is the master key's `config_hash`, so a changed byte
fails `Open` with `ErrCorruptConfig`. A reader hashes the bytes, not a canonical form, so the
spelling is the writer's choice; ark writes the keys in this order with no whitespace.

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
| `min` | number | no | no chunk but the last of a file is shorter than this |
| `avg` | number | no | the target size, a power of two |
| `max` | number | no | the largest chunk, at most 67108864 (64 MiB) |

`ark init` writes `min` 262144 (256 KiB), `avg` 1048576 (1 MiB), and `max` 4194304 (4 MiB),
unless `--chunk-min`, `--chunk-avg`, or `--chunk-max` is given, see [init](cli/commands.md#init).
The bound on `max` lets every reader hold a buffer of `max` bytes, as ark's chunker does.

### Chunker algorithm

ark cuts chunks with FastCDC and a gear hash. A writer that cuts the same way, with the same
sizes and `chunker_seed`, gets the same chunks as ark, and with the same `id_key` the same chunk
names. A writer that
cuts differently still writes a valid repository; its chunks just don't deduplicate against
ark's.

**Gear table.** The seed is the 32 bytes the master key's `chunker_seed` decodes to. The table is
256 unsigned 64-bit numbers: the first 2048 bytes of the ChaCha20 keystream (RFC 8439, 20 rounds)
with the seed as key, a zero nonce of 12 bytes, and block counter 0, where `gear[i]` is bytes `8i`
to `8i+7` read little endian. With a zero nonce, the original ChaCha20 with its 8-byte nonce and
64-bit counter gives the same keystream.

**Masks.** With `b` the base 2 logarithm of `avg`, rounded down. A config's `avg` is a power of
two, so `avg = 2^b`; only a test vector below takes another `avg`:

- `maskS = 2^(b+1) - 1`, the low `b+1` bits
- `maskL = 2^(b-1) - 1`, the low `b-1` bits, and 0 when `avg` is 1
- `normal = min + (avg - min) / 2`, with integer division

**Cut rule.** Every file is cut on its own, from its first byte. An empty file has no chunks. The
first chunk of the rest of a file, `data` of `n` bytes, is `cut(data)` bytes long; the cut repeats
on what follows it until nothing is left. All arithmetic on `fp` is unsigned 64-bit and wraps.

```text
cut(data):
  if n <= min: return n
  end = smaller of n and max
  fp = 0
  for i = min; i < end; i++:
    fp = (fp << 1) + gear[data[i]]
    mask = maskS if i < normal else maskL
    if (fp & mask) == 0: return i + 1
  return end
```

- The hash starts at 0 at offset `min` of each chunk. The bytes before it don't enter the hash.
- Up to `normal` the wider `maskS` makes a cut rarer, after it the narrower `maskL` makes one more
  likely. That keeps chunk sizes close to `avg`.
- A file's rest of at most `min` bytes is its last chunk. A rest without a cut point up to `max`
  is cut at `max`, or ends the file when it is shorter.

#### Chunker test vectors

Each vector gives a seed and the sizes, an input, and the chunk lengths in order. An input
`chacha20 <key> <n>` is the first `n` bytes of the keystream of the gear table rule with `<key>`
as key. An input `zero <n>` is `n` zero bytes. Seeds and keys are 32 bytes in hex.

```text
seed    000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
min     4096
avg     16384
max     65536
input   chacha20 202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f 262144
lengths 6207 5867 27212 17014 7171 39978 9910 10137 15598 21079 5907 11909 13167 14185 18610
        13740 12098 12355
```

`avg` 3000 takes the masks of 2048, and `normal` is 2000:

```text
seed    404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f
min     1000
avg     3000
max     9000
input   chacha20 0000000000000000000000000000000000000000000000000000000000000000 65536
lengths 2479 2252 1895 1526 2153 2827 6888 1023 2147 2634 3094 2602 2277 3570 2841 2298 2651
        3414 3340 2288 3663 1402 3467 2805
```

Small sizes put many cuts near `min` and `normal`, and this input ends in a chunk of 1 byte:

```text
seed    6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80
min     64
avg     256
max     1024
input   chacha20 8485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3 8192
lengths 190 663 189 999 211 338 483 253 87 401 254 68 202 173 485 305 94 244 198 218 224 893
        201 218 167 161 272 1
```

Input without a cut point is cut at `max`:

```text
seed    000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
min     4096
avg     16384
max     65536
input   zero 200000
lengths 65536 65536 65536 3392
```

### Key file

A key file is plain JSON, named by 16 random bytes as 32 lowercase hex characters. `Open` tries at
most 20 of them.

| Key | Type | Optional | Content |
|---|---|---|---|
| `version` | number | no | 1 |
| `kdf` | string | no | `argon2id`, or `none` for a repository without a password |
| `time` | number | yes, absent with `none` | argon2id passes, 1 to 16 |
| `memory` | number | yes, absent with `none` | argon2id memory in KiB, at least 8 per thread, at most 1 GiB |
| `threads` | number | yes, absent with `none` | argon2id lanes, 1 to 255 |
| `salt` | string, lowercase hex | yes, absent with `none` | 16 random bytes; a reader refuses any other length |
| `key` | string, lowercase hex | no | the sealed [master key](#master-key) |

The wrap key is argon2id (RFC 9106) of version `0x13`, with:

- the password as its bytes, unchanged: no Unicode normalisation and no trimming in the format.
  The CLI drops one trailing `\n` or `\r\n` of a password file or of the output of
  `--password-command`, see [Password](cli/index.md#password).
- the salt as the bytes its hex decodes to
- `time` as the passes, `memory` in KiB, `threads` as the parallelism
- no secret and no associated data
- a tag of 32 bytes

With `none` the wrap key is 32 zero bytes. ark writes `time` 3, `memory` 65536, and `threads` 4.

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

- A reader takes the snapshot ID from the object's name, which the associated data binds. ark
  doesn't compare `id` with it and reports the name; a writer writes the same ID in both.
- ark checks `parent_id` and `root_tree` for nothing when it opens a manifest. A `root_tree` that
  isn't 64 lowercase hex fails the first read of the snapshot's entries, in ark with an error of
  none of the classes above. Another reader may report it as a damaged manifest.

### Statistics

The `stats` object of a manifest. Sizes are in bytes. The deduplication and compression ratios
follow from the counters and aren't stored.

| Key | Type | Optional | Content |
|---|---|---|---|
| `total_files` | number | no | regular files in the snapshot, every further name of a hardlinked file included |
| `total_dirs` | number | no | directories in the snapshot, the root not counted |
| `skipped` | number | no | entries the source offered and the snapshot leaves out |
| `files_unchanged` | number | yes | files taken from the parent without being read |
| `total_size` | number | no | the content size of the regular files, a file under several names counted once |
| `stored_size` | number | no | the sealed size of the chunks this snapshot stored first |
| `new_chunks_raw_size` | number | no | the content size of those chunks |
| `chunks_total` | number | no | chunk references of all files |
| `chunks_new` | number | no | distinct chunks this snapshot stored first |
| `chunks_reused` | number | no | `chunks_total` minus `chunks_new` |
| `duration` | string | no | the run time in Go's duration syntax, for example `1m2.5s` |

The `duration` is what Go's `time.Duration.String` writes: `0s` for zero; below one second a
decimal number with the unit `ns`, `µs` (U+00B5), or `ms`, as `1.5ms`; from one second on hours,
minutes, and seconds with a decimal fraction, leading zero units left out, as `2.5s`, `1m2.5s`,
or `1h0m0s`. A duration that doesn't parse reads as 0 and fails nothing.

### Lock

`locks/<name>` is a sealed JSON object. `<name>` is 26 characters of the lowercase Crockford
base32 alphabet `0123456789abcdefghjkmnpqrstvwxyz`:

- 10 characters: the Unix time in milliseconds when the lock was taken, most significant
  digit first, padded with `0`
- 16 characters: 10 random bytes, 5 bits per character, most significant bit first

A writer draws the random bytes from a cryptographic source for every new lock. Names compare
as byte strings: the earlier name holds the earlier millisecond, and two locks taken in the same
millisecond order by their random part. A lock keeps its name when it's refreshed, so the name
says when it was taken and `time` when it was last written.

| Key | Type | Optional | Content |
|---|---|---|---|
| `time` | string, time | no | when the lock was taken or last refreshed |
| `exclusive` | boolean | no | true for an exclusive lock, false for a shared one |
| `hostname` | string | no | the host of the holder |
| `username` | string | no | the user of the holder, empty when unknown |
| `pid` | number | no | the process ID of the holder |
| `uid` | number | no | the user ID of the holder |
| `gid` | number | no | the group ID of the holder |

When a lock is stale, and how locks are taken, refreshed, and released, is in
[Lock protocol](#lock-protocol).

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

**Table.** The table starts with the SHA-256 of the blob section, unkeyed, 32 bytes. One entry per
blob follows: its 32-byte name and its sealed length as 4 bytes little endian. Offsets follow from
the order. The table is sealed with the data key and, behind its format byte, the associated data
`datapacks/table` or `treepacks/table`, by the pack's namespace. That is the same for every pack
of a namespace, so the table opens from the tail of a pack alone: the last 4 bytes give its
length, the bytes before them are the table.

A reader that holds the whole pack hashes the blob section and refuses a table that carries
another hash. A reader that holds only the tail can't make that comparison. For such a reader, a
table moved in from another pack of the repository opens. It can't lead the reader to content
other than the blob a name stands for: a blob is bound to its own name and not to the table, so
each blob it lists that this pack doesn't also hold at that place fails to open.

**Names.** A pack is named by the hex SHA-256 of all its bytes. Chunk and node names are the hex
HMAC-SHA256 under the `id_key` of their plaintext: the content of a chunk, the bytes of a node,
before compression and sealing. The name of a pack isn't keyed.

**Size.** A pack closes once it holds at least 16 MiB of blobs, so it overshoots by at most one
blob. A blob of 16 MiB or more gets a pack of its own. The last pack of a run is as short as it
is. The size is the writer's choice: a reader takes a pack of any size within the limits below.

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

The content of an index object, before it is sealed, is these parts in this order:

| Bytes | Content |
|---|---|
| 4 | the record count `r`, uint32 little endian |
| 4 | the pack count `p`, uint32 little endian |
| 32 × `p` | the pack names, the 32 bytes each hex name spells |
| 44 × `r` | the records |

A record is these 44 bytes:

| Bytes | Content |
|---|---|
| 32 | the blob name, the 32 bytes its hex spells |
| 4 | the number of its pack, from 0, in the pack names above, uint32 little endian |
| 4 | the offset of the sealed blob in the pack, uint32 little endian |
| 4 | the sealed length of the blob, uint32 little endian |

- A writer lists each pack once, in ascending byte order of the names. It sorts the records by
  blob name, then pack number, then offset, each in ascending order, and lists none twice. A
  reader relies on neither order.
- The content is sealed like any single object, with the data key and the associated data
  `dataindex/<name>` or `treeindex/<name>`.
- The name is the hex HMAC-SHA256 under the `id_key` of the content, the bytes in the tables
  above before compression and sealing, as for a chunk.
- Content that isn't exactly `8 + 32 × p + 44 × r` bytes long, or a record whose pack number is
  `p` or more, makes the object one that doesn't open, like a tag that doesn't verify. An object
  under a format byte the reader doesn't know fails the load with `ErrUnsupportedFormat`.

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
  range. A range that runs past the end of the pack is damage to the blob: `ErrCorruptChunk` for a
  data pack, `ErrCorruptTree` for a tree pack.

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

- The object is `lostpacks/<name>`. The name is the hex HMAC-SHA256 under the `id_key` of the 9
  ASCII bytes `lostpacks`, so it differs between repositories.
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

## Lock protocol

Operations that run at the same time coordinate through the objects in `locks/` (see
[Lock](#lock)). A writer of another implementation follows this protocol too, or a concurrent
prune deletes its packs. What a user sees of it, exit code 3 and `--lock-wait`, is in
[Concepts](concepts.md#locks).

### Timings

Durations are spelled as in [Config](#config).

| Name | Value | Meaning |
|---|---|---|
| settle delay | `200ms` | the pause between writing the own lock and listing again |
| refresh interval | `5m0s` | how often a holder writes its lock again |
| self-timeout | `22m30s` | how long a holder's lock may go without a write before the holder aborts |
| self-timeout check | `1s` | how often a holder compares its lock's age with the self-timeout |
| stale age | `30m0s` | the age of `time` from which any lock is stale |
| first backoff | `500ms` | the wait after the first rejected attempt, before jitter |
| longest backoff | `8s` | the bound of the doubled wait, before jitter |
| release timeout | `1m0s` | how long deleting the own lock may take |

### Which lock an operation takes

| Operation | Lock |
|---|---|
| backup, import, docker backup, profile backup | shared |
| restore, export, docker restore | shared |
| check, verify | shared |
| ls, find | shared, one per snapshot walked |
| forget with `--dry-run` | shared |
| mount | shared, until it's unmounted |
| debug node | shared |
| forget | exclusive |
| prune | exclusive |
| repair index | exclusive |
| key passwd | exclusive |
| info with `--exact` | exclusive |

`init`, `snapshots`, `inspect`, `unlock`, `info` without `--exact`, `docker volumes`,
`docker host`, `profile list`, `profile show`, and `version` take no lock. `forget --prune` takes
the exclusive lock for the forget, releases it, and takes it again for the prune.

Prune needs the exclusive lock because it deletes every pack that no index lists and every blob
that no snapshot references. A backup that runs has both: packs it has stored before the index
objects that list them (see [Order of writes](#order-of-writes)), and blobs, new ones and stored
ones it deduplicates against, that only its manifest references, and the manifest is written
last. Beside a prune, the backup would store a manifest that points at deleted blobs. Repair index
deletes the old index objects, including one a concurrent writer has just stored. Info with
`--exact` runs a prune dry run, which may store the lost-pack marker. Key passwd replaces the key
file. Forget needs it because it decides from the manifests it lists: two forgets with different
policies that list the same manifests can each remove what the other keeps, and leave a series
empty between them. A forget that only reports, `--dry-run`, deletes nothing and takes the shared
lock. What a forget leaves unreferenced is deleted by the next prune.

### Conflicts

- A shared lock conflicts with every exclusive lock that isn't stale.
- An exclusive lock conflicts with every lock that isn't stale.
- A lock never conflicts with itself, and a stale lock conflicts with nothing. Taking a lock
  never deletes a stale one; only `ark unlock` does.

### Taking a lock

1. List `locks/` and read every object, as [Reading locks](#reading-locks) says. Stop if one is
   refused. Back off if one conflicts with the class you want; nothing is written.
2. Name a new lock from the current time and write it, with `time` the same instant in UTC.
   A write that fails may still have stored the object: delete the name, then stop.
3. Wait the settle delay.
4. List and read again. Delete your lock and stop if one is refused. Delete your lock and back
   off if another one conflicts.
5. Delete your lock and stop if the self-timeout has passed since the `time` it carries: others
   may already count it as stale.
6. You hold the lock. Refresh it until the operation ends, then release it.

Read what the operation decides from, the index above all, only once you hold the lock. An index
loaded before, also by an earlier operation of the same process, may list blobs that a prune has
deleted since, and a backup that deduplicates against them stores a manifest that points at
nothing.

Nothing breaks a tie. Two claimants that both pass step 1 and see each other in step 4 both back
off, whatever their classes; no name, time, or host wins. The second listing is what makes the
protocol safe: of two claimants, the one whose write landed later lists after the other's write,
so it sees the other's lock and backs off. No two conflicting locks are ever held at once, on a
backend that keeps what [What a backend must guarantee](#what-a-backend-must-guarantee) asks. Two
claimants can back off again on the next attempt, and the jitter spreads them apart.

### Backing off

Without a wait, the operation fails with `ErrRepoLocked` and names a conflicting lock. With a
wait, it tries again until the wait, counted from the first attempt, is used up. After attempt
`n`, counted from 0, it sleeps `d` plus a random duration below `d`, where `d` is the first
backoff times `2^n` or the longest backoff, whichever is smaller. It never sleeps past the end of
the wait. A rejection with no wait left fails with `ErrRepoLocked`.

### Refreshing

Every refresh interval, the holder writes its lock again under the same name, with `time` the
current instant in UTC and the other keys unchanged. A refresh that fails is tried again at the
next interval.

Every self-timeout check, the holder takes the age of its lock: the larger of the wall clock
time since the `time` last written, and the monotonic time since that write began. A write
counts once it has returned without an error; while a refresh hangs or after it failed, the age
runs on from the write before. The wall
clock counts the time a suspended host slept; the monotonic clock counts a write that hangs.
Once the age reaches the self-timeout, the holder aborts its operation and never writes the lock
again. It keeps checking while a refresh hangs, so a hung write can't keep the operation alive.

The self-timeout lies 7m30s below the stale age, so a holder stops writing before any other host
counts its lock as stale. The clocks of the hosts that share a repository must agree within that
margin.

### Stale locks

A lock is stale when its `time` is the stale age old or older, by the reader's clock. Nothing
else makes a lock stale, also not on the holder's own host.

`hostname`, `username`, and `pid` name the holder for `ark unlock` and error messages. A reader
never asks whether that process still runs: a host name and a process ID don't identify a
process across PID namespaces, so a process of the same ID in another container may be the
running holder. A lock that a crashed operation left behind blocks conflicting operations until
it's stale or `ark unlock --force` removes it.

### Releasing

The holder deletes its lock object when the operation ends, also when it failed or was
cancelled. It stops refreshing first, since a refresh that returns after the delete stores the
lock again. A refresh in flight is waited for only until the self-timeout; should it return after
the delete, the holder deletes the lock again. ark gives the delete the release timeout. A lock whose delete fails stays until it's
stale, and blocks conflicting operations until then.

### Reading locks

Steps 1 and 4 read every object that the listing names. ark warns only in step 1, so it doesn't
warn about one object twice:

| Object | What a reader does |
|---|---|
| zero bytes long | ignores it: another writer's write in flight |
| gone between listing and reading | ignores it: released since the listing |
| a tag that doesn't verify, or JSON that doesn't decode | passes over it and warns |
| a read that fails, for any other reason | stops with the read's error, also under a lock wait: the lock may be one that conflicts. In step 4, it deletes its own lock first |
| refused (see [What a reader refuses](#what-a-reader-refuses)) | stops with `ErrUnsupportedFormat`: a later build holds it, and what it guards is unknown here |

`ark unlock --force` removes every lock, the passed-over and refused ones included, and a
zero-byte object whose name is the stale age old or isn't a lock name. It also removes locks that
running operations hold, so no operation of any build may run. `ark unlock` alone removes only
stale locks.

### What a backend must guarantee

Once a write returns, a listing that starts after it, on any host, names the object: a listing
reads its own and others' writes. A backend that needs a moment for that is safe as long as the
settle delay covers it. ark's local, SFTP, and in-memory backends list a finished write at once.

A write to a name that already exists, as a refresh is, replaces the object in one step: a
listing and a read see the old object or the new one, never a missing, empty, or partial one.
Otherwise a claimant can list `locks/` while a held lock is missing and take a conflicting one.
ark's local and in-memory backends replace in one step, and so does its SFTP backend on a server
that offers the `posix-rename@openssh.com` extension. On a server without it, the SFTP backend
deletes the old object before it renames the new one into place, so a lock is missing for a round
trip at every refresh. Don't run a prune, repair index, or key passwd there while another
operation runs.

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
