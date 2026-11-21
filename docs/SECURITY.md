# Security

This page is the threat model of an ark repository, at rest and in transit: what ark protects,
against whom, and what it knowingly doesn't protect.

Every guarantee and every accepted risk ends with the name of the test that pins it. The tests
carry the detail. `TestSecurityDocCitesDefinedTests` fails when a cited test doesn't exist, or
when a guarantee or risk cites none.

## In short

For a repository that was created with a password:

- **Reading the storage reveals no content.** Someone who can read every object learns no file
  contents, no file names, and no snapshot metadata.
- **Changing the storage can't forge a restore.** A modified, swapped, or moved object fails to
  open. A restore returns the bytes that were backed up, or an error.
- **The storage can still delete and roll back.** Someone who can write to the storage can remove
  data, and can put the repository back into an older state without being detected.
- **Sizes and timing show.** The number and size of the stored objects reveal roughly how much
  data a backup added and when.
- **The client is trusted.** A machine that holds the password can read, forge, and delete
  everything.

The sections below state each of these precisely.

## Assets

- File contents, and the metadata trees record for them: paths, file and directory names, symlink
  targets, xattrs, sizes, modes, owners and times.
- Snapshot metadata: host, tags, labels, comment, source name, creation time and statistics.
- The password and the master key it unlocks.
- The integrity of a restore: the bytes that come back are the bytes that were backed up.

## Attacker model

In scope are three attackers, alone or together:

- A passive reader of the backend: reads every object, listing, name and size, as often as they like.
- An active writer on the backend: modifies, truncates, swaps, moves, deletes and puts back old
  objects, and copies objects in from another repository, also one opened with the same password.
- A network attacker between the client and an sftp server.

The guarantees hold for a repository created with a password. Data sent over sftp is already
sealed, so a network attacker on that path gains no more than an attacker on the backend: with the
host key verified, a passive reader of the traffic; with `insecure=1`, or as the server, an active
writer, every accepted risk included.

## Format

- Chunks, trees, index objects, manifests, locks and the lost-pack marker are sealed under the
  data key as `format byte | 24-byte nonce | XChaCha20-Poly1305(payload)` with the associated data
  `format byte | <namespace>/<name>`. Under format byte `0x02` the payload is the zstd frame of
  the content. Under `0x03` it is the content as it is, which the writer picks when the zstd frame
  would not be smaller. An object with any other format byte is rejected, and one whose format
  byte was changed to the other does not open: `TestUnknownFormatByteIsRejected`,
  `TestWriterStoresABlobRawWhenZstdIsNotSmaller`. A key file's `key` field is sealed the same way.
- Chunks and trees are stored inside pack objects: each one sealed on its own as above, followed
  by a sealed table of their names and lengths that carries the SHA-256 of the bytes before
  it. A pack is named by the SHA-256 of its bytes, which are ciphertext throughout. The layout
  of packs and index objects is in [Repository format](format.md).
- Chunk, tree and index names are the hex HMAC-SHA256 of their plaintext under the repository's ID
  key, the lost-pack marker's that of its namespace name, manifests and locks are sealed under
  chosen names: snapshot IDs are 128 random bits in 32 lowercase hex characters, lock names a
  millisecond timestamp plus 80 random bits. A repository may also hold snapshots under 26
  character Crockford base32 IDs, which ark resolves but does not write; an import of an archive
  whose ID has another form names the copy by the first 128 bits of the SHA-256 of that ID:
  `TestChunksAndTreesAreSealedUnderKeyedNames`, `TestSnapshotIDsCarryNoTimestamp`,
  `TestBase32SnapshotIDsStillWork`.
- The master key holds the data key, the ID key, the chunker seed (which keys the chunk boundaries),
  the repository ID and `config_hash`, the SHA-256 of the config bytes Init stored.
- A key file seals the master key under a wrap key derived with argon2id from the password and a
  16-byte random salt: time 3, memory 64 MiB, 4 threads. Its plaintext is version, kdf, the three
  parameters, salt and the sealed key, with no hostname or username, unlike restic's key files:
  `TestInitWritesOneKeyFile`, `TestEveryFlippedKeyFileByteFailsOpen`.
- Open refuses a key file asking for more than time 16 or 1 GiB of memory and tries at most 20 key
  files, so a forged key file cannot make it allocate or spin without bound:
  `TestOpenRejectsUnknownKeyFiles`, `TestOpenTriesAtMostTwentyKeyFiles`.
- The config is plain JSON (version, id, created_at, chunker, compression), authenticated through
  `config_hash`: `TestConfigHoldsOnlyItsPlaintextFields`, `TestTamperedConfigIsRejected`.

## Guarantees at rest

- Without the password no object name, body, listing or stat reveals file contents, paths, names,
  symlink targets, xattrs, exclude patterns or snapshot metadata, packs and index objects included:
  `TestBackendHoldsNoPlaintext`.
