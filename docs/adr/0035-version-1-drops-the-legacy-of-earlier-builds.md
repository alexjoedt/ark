# 0035: Version 1 drops the legacy of earlier builds

Status: accepted
Date: 2026-11-21

## Context

Repository format version 1 is frozen with the first stable release, and ark has no migration.
Whatever version 1 carries on that day it carries for good. It still carried what builds from
before the freeze wrote or expected, although nobody has a repository from them:

- The manifest kept its labels under the key `source_meta`, the name of an API field that 0022
  renamed to `Labels`.
- A repository could hold snapshots under 26 character base32 names. Such a name begins with the
  millisecond its snapshot was taken, so a listing of the storage dates the snapshot.
  `docs/SECURITY.md` listed that as an accepted risk.
- The config had a `compression` field with one legal value, `zstd`. The format byte of each
  sealed object already names its codec (0032).
- The statistics of a manifest stored the deduplication and compression ratios as floats, next
  to the counters they are computed from.
- A master key without `config_hash` was reported as an older format instead of as a config
  that does not match (0006).
- Import read a second archive layout, the metadata of a binary that predates the manifest.
- The format bytes are `0x02` and `0x03`. There is no `0x01`.

Each item is a few lines of code. Each is also a promise once the format is frozen: a second
key, a second name form or a second archive layout that every later reader has to keep.

## Decision

Version 1 drops all of it except the numbering of the format byte. Breaking the format is free
until the freeze, so nothing is migrated and nothing is read for compatibility.

- The manifest key is `labels`.
- A snapshot ID is 128 random bits as 32 lowercase hex characters, and a repository holds no
  other form. A manifest that opens under another name is `ErrUnsupportedFormat`. The name is
  judged after the tag, like an unknown format byte (0034): an object that does not authenticate
  is corrupt whatever it is called and is still removed by a forget of its name, so nobody
  without the key can make a repository read as unsupported. A forget of the name removes an
  object under a name that is not hex without reading it, so there is a way out of a
  repository that a listing refuses.
- An archive is outside input, so import still takes an ID of any form. It keeps a hex ID and
  names the copy of any other by the first 128 bits of the SHA-256 of that ID
  (`importedSnapshotID` in `ulid.go`).
- The config is `version`, `id`, `created_at` and `chunker`. A config with a `compression` key
  is refused like any config with a key the build does not know.
- A manifest stores the counters and the duration. The two ratios are computed when it is
  loaded (`snapshotStats` in `manifest.go`). `SnapshotStats` keeps both fields, and the JSON
  output of the CLI keeps both keys.
- A master key without `config_hash` does not match the config. Open fails with
  `ErrCorruptConfig`, as for any other hash that differs.
- Import reads one layout: `metadata.json` holding a manifest of version 1, decoded strictly.
  It still takes a gzip stream, because that is how a caller compresses an export.
- The format byte keeps `0x02` and `0x03`. `0x01` is retired and never assigned.
- The repository format version stays 1.

Rejected:

- Not UUIDs for snapshot IDs, because the hyphens get in the way of prefix matching and a
  version 7 UUID carries its creation time, which is what the hex form was chosen to hide.
- Not renumbering the format byte to start at `0x01`, because the number means nothing beyond
  being distinct, and a byte that stays unassigned can never be read under a second meaning.
- Not keeping `compression` as a place to name a later codec, because the config cannot change
  after Init, and a codec is a per-object property that the format byte already carries.
- Not keeping the stored ratios for readers other than ark, because a value that follows from
  its neighbors can disagree with them, and then a reader has to pick one.
- Not a format version bump, because nobody has a repository that a new number would tell
  apart, and 1 is the version the first stable release freezes.

## Consequences

A repository or an export archive from a build before this change does not open. It's created
again.

This supersedes the part of 0022 that keeps the manifest key `source_meta`, and the sentence of
0006 that has Open report a master key without `config_hash` as an older format.

The tree node is outside this decision. Its JSON form is replaced by a binary encoding before
the freeze, which drops the stored offset and size of a chunk reference; until then a node is
decoded without refusing unknown keys, and no test pins a key of an earlier build.

`docs/SECURITY.md` no longer lists dated snapshot names as an accepted risk. Lock names still
carry their time.

A later 1.x release that wants a new manifest or config key adds it as 0034 describes: older
builds refuse the object.

Tests: `TestManifestKeepsLabelsUnderLabels`, `TestManifestStoresNoRatios`,
`TestSnapshotUnderAnotherNameIsRefused`, `TestSnapshotIDsCarryNoTimestamp`,
`TestImportNamesAForeignIDByItsHash`, `TestArchiveMetadataIsDecodedStrictly`,
`TestImportAcceptsACallerCompressedArchive`, `TestInitWritesTheConfigSchema`,
`TestOpenRejectsUnknownFormats`, `TestOpenRejectsAMasterKeyWithoutConfigHash`.
