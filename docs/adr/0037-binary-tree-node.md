# 0037: The tree node is a binary object with one encoding

Status: accepted
Date: 2026-11-21

## Context

A tree node lists the entries of one directory. It is the one object of format version 1 that
every run that reads a tree decodes once per file: an incremental backup, a prune, a check, a
find, a mount. Until now it was the JSON of a Go struct, and that had four faults the freeze of
version 1 would have made permanent:

- Decoding was most of the read path. On a tree of 1 000 000 files in 20 409 nodes (AMD Ryzen 5
  2600, Go 1.27.4, one goroutine), opening and decoding every node took 5.9 s of CPU, of which
  the JSON decoder took 83%. With every other change of this record applied to the JSON (times as
  integers, no `size`, no `offset`) it was still 5.0 s and 82%.
- Names, link targets and xattr names were JSON strings. Bytes that are not valid UTF-8 became
  U+FFFD, so `caf\xe9.txt` and `caf\xe8.txt` were stored as two entries of one name and a restore
  wrote one file.
- `mtime` and `ctime` were RFC 3339 text in the time zone of the process. The name of a node is a
  keyed hash of its bytes, so the same tree backed up under another `TZ` stored every node again.
- The decoder took any JSON that fit the struct, in any key order and with any spacing. Nothing
  tied a node to the bytes its name was computed from but the writer's good behaviour.

Version 1 also had no entry for a fifo or a device node, each chunk ref carried an `offset` that
is the sum of the lengths before it, and each file a `size` that is the sum of all of them.

There is no migration between format versions, so a second decoder added after the freeze would
stay for good.

The figures for a binary node in this record come from a prototype measured against the JSON
before the layout was settled. It packed type and flags into one byte and still stored a size.
The benchmark named under Consequences measures the layout below.

## Decision

A tree node is a binary object written and read by hand in `treenode.go`, with no dependency.
Integers are little endian. A uvarint is the one of `encoding/binary`. The codec comes first,
beside the JSON node. The tree is then switched to it in one step, with no reader for JSON nodes,
before the format is frozen.

```
node  = version (1 byte, value 1), count (uvarint), entry * count
entry = name_len (uvarint), name
        type (1 byte)      1 file, 2 dir, 3 symlink, 4 hardlink, 5 fifo, 6 chardev, 7 blockdev
        flags (1 byte)     bit 0 ctime present, bit 1 xattrs present
        mode (uint16)      mode_t & 07777
        uid (uvarint), gid (uvarint)
        mtime (int64)      nanoseconds since the epoch
        ctime (int64)      only if flagged
        inode (uvarint)    0 means none
        by type:
          file               chunk_count (uvarint), { id (32 bytes), length (uvarint) } each
          dir                tree_hash (32 bytes)
          symlink, hardlink  target_len (uvarint), target
          chardev, blockdev  dev_major (uvarint), dev_minor (uvarint)
          fifo               nothing
        xattr_count (uvarint), { name_len, name, value_len, value } each, only if flagged
```

What the fields mean:

- A name is the raw bytes of one path element. Entries are sorted by those bytes and no two
  entries of a node have the same name. A link target and an xattr name are raw bytes too, and the
  xattrs of an entry are sorted by name the same way.
- `mode` is `mode_t & 07777`: the permission bits with setuid, setgid and sticky. The file type
  is the type byte and nowhere else.
- `mtime` and `ctime` are nanoseconds since the Unix epoch without a zone. A time outside the
  range of an int64 is stored as the nearest bound, so a source that knows no mtime stores the
  lowest value. A reader gets both in UTC.
- A file is its chunks in order, each the 32-byte id and the length of the content it holds. The
  size of the file is the sum of the lengths and the offset of a chunk is the sum of the lengths
  before it. Neither is stored. A reader builds both once when it decodes the node.
- A fifo, a character device and a block device are entries of their own, the devices with their
  major and minor numbers. A socket is still skipped by a backup and counted. Owner and group
  names, a birth time, a map of sparse regions and file flags are not stored and will not be.

A node has exactly one encoding. The reader accepts no other spelling, so encoding what it
decoded gives back the bytes it read, and the name of a node, the keyed hash of those bytes,
belongs to one tree and no other. It refuses, as a corrupt tree:

- a uvarint that is not in its shortest form, or that does not fit 64 bits,
- entries, or the xattrs of an entry, that are not in strictly ascending order,
- an empty name, and a name that holds `/` or NUL,
- a mode above 07777,
- a uid, a gid or a device number above 32 bits,
- a chunk of length zero, and chunk lengths that add up to more than an int64 holds,
- the xattr flag with a count of zero,
- a count the rest of the input cannot hold, which is refused before anything is allocated for
  it, and input that ends inside a field,
- bytes after the last entry.

A version byte other than 1, a type byte outside 1 to 7 and a flag bit other than the two above
are `ErrUnsupportedFormat` (0034): a later build wrote them. The writer refuses every node the
reader would refuse, and an entry with a field its type has no place for, such as a link target
on a regular file, so nothing a source filled in is dropped without a word.

The version byte is the only way the layout grows. There are no reserved bytes and no optional
fields beyond the two flags. Any change is node version 2, and a build that writes it into an
existing repository marks the repository through the features object first.

Only the tree node is binary. The manifest, the lock, the key file, the config and the features
object are read once per snapshot or once per run and stay JSON.

- Not JSON with the faults repaired one by one (integer times, escaped names, a strict decoder),
  because the decoder would still take 82% of the read path: 3 800 ns per entry against 594 ns
  for a binary node on the tree above.
- Not the protobuf wire format written by hand, which was measured too: 79.3 B per entry sealed
  against 75.2 B on this repository's own tree, 548 ns against 420 ns per entry to decode there,
  333 lines against 258. Its optional fields and its freedom of field order are what a canonical
  reader has to forbid again.
- Not a protobuf, CBOR or msgpack library, because the core module takes no dependency for what
  a few hundred lines do.
- Not times as signed varints: a current time in nanoseconds takes 9 bytes that way and 8 fixed.
- Not the type and the flags packed into one byte: seven types fill three bits and leave no
  value for an eighth.
- Not a stored `size` and `offset`. On a file of 8 GiB every one of 7 983 stored offsets equalled
  the running sum, and the only reader of the offset is the range read behind the mount. A chunk
  ref was 46 B sealed as JSON and is about 36 B now.
- Not indirect chunk lists, where a file would point at blobs that hold its refs. A file of 1 TiB
  has about a million chunks, so each backup that changes it or a file beside it stores a node of
  about 37 MB again, and a sealed blob of 4 GiB bounds a file at about 100 TiB. That is accepted
  for now. If it hurts, indirect lists come as a required feature.
- Not a reader for JSON nodes beside the new one. Version 1 has no repository to keep readable.

## Consequences

Reading a tree costs about a sixth of what it did: 1.0 s instead of 5.9 s of CPU per million
files for opening and decoding, and encoding dropped from 2.5 s to 0.2 s. `BenchmarkTreeNodeDecode`
in `treenode_internal_test.go` keeps the comparison in the repository. It decodes the same 10 000
entries from binary nodes and from the JSON nodes they replace, 4.7 ms against 39.7 ms on the
machine above, and the binary decoder has to stay at least four times faster. Chunk ids and tree
hashes stay lowercase hex strings in memory, as every caller holds them, because the decoder
meets that bound with the conversion included.

Size was not the reason and changes little. Sealed, the prototype took 75 B per entry instead
of 89 B on this repository's own tree. On a tree full of hexadecimal names it took 89 B instead
of 102 B, which is no less than JSON with integer times would have taken.

Two names that differ only in bytes that are not valid UTF-8 are two entries, and a tree backed
up under two time zones is one tree.

Once the tree is switched, a repository written before it cannot be read, and no build converts
one.

The node is no longer lenient about anything. What 0034 says about a tree node that accepts keys
it does not know is replaced by the rules above.

The reader checks a name for nothing but the rules above. `.` and `..` are names like any other
to it, and an empty link target or xattr name decodes. A sink that writes to a file system has to
judge a path itself, as before.

A uid or gid above 32 bits cannot be stored and fails the backup that meets one.

The layout is frozen with the first stable release. What holds it in place until then and after
are the tests that pin every refusal by name, the bytes of one node written out by hand, and the
check that whatever a changed bit leaves decodable encodes back to itself.

Tests: `TestTreeNodeLayout`, `TestTreeNodeRoundTrips`, `TestTreeNodeDecodeRefuses`,
`TestTreeNodeHasOneEncoding`, `TestTreeNodeEncodeRefuses`,
`TestTreeNodeTimesHaveNoZoneAndSaturate`, `TestTreeNodeDecodeSharesNoMemoryWithItsInput`.