- Object names are keyed: the same files in two repositories share no name in any namespace, and no
  chunk is named by the SHA-256 of its content: `TestSameFilesInTwoRepositoriesShareNoNames`,
  `TestChunksAndTreesAreSealedUnderKeyedNames`.
- The snapshot IDs ark writes carry no timestamp; prefix resolution still works:
  `TestSnapshotIDsCarryNoTimestamp`.
- A wrong password fails with `ErrWrongPassword` after reading only the config and the key files:
  `TestOpenChecksThePassword`, `TestWrongPasswordReadsOnlyConfigAndKeys`.
- Every key file, chunk, tree and snapshot that is flipped in its format byte, nonce, ciphertext or
  tag, truncated, swapped, moved, copied from another namespace or replaced by the same-name object
  of another repository is rejected, a chunk or tree in the place its pack holds it: Open, Restore
  and Verify fail with the namespace's sentinel, Check lists it under errors, no restore returns
  different bytes and no Verify reports OK: `TestTamperedObjectsAreRejected`.
- An index object under the same mutations is skipped, so the chunks or trees it lists count as
  missing: Restore and Verify fail with `ErrNotFound` or report the chunk missing, Check lists it
  under errors, and prune deletes nothing while a snapshot references a blob no index lists:
  `TestTamperedObjectsAreRejected`, `TestPruneAbortsWhenTheIndexHidesALiveBlob`.
- The blob table of a pack under the same mutations is refused, also when the pack is stored
  under the name its changed bytes hash to: RebuildIndex names the pack unreadable and takes no
  entry from it, so a rebuilt index never points a name at other bytes. A changed table does not
  open. The table of another pack of the repository opens, since all tables of a namespace are
  sealed under one name, and is refused by the hash of the blobs it carries, which RebuildIndex
  compares with the pack it read: `TestTamperedObjectsAreRejected`,
  `TestPackTableDoesNotOpenBehindOtherBlobs`. A table read from the tail of a pack alone lacks
  that comparison; the blobs it lists then fail to open under their names:
  `TestPackTableOfAnotherPackOpensFromATailReadAndNoBlobDoes`.
- A pack that is gone while the index lists it is an error of Check whether or not a snapshot
  references it, as is a pack whose own table disagrees with the index. A backup passes over what
  the index lists in a pack that is gone and stores again what it reads. Once an index load has
  seen the lost data pack or a data index object that does not open, a marker makes every later
  backup read again a file it would take unchanged from its parent with a chunk no stored pack
  holds, and only a prune that finds every referenced chunk in a stored pack removes it. A backup
  whose own load finds the loss reads such a file the walk reaches after that load and fails
  rather than commit one it took unread before it; one whose own load found no loss asks for the
  marker again before it commits and, when it is there, looks up the packs again. Until a load has
  seen it, such a file keeps the parent's references to the lost chunks:
  `TestScenarioCheckCoversEverySnapshot`, `TestCheckReportsAPackThatDisagreesWithTheIndex`,
  `TestBackupStoresAgainWhatALostPackHeld`,
  `TestUnchangedFileIsReadAgainOnceALoadFoundALostPack`, `TestOnlyPruneDropsTheMarker`,
  `TestBackupThatFindsTheLossCommitsNoUnreadFileOfIt`, `TestBackupThatFindsTheLossReadsWhatItCan`,
  `TestBackupAsksForTheMarkerAgainBeforeItCommits`,
  `TestBackupChecksWhatItTookOnceTheMarkerComesAfterItsLoad`.
- A lock under the same mutations is treated as absent, Check lists it under warnings, and only a
  forced unlock deletes it: `TestTamperedObjectsAreRejected`,
  `TestTamperedLockIsIgnoredUntilForced`.
- A key file with any byte changed, or one from another repository, does not open:
  `TestEveryFlippedKeyFileByteFailsOpen`, `TestKeyFileDoesNotOpenElsewhere`.
- A config that is flipped, truncated, reformatted or replaced by another repository's fails Open
  with `ErrCorruptConfig`, also after ChangePassword, and a master key without `config_hash` is
  refused: `TestTamperedConfigIsRejected`, `TestOpenRejectsAMasterKeyWithoutConfigHash`.
- A replayed snapshot manifest restores exactly the bytes its snapshot held, never newer ones;
  after a prune it fails with `ErrNotFound` on its root tree: `TestReplayedManifestRestoresOnlyItsOwnBytes`.
- An old config put back beside the current key files fails Open with `ErrCorruptConfig`:
  `TestReplayedConfigOpensOnlyWithItsKeyFile`.

## Guarantees in transit

- sftp verifies the server's host key against known_hosts: an unknown host and a changed key are
  rejected: `TestHostKeysRejectsAnUnknownHost`, `TestHostKeysRejectsAChangedKey`,
  `TestHandshakeVerifiesTheServerHostKey`.
