# 0032: A raw codec and an authenticated format byte

Status: accepted
Date: 2026-11-21

## Context

Every sealed object was `0x02 | nonce | XChaCha20-Poly1305(zstd frame)` with the associated data
`<namespace>/<name>`. There was no stored form for content that does not compress, so media,
archives and encrypted files went through zstd on the way in and on the way out for nothing. On
an 8 GiB random file zstd encode was 22% of the backup's CPU time and zstd decode 9% of the
restore's, and each blob grew by a zstd frame of about 37 bytes on top of the 41 bytes of the
seal.

The first byte of an object names its codec. A codec added after the first stable release is a
byte that every earlier build of the release line rejects, so a repository holding one blob of
it would no longer open with them. A codec that readers know from the start costs nothing later:
a writer may begin to use it in any release.

The format byte was not authenticated. With one codec that was harmless: any other byte was
refused. With two, whoever can write to the storage could change `0x02` to `0x03` and the tag
would still verify, so the reader would hand out the zstd frame as the content.

## Decision

Format byte `0x03` is a sealed object whose payload is the content itself:
`0x03 | 24-byte nonce | XChaCha20-Poly1305(content)`. Every build reads `0x02` and `0x03`, for
every sealed object: chunks, tree nodes, pack tables, index objects, manifests, locks, the
lost-pack marker and the `key` field of a key file.

The associated data of every sealed object is the format byte followed by what it was before:
`format byte | <namespace>/<name>`. An object whose format byte was changed fails on its tag.

The writer compresses and stores the content raw when the zstd frame is not smaller than the
content (`seal` in `objectstore.go`). The rule belongs to the writer, not to the format: a reader
takes either byte for any content.

- Not zstd for good, because the byte could not be added later without a new format version.
- Not a reader that knows `0x03` while no build writes it, because the size rule is a few lines,
  saves the decode and the frame bytes now, and puts the codec under every test from the start.
- Not a required feature in the repository for the codec, because a byte every build reads needs
  no announcement.
- Not a skip of the compression up front for content that looks incompressible, because it needs
  a heuristic and a measurement of what it misses. The encode cost stays for now. A later release
  may add the skip without a format change.
- Not a check of the format byte beside the tag, because binding it into the tag covers every
  codec the same way and costs one byte of associated data.

## Consequences

A blob that does not compress is its plain length plus 41 bytes, which a reader of the storage
sees where it sees the blob's size; `docs/SECURITY.md` lists it under the accepted risks. Opening
1 MiB of random content goes from about 880 to about 1 340 MB/s on `BenchmarkObjectCodec`; sealing
it is as fast as before, since the writer still compresses first. Short content such as a small
tree node or a lock is stored raw too, whenever the zstd frame header outweighs what it saves.

Prune and repack copy sealed blobs as stored and never look at the codec, so both bytes move
alike: `TestPruneCopiesRawAndZstdBlobsAsStored`. The pairing of the format byte and the tag is
pinned by `TestCodecOpensBothFormatBytes`, `TestWriterStoresABlobRawWhenZstdIsNotSmaller` and the
format-byte cell of `TestTamperedObjectsAreRejected`.

The associated data changed for every object, so a repository sealed by an earlier build does not
open. The repository format version stays 1, since version 1 is not frozen before that release,
so such a repository fails on the `key` field and Open reports `ErrWrongPassword`, not
`ErrUnsupportedFormat`. That was free before the first stable release and is fixed after it: a third codec is a new
format byte with the same binding, and builds that do not know it refuse the object.
