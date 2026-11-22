# Test vectors

Fixed inputs and the outputs they give, one building block of the
[Repository format](../format.md) at a time. An implementation checks each step here before a
whole repository opens: a wrong wrap key, seal, name, pack, or index shows in its own vector. The
chunker's vectors are on the format page, see [Chunker test vectors](../format.md#chunker-test-vectors).

How to read the blocks:

- Each line holds a name, then its value. A value too long for one line goes on over the indented
  lines below it; join them without the spaces.
- Values are bytes in hex, except `aad`, which is ASCII text, and `time`, `memory`, and
  `threads`, which are decimal numbers.
- Every block uses the two keys of [Keys](#keys). All its other inputs are in the block itself.
- A sealed object is the format byte, the nonce, then the ciphertext with its 16-byte tag. The
  tag covers the format byte followed by `aad`, see [Sealed objects](../format.md#sealed-objects).
- The nonces are fixed, so the outputs are too. A writer draws a random nonce for every object.

ark's tests read this page and recompute every vector with ark's own code, so the page and ark
can't drift apart.

## Keys

Test keys, the bytes counting up. In a repository they are the `data_key` and `id_key` of the
[master key](../format.md#master-key).

```text
data_key    000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
id_key      202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f
```

## Wrap key

The argon2id wrap key of the password `correct horse battery staple`, version `0x13`, with a tag
of 32 bytes, see [Key file](../format.md#key-file).

These are test parameters, small so that the vector takes no time. They aren't the defaults: ark
writes `time` 3, `memory` 65536, and `threads` 4. With `threads` 1, an argon2id that has no
parallelism parameter, like libsodium's, reproduces the vector.

```text
password    636f727265637420686f727365206261747465727920737461706c65
salt        000102030405060708090a0b0c0d0e0f
time        1
memory      64
threads     1
wrap_key    92dc5d67019623868bde079275e522f4b7e8213d3414ed85cbc2ac8a41117288
```

## Sealed objects

Both objects are sealed with `data_key`, under the associated data of a manifest.

### Raw

Format byte `0x03`: the payload is the content, `format 1` in ASCII.

```text
format      03
nonce       010101010101010101010101010101010101010101010101
aad         snapshots/000102030405060708090a0b0c0d0e0f
content     666f726d61742031
sealed      03010101010101010101010101010101010101010101010101cf8e65651d6299
            737f23d3e931ec62e0cc6c46846b6dfb05
```

### Zstd

Format byte `0x02`: the payload is a zstd frame of the content, `ark ` 32 times in ASCII. The
frame is the one ark writes for it, with a content checksum. Another encoder may write another
frame, and that frame is valid too: a writer checks its seal of this payload, a reader checks that
the payload decompresses to the content.

```text
format      02
nonce       020202020202020202020202020202020202020202020202
aad         snapshots/000102030405060708090a0b0c0d0e0f
content     61726b2061726b2061726b2061726b2061726b2061726b2061726b2061726b20
            61726b2061726b2061726b2061726b2061726b2061726b2061726b2061726b20
            61726b2061726b2061726b2061726b2061726b2061726b2061726b2061726b20
            61726b2061726b2061726b2061726b2061726b2061726b2061726b2061726b20
payload     28b52ffd04005d00002061726b20015404022af99483990c
sealed      020202020202020202020202020202020202020202020202028e541dd9ea6444
            15f99ec2de9c13dce8b36d463176976e6c4b7f4c1d6ebc0004e339363f44ee34
            98
```

## Names

Each name is the hex HMAC-SHA256 under `id_key` of its input:

- `chunk` is a chunk with the content `hello, ` in ASCII.
- `node` is the node of an empty directory: version 1, no entries, see [Tree node](tree-node.md).
- `marker` is the 9 ASCII bytes `lostpacks`. The marker is stored as `lostpacks/<marker_name>`,
  see [Lost-pack marker](../format.md#lost-pack-marker).

```text
chunk       68656c6c6f2c20
chunk_name  93e757ac0c558611d3823f11191033d5a056f8af2864d5477035b3e7bea2ed94
node        0100
node_name   807d5b32722d2bf0c05ee79689f762e0be57832810d3037a62f1c6029afce554
marker      6c6f73747061636b73
marker_name be2a34d08912eb6792437b3bf28160a937d6058df84dc4f4e9049309fc232361
```

## Pack

A data pack of two chunks, `hello, ` and `world` with a newline, each sealed raw, see
[Packs](../format.md#packs).

### First blob

```text
content     68656c6c6f2c20
name        93e757ac0c558611d3823f11191033d5a056f8af2864d5477035b3e7bea2ed94
nonce       111111111111111111111111111111111111111111111111
aad         chunks/93e757ac0c558611d3823f11191033d5a056f8af2864d5477035b3e7bea2ed94
sealed      03111111111111111111111111111111111111111111111111ffb25b11f25a52
            1b57bd5610cc05fdb59641656df1e4c5
```

### Second blob

```text
content     776f726c640a
name        2e6a3f33340d316ac8e076b52fdb4662f6f48957179b290a386d4f07160e02ef
nonce       121212121212121212121212121212121212121212121212
aad         chunks/2e6a3f33340d316ac8e076b52fdb4662f6f48957179b290a386d4f07160e02ef
sealed      0312121212121212121212121212121212121212121212121260ab7d2f1b2f31
            053541de206b0776513b24720c66a8
```

### Table

The content is the SHA-256 of the blob section, the two sealed blobs in order, then per blob its
name and its sealed length, 48 and 47 bytes, as 4 bytes little endian. It is sealed raw, under the
associated data of every table of `datapacks`.

```text
content     e868db3ec640289704a11376f46169d372b3627d9b624859d657262ecae28c33
            93e757ac0c558611d3823f11191033d5a056f8af2864d5477035b3e7bea2ed94
            300000002e6a3f33340d316ac8e076b52fdb4662f6f48957179b290a386d4f07
            160e02ef2f000000
nonce       131313131313131313131313131313131313131313131313
aad         datapacks/table
sealed      03131313131313131313131313131313131313131313131313f2845de443259f
            39cde0ff80965824110dd71f1ce700fc42e08f151b45612b825460af156b47e2
            e9097bc614e93bdd63d5c0f3eac6edc4069cda7a9392dbb52996cc026eff50ca
            3f71bc14d29e4faf4ef805262db00cc8ba541eb47ae2177a5279153c2d24e42a
            41695b525d1df6b8479882064eada2ddc1
```

### Whole pack

The two sealed blobs, the sealed table, and the table's length of 145 bytes as 4 bytes little
endian. The name is the hex SHA-256 of these bytes. The pack is stored as `datapacks/<name>`.

```text
pack        03111111111111111111111111111111111111111111111111ffb25b11f25a52
            1b57bd5610cc05fdb59641656df1e4c503121212121212121212121212121212
            12121212121212121260ab7d2f1b2f31053541de206b0776513b24720c66a803
            131313131313131313131313131313131313131313131313f2845de443259f39
            cde0ff80965824110dd71f1ce700fc42e08f151b45612b825460af156b47e2e9
            097bc614e93bdd63d5c0f3eac6edc4069cda7a9392dbb52996cc026eff50ca3f
            71bc14d29e4faf4ef805262db00cc8ba541eb47ae2177a5279153c2d24e42a41
            695b525d1df6b8479882064eada2ddc191000000
name        fd1f2642d85e0de7aad34276ebe3735977fa6fc68c14cc1311daf71e04dd7ac5
```

## Index object

A data index object that lists the pack above, see [Index objects](../format.md#index-objects):
2 records, 1 pack, the pack's name, then the records by blob name. The second blob sorts first:
its name starts with `2e`. Its record gives pack 0, offset 48, and length 47; the first blob's
gives pack 0, offset 0, and length 48. The object is sealed raw and stored as `dataindex/<name>`.

```text
content     0200000001000000fd1f2642d85e0de7aad34276ebe3735977fa6fc68c14cc13
            11daf71e04dd7ac52e6a3f33340d316ac8e076b52fdb4662f6f48957179b290a
            386d4f07160e02ef00000000300000002f00000093e757ac0c558611d3823f11
            191033d5a056f8af2864d5477035b3e7bea2ed94000000000000000030000000
name        af1f9e4028de2f7717a65081b2ecc368aa35fbbf5b349b4d61f5fb515a622b1c
nonce       141414141414141414141414141414141414141414141414
aad         dataindex/af1f9e4028de2f7717a65081b2ecc368aa35fbbf5b349b4d61f5fb515a622b1c
sealed      031414141414141414141414141414141414141414141414146660fc30e555e2
            abfa7c2819a41d77e2a6b38b03c2777a8a325549e72c98e155f5cd0fbb34598c
            73956461e7df1ea4d057ee38d3a17856ad5c7f2d1ec52239f2e045fcbe7ef143
            8c30b3b6d29d6dd4549aa1de1f81a97d15aa7ada1e5f62a2c42bdbc4cbf26f07
            099d9536a83a65cdb08391168a55cd2905ee6883f849eaaacd98539dbb37718e
            d8a9826935384711bc
```

## Fixture repository

`testdata/repo-v1` in the source tree is a whole repository of format version 1, in the layout of
the directory backend. It holds two snapshots of one series with an entry of every node type,
names and an xattr name that aren't valid UTF-8, a hardlink, setuid, setgid and sticky modes, and
an mtime before 1970. Its chunker sizes are 4 KiB, 8 KiB, and 16 KiB, so a file spans several
chunks, and its key file uses argon2id with the defaults.

The password is `ark fixture, format version 1`. It guards nothing.

The fixture is written once and never again: a reader that restores both snapshots as ark does
reads what every version 1 build wrote. `ark restore` and `ark debug node` show what it holds.