- `insecure=1` accepts any host key and logs a warning once: `TestHostKeysInsecureWarnsOnce`.
- A password never travels in a URI: arkuri rejects `user:pass@` with an error naming
  `ARK_SFTP_PASSWORD`, and no parse error echoes the URI: `TestOpenRejects`, `TestOpenNeverEchoesAPassword`.
- No command takes a password as a flag, so it never lands in argv; the inputs are `ARK_PASSWORD`,
  `--password-file` or `ARK_PASSWORD_FILE`, `--password-command` or `ARK_PASSWORD_COMMAND`, a
  profile's `password-file` or `password-env`, and the prompt; `ark key passwd` takes the new one
  from `ARK_NEW_PASSWORD`, `--new-password-file` or its prompt: `TestNoPasswordFlag`.
- The password never appears in the library's or the CLI's logs, debug level included, during
  Init, Open, a failed Open and ChangePassword: `TestPasswordNeverInTheLogs`.

## Accepted risks

ark keeps no freshness anchor, so a writer on the backend can roll the repository back to an older
state. A rollback never restores bytes a snapshot did not hold, but it is not detected.

- Forgotten snapshots come back when their manifests are put back, until a prune has removed their
  data: `TestReplayedManifestRestoresOnlyItsOwnBytes`.
- ChangePassword does not revoke the old password: the old key file, put back, opens the
  repository under it again: `TestReplayedKeyFileOpensUnderTheOldPassword`.
- An old config put back together with its old key file opens: `TestReplayedConfigOpensOnlyWithItsKeyFile`.
- A replayed lock blocks writers until it is stale: `TestReplayedLockIsHonouredUntilItIsStale`.
- The index objects of a pruned backup, put back, list packs that are gone. A load of the index
  lists the packs that are stored and passes over what is listed in any other, so a later backup
  of the same data stores it again and its snapshot restores. The replay is not detected as one:
  Check names the packs it lists as not stored, as a warning once a stored pack holds what they
  listed, until a prune or a rebuild of the index drops the entries:
  `TestReplayedIndexObjectIsPassedOver`.
- Object sizes and counts per namespace show: the size of every pack and index object, and with the
  index sizes the number of chunks and trees. A sealed chunk is 41 bytes plus its zstd frame under
  format byte `0x02`, or plus its plain length under `0x03`, and a pack is its sealed chunks plus a
  sealed table, so the stored bytes show, while the boundaries between the blobs of a pack do not,
  they sit in the sealed table. A stored lost-pack marker shows that an index load found a data
  pack missing or a data index object that does not open: `TestListingRevealsCountsAndSizes`.
- The format byte is plaintext. For a single object, a key file's `key` field, and the first blob
  and the table of a pack it shows whether the content compressed, and an object stored raw is its
  exact plain length plus 41 bytes. The format bytes of the other blobs of a pack sit at offsets
  only the sealed table gives: `TestWriterStoresABlobRawWhenZstdIsNotSmaller`.
- The number of packs shows the size of what a backup added: a pack closes once its blobs reach
  16 MiB, so the number of full packs and the size of the run's last, short one give it:
  `TestPackWriterClosesPacksAtTheTargetSize`.
- The argon2id parameters and the config are plaintext: `TestInitWritesOneKeyFile`,
  `TestConfigHoldsOnlyItsPlaintextFields`.
- Lock names carry the millisecond they were created: `TestLockNamesCarryTheirTime`.
- A 26 character base32 snapshot ID begins with the millisecond its snapshot was created in its
  first 10 characters, so whoever can list `snapshots/` dates those snapshots:
  `TestBase32SnapshotIDsStillWork`.
- `ARK_PASSWORD` and `ARK_NEW_PASSWORD` are visible to every process of the same user;
  `--password-file` and `--new-password-file` are the safer inputs: `TestNoPasswordFlag`.

## Out of scope

- A compromised client holding the password: it can read, forge and delete everything. ark has
  no append-only mode against it.
- Side channels on the client host: memory, swap, core dumps and timing. Key material is not zeroed
  in memory and core dumps are not disabled.
- A leaked master key: ChangePassword re-seals the same key, a leaked repository needs a new one.
  ark has no key rotation.
- Availability: a writer on the backend can delete objects; Check, Verify and Restore report what is
  missing, nothing prevents it.
- Repositories created with `NoPassword` or `--insecure-no-password`: the master key is stored
  unprotected, so anyone who reads the backend can read and forge them.
- Metadata the storage host keeps on its own, such as modification times: they show when each pack
  and index object was written, and with it when backups ran and how long they took.

## Reporting a vulnerability

Report it privately through GitHub Private Vulnerability Reporting: the repository's Security tab,
"Report a vulnerability" (https://github.com/alexjoedt/ark/security/advisories/new). Do not open a
public issue. There is no promised response time.

## Dependency scanning

The CI step `Vulnerability check` in `.github/workflows/go.yml` runs govulncheck against every
go.work module (root, arkdocker, cmd/ark, examples) and fails on a finding in reachable code;
`task vuln` runs the same loop locally. gosec runs as part of golangci-lint.
