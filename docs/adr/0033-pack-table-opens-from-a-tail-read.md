# 0033: A pack table that opens from the tail of its pack

Status: accepted
Date: 2026-11-21

## Context

0010 sealed the blob table of a pack with the associated data
`<pack namespace>/<hex SHA-256 of the blob section>`. A reader therefore needed every byte of a
pack before it could open the table, and rebuilding the index from the packs downloads the whole
repository where the tables alone would do. On a local disk or over SFTP that costs time. On an
object storage backend it costs egress for all stored data.

The layout of a pack is part of repository format version 1 and cannot change after the first
stable release. What a table is bound to had to be settled before it: a build that reads tables
by their tail cannot be added later when the table does not open that way.

## Decision

The sealed table starts with the SHA-256 of the blob section, 32 bytes, followed by the entries
as before. Its associated data is the format byte and `datapacks/table` or `treepacks/table`, the
same for every pack of a namespace. The writer has the sum before it seals the table, so a pack
is still written in one pass (`finish` in `pack.go`).

A reader that holds the whole pack hashes the blob section and refuses a table that carries
another sum (`parse`). A reader that holds only the tail takes the last 4 bytes as the length of
the sealed table, reads that many bytes before them and opens the table (`openTable`).

This supersedes the part of 0010 that puts the hash of the blob section into the associated data
of the table, and its consequence that opening a table needs the whole pack. The rest of 0010
stands.

Rebuilding the index keeps reading whole packs for now. It also checks each pack against its name
that way. A rebuild from the tails is a change of the reader, not of the format.

- Not the binding of 0010, because it fixes the cost of every later reader of the tables at the
  size of the repository.
- Not the pack's name in the associated data, because the name is the hash of all bytes of the
  pack, the sealed table among them.
- Not the hash of the blob section in the pack's name or in a plaintext trailer, because the
  sealed table already authenticates what it carries and a second place for the sum adds a second
  check to keep in step.
- Not the length of the blob section beside the sum, because the entries give it.

## Consequences

The table of a pack opens with two range reads: `TestPackTableOpensFromATailRead`. Every table
grows by 32 bytes.

A table is no longer rejected by its tag when it sits behind other blobs. Whoever can write to
the storage can put the sealed table of one pack behind the blobs of another pack of the same
namespace and repository. A reader of the whole pack refuses it by the sum, as before:
`TestPackTableDoesNotOpenBehindOtherBlobs`, and the pack table rows of
`TestTamperedObjectsAreRejected` for a rebuild. A reader of the tail alone takes the table as
that of the pack. It cannot be led to other content by it: a blob is sealed under its own name,
so every blob read at a place such a table lists fails to open
(`TestPackTableOfAnotherPackOpensFromATailReadAndNoBlobDoes`). What a reader of the tail loses is
the early refusal. It learns of the swap when it reads a blob, and an index built from tails
alone would list blobs that then do not open, which a check reports as it reports any blob that
does not open.

A table cannot be forged or changed without the data key, and one of the other pack namespace or
of another repository does not open, as under 0010.

A pack sealed by an earlier build does not parse. The repository format version stays 1, since
version 1 is not frozen before the first stable release.
