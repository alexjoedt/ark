# Tree node

A tree node lists the entries of one directory of a snapshot. Every other object, packs among
them, is described in [Repository format](../format.md). This page is the whole contract of the
node: a reader and a writer can be built from it alone.

## Encoding

- Every integer of fixed width is little endian. `int64` is two's complement.
- A uvarint is an unsigned integer in 7-bit groups, lowest group first. Each byte carries one
  group in its low 7 bits and sets its high bit when another byte follows. It is the encoding of
  `binary.AppendUvarint` in Go and of unsigned LEB128.
- A uvarint is always in its shortest form: a uvarint of more than one byte never ends in the
  byte `0x00`. It fits 64 bits: it has at most 10 bytes, and a tenth byte is `0x01`.
- Bytes means a uvarint length followed by that many bytes, with no terminator.
- Ids and hashes are 32 raw bytes. Elsewhere in the format they are written as 64 lowercase hex
  characters.

## Layout

A node is a version byte, an entry count and the entries:

| Field | Encoding | Content |
|---|---|---|
| version | 1 byte | `0x01` |
| count | uvarint | the number of entries |
| entries | count times an entry | sorted by name, see [Order](#order) |

An empty directory is the 2 bytes `01 00`.

### Entry

Every entry starts with the same fields:

| Field | Encoding | Content |
|---|---|---|
| name | bytes | one path element, raw bytes |
| type | 1 byte | the file type, see [Types](#types) |
| flags | 1 byte | bit 0: a ctime follows; bit 1: xattrs follow; no other bit is set |
| mode | 2 bytes, uint16 | `mode_t & 07777`: the permission bits, setuid, setgid and sticky |
| uid | uvarint | the owner's user id |
| gid | uvarint | the owner's group id |
| mtime | 8 bytes, int64 | the modification time in nanoseconds since 1970-01-01T00:00:00Z |
| ctime | 8 bytes, int64 | the status change time, the same way; only when flag bit 0 is set |
| inode | uvarint | the inode number the source reported; 0 means none |
| by type | see below | the fields of the entry's type |
| xattrs | see below | only when flag bit 1 is set |

The fields of each type follow the inode:

| Type | Fields |
|---|---|
| file | chunk count (uvarint), then per chunk its id (32 bytes) and its length (uvarint) |
| directory | the tree hash of the directory's own node (32 bytes) |
| symlink | the target (bytes) |
| hardlink | the target (bytes) |
| fifo | nothing |
| character device | the major number (uvarint), then the minor number (uvarint) |
| block device | the major number (uvarint), then the minor number (uvarint) |

The xattrs, when flag bit 1 is set, are a count (uvarint) followed by that many pairs of a name
(bytes) and a value (bytes).

### Types

| Byte | Type |
|---|---|
| `0x01` | file |
| `0x02` | directory |
| `0x03` | symlink |
| `0x04` | hardlink |
| `0x05` | fifo |
| `0x06` | character device |
| `0x07` | block device |

`0x00` is no type. A socket has no type; a backup skips it and counts it as skipped.

## Fields

- **name** is the raw bytes of one path element, with no encoding assumed. It is never empty,
  never `.` or `..`, and holds no `/` (`0x2f`) and no NUL (`0x00`). Any other byte is allowed,
  including bytes that are not valid UTF-8, so a path joined from names stays inside the snapshot.
- **mode** holds the permission bits with setuid `04000`, setgid `02000` and sticky `01000`. The
  file type is in the type byte and nowhere else.
- **uid** and **gid** are 32-bit ids. `4294967295` means no owner is known: a source without
  owners stores it.
- **mtime** and **ctime** carry no time zone. A time an int64 of nanoseconds can't hold, before
  1677-09-21 or after 2262-04-11, is stored as the nearest bound and doesn't survive. The lowest
  value, `-9223372036854775808`, as mtime means the source knew no mtime. An entry without a ctime
  leaves flag bit 0 clear.
- **inode** and **ctime** let a later backup tell an unchanged file from a changed one. A source
  that knows neither, such as a tar stream, stores neither.
- **file**: the content is the chunks in order. A chunk id is the name of a chunk blob in the data
  packs. The length is the chunk's plaintext length, at least 1. The size of the file is the sum
  of its lengths, and the offset of a chunk in the file is the sum of the lengths before it.
  Neither is stored. A file without content has a chunk count of 0.
- **directory**: the tree hash is the name of the node that lists the directory's entries.
- **symlink**: the target is the raw bytes of the link, never empty, as the source read them. A
  reader checks a target for nothing else: a sink that writes to a file system judges it itself.
- **hardlink**: the entry is a second name of another regular file of the same snapshot.
  The target is that file's path from the snapshot root, its names joined by `/`, never empty.
  The content is stored once, under the target. A target that names no regular file of the
  snapshot is `ErrCorruptTree` when a reader follows it; the node reader itself checks only that
  it isn't empty.
- **fifo** holds no content and no fields of its own.
- **character device** and **block device** carry the major and minor device numbers, each 32
  bits. They hold no content.
- **xattrs** are the extended attributes of the entry. A name is raw bytes and may be empty; a
  value is raw bytes and may be empty. An entry without xattrs leaves flag bit 1 clear and stores
  no count.

### Order

Names compare as byte strings: byte by byte as unsigned values, and a name that is a prefix of
another sorts first. The entries of a node are in strictly ascending order of their names, so no
two entries share a name. The xattrs of one entry are in strictly ascending order of their names
the same way.

## Canonical rules

A node has exactly one encoding. A reader accepts no other spelling of it, so a writer that
encodes what a reader decoded gets back the bytes it read. That keeps the name of a node, the
keyed hash of its bytes, tied to one tree.

A writer sets flag bit 0 when it stores a ctime and flag bit 1 when the entry has at least one
xattr, and sets no other bit. It writes every uvarint in its shortest form and the entries and
xattrs in order.

A reader takes the node apart front to back and returns the error of the first rule the input
breaks. It checks the order of an entry against the one before it once the entry is read, and
the order of an xattr once its value is read.

A later build wrote what this reader doesn't know. That is `ark.ErrUnsupportedFormat`:

| Input | Error |
|---|---|
| a version byte other than `0x01` | `ErrUnsupportedFormat` |
| a type byte of `0x00` or above `0x07` | `ErrUnsupportedFormat` |
| a flag bit other than bit 0 and bit 1 | `ErrUnsupportedFormat` |

Everything else that isn't the one encoding of a node is damage, `ark.ErrCorruptTree`:

| Input | Error |
|---|---|
| a uvarint that isn't in its shortest form | `ErrCorruptTree` |
| a uvarint that doesn't fit 64 bits | `ErrCorruptTree` |
| an empty input, or one that ends inside a field | `ErrCorruptTree` |
| a name, target, xattr name or value longer than the rest of the input | `ErrCorruptTree` |
| bytes after the last entry | `ErrCorruptTree` |
| an entry count above the bytes after it divided by 17 | `ErrCorruptTree` |
| a chunk count above the bytes after it divided by 33 | `ErrCorruptTree` |
| an xattr count above the bytes after it divided by 2 | `ErrCorruptTree` |
| entries not in strictly ascending order, two of one name too | `ErrCorruptTree` |
| xattrs of an entry not in strictly ascending order, two of one name too | `ErrCorruptTree` |
| an empty name | `ErrCorruptTree` |
| a name that holds `/` or NUL | `ErrCorruptTree` |
| the name `.` or `..` | `ErrCorruptTree` |
| a mode above `07777` | `ErrCorruptTree` |
| a uid or gid above `4294967295` | `ErrCorruptTree` |
| a device major or minor above `4294967295` | `ErrCorruptTree` |
| a symlink or hardlink with an empty target | `ErrCorruptTree` |
| a chunk of length 0 | `ErrCorruptTree` |
| chunk lengths of one file whose sum is above `9223372036854775807` | `ErrCorruptTree` |
| flag bit 1 with an xattr count of 0 | `ErrCorruptTree` |

17, 33 and 2 are the fewest bytes an entry, a chunk and an xattr take. The count rules refuse a
count before anything is allocated for it. A reader that skips them still refuses such a node,
once the input ends inside a field.

## Name, seal and references

- A node's name is the hex HMAC-SHA256 of the bytes this page describes, before compression and
  sealing, under the `id_key` of the [master key](../format.md#master-key). Two directories with
  the same entries are the same bytes, so they share one node.
- A node is stored as a blob in a tree pack, sealed on its own with `trees/<name>` behind the
  format byte as its associated data, and the tree index lists it. See
  [Packs](../format.md#packs).
- The `root_tree` of a [manifest](../format.md#manifest) names the node of the snapshot's root.
  Its entries are the top-level entries of the snapshot.
- The tree hash of a directory entry names the node of that directory.

## Limits

- uid, gid and device numbers have 32 bits. A backup that meets a larger uid or gid fails.
- A time outside 1677-09-21 to 2262-04-11 is stored as the nearest bound.
- A node is one blob, and the writer refuses a sealed blob of 4 GiB or more. A chunk ref takes
  about 36 bytes, so with the default average chunk of 1 MiB a file can hold about 100 TiB.
- Chunk lists are stored inline. A file of 1 TiB has about a million chunks, so a backup that
  changes such a file, or another entry in its directory, stores a node of about 37 MB again.
- A name, a target and an xattr have no limit of their own in the node.

## How the format grows

The version byte is the only way the node changes. There are no reserved bytes and no optional
fields beyond the two flags. Any change to the layout is node version 2. A build that writes it
into an existing repository first marks the repository as requiring it, in the
[features](../format.md#features) object, so an earlier build refuses the repository before it
reads a node. A reader of version 1 that meets another version byte returns
`ErrUnsupportedFormat`.

## Worked example

A directory of seven entries, one of each type. All times are 2026-11-21T18:35:07.123456789Z,
the ctime of `file` is 2026-11-21T18:35:08Z. The ids and the tree hash are placeholders of one
repeated byte; in a repository they are keyed hashes. `file` has a size of 1 048 876 bytes, and
its second chunk starts at offset 1 048 576. `dir` has one xattr, `file` has a ctime.

Each line holds the bytes of one field in hex, then what they mean:

```text
01                                                version 1
07                                                7 entries

03 64 69 72                                       name: 3 bytes, "dir"
02                                                type 2, directory
02                                                flags: bit 1, xattrs
ed 01                                             mode 0755
e8 07                                             uid 1000
e8 07                                             gid 1000
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00                                                inode 0, none
cc cc cc cc cc cc cc cc cc cc cc cc cc cc cc cc   tree hash, bytes 0 to 15
cc cc cc cc cc cc cc cc cc cc cc cc cc cc cc cc   tree hash, bytes 16 to 31
01                                                1 xattr
08 75 73 65 72 2e 74 61 67                          name: 8 bytes, "user.tag"
02 6f 6b                                            value: 2 bytes, "ok"

04 66 69 6c 65                                    name: 4 bytes, "file"
01                                                type 1, file
01                                                flags: bit 0, ctime
a4 01                                             mode 0644
e8 07                                             uid 1000
e8 07                                             gid 1000
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00 98 88 11 34 23 ea 18                           ctime 1795286108000000000 ns
92 21                                             inode 4242
02                                                2 chunks
aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa     id, bytes 0 to 15
aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa     id, bytes 16 to 31
80 80 40                                            length 1048576
bb bb bb bb bb bb bb bb bb bb bb bb bb bb bb bb     id, bytes 0 to 15
bb bb bb bb bb bb bb bb bb bb bb bb bb bb bb bb     id, bytes 16 to 31
ac 02                                               length 300

04 6c 69 6e 6b                                    name: 4 bytes, "link"
04                                                type 4, hardlink
00                                                flags: none
a4 01                                             mode 0644
e8 07                                             uid 1000
e8 07                                             gid 1000
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
92 21                                             inode 4242
04 66 69 6c 65                                    target: 4 bytes, "file"

04 6e 75 6c 6c                                    name: 4 bytes, "null"
06                                                type 6, character device
00                                                flags: none
b6 01                                             mode 0666
00                                                uid 0
00                                                gid 0
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00                                                inode 0, none
01                                                major 1
03                                                minor 3

04 70 69 70 65                                    name: 4 bytes, "pipe"
05                                                type 5, fifo
00                                                flags: none
80 01                                             mode 0600
e8 07                                             uid 1000
e8 07                                             gid 1000
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00                                                inode 0, none

03 73 64 61                                       name: 3 bytes, "sda"
07                                                type 7, block device
00                                                flags: none
b0 01                                             mode 0660
00                                                uid 0
06                                                gid 6
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00                                                inode 0, none
08                                                major 8
00                                                minor 0

03 73 79 6d                                       name: 3 bytes, "sym"
03                                                type 3, symlink
00                                                flags: none
ff 01                                             mode 0777
e8 07                                             uid 1000
e8 07                                             gid 1000
15 9b 49 dd 33 23 ea 18                           mtime 1795286107123456789 ns
00                                                inode 0, none
05 64 69 72 2f 78                                 target: 5 bytes, "dir/x"
```

The node is 289 bytes. A test in the repository reads the dump from this page, decodes it and
encodes it back to the same bytes.

## Inspect a node

`ark debug node <id> [path]` prints a stored node as JSON: the snapshot's root, or the directory
at `path`. Each entry is an object in an `entries` array, with chunk ids and tree hashes in hex,
times in nanoseconds and the mode in octal. In a snapshot whose root is the node above,
`ark debug node <id>` prints this object for `file`:

```json
{
  "name": "file",
  "type": "regular",
  "mode": "0644",
  "uid": 1000,
  "gid": 1000,
  "mtime_ns": 1795286107123456789,
  "mtime": "2026-11-21T18:35:07.123456789Z",
  "ctime_ns": 1795286108000000000,
  "inode": 4242,
  "chunks": [
    {
      "id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "length": 1048576
    },
    {
      "id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "length": 300
    }
  ]
}
```

The output is for people, not for programs, and may change in any release. See
[`debug node`](../cli/commands.md#debug-node).
