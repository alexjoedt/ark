# Security

The threat model of an ark repository at rest and in transit. Every guarantee and every accepted
risk ends with the test that pins it; the tests carry the detail. `TestSecurityDocCitesDefinedTests`
fails when a cited test does not exist or a guarantee or risk cites none.
The reasoning behind the boundaries is in `docs/adr/0006-threat-model-boundaries.md`.

## Assets

- File contents, and the metadata trees record for them: paths, file and directory names, symlink
  targets, xattrs, sizes, modes, owners and times.
- Snapshot metadata: host, tags, comment, source name, creation time and statistics.
- The passphrase and the master key it unlocks.
- The integrity of a restore: the bytes that come back are the bytes that were backed up.

## Attacker model

In scope are three attackers, alone or together:

- A passive reader of the backend: reads every object, listing, name and size, as often as they like.
- An active writer on the backend: modifies, truncates, swaps, moves, deletes and puts back old
  objects, and copies objects in from another repository, also one opened with the same passphrase.
- A network attacker between the client and an sftp server, or between a browser and `ark serve`.

The guarantees hold for a repository created with a passphrase. Data sent over sftp is already
sealed, so a network attacker on that path gains no more than an attacker on the backend: with the
host key verified, a passive reader of the traffic; with `insecure=1`, or as the server, an active
writer, every accepted risk included.

## Format

- Chunks, trees, index objects, manifests, locks and the lost-pack marker are sealed under the
  data key as `0x02 | 24-byte nonce | XChaCha20-Poly1305(zstd frame)` with the associated data
  `<namespace>/<name>`; data is compressed with zstd before it is sealed, and an object with any
  other format byte is rejected: `TestUnknownFormatByteIsRejected`. A key file's `key` field is
  sealed the same way.
- Chunks and trees are stored inside pack objects: each one sealed on its own as above, followed
  by a sealed table of their names and lengths that is bound to the SHA-256 of the bytes before
  it. A pack is named by the SHA-256 of its bytes, which are ciphertext throughout. The layout
  of packs and index objects is in [FORMAT.md](FORMAT.md).
- Chunk, tree and index names are the hex HMAC-SHA256 of their plaintext under the repository's ID
  key, the lost-pack marker's that of its namespace name, manifests and locks are sealed under
  chosen names: snapshot IDs are 128 random bits in 26 Crockford base32 characters, lock names a
  millisecond timestamp plus 80 random bits:
  `TestChunksAndTreesAreSealedUnderKeyedNames`, `TestSnapshotIDsCarryNoTimestamp`.
- The master key holds the data key, the ID key, the chunker seed (which keys the chunk boundaries),
  the repository ID and `config_hash`, the SHA-256 of the config bytes Init stored.
- A key file seals the master key under a wrap key derived with argon2id from the passphrase and a
  16-byte random salt: time 3, memory 64 MiB, 4 threads. Its plaintext is version, kdf, the three
  parameters, salt and the sealed key, with no hostname or username, unlike restic's key files:
  `TestInitWritesOneKeyFile`, `TestEveryFlippedKeyFileByteFailsOpen`.
- Open refuses a key file asking for more than time 16 or 1 GiB of memory and tries at most 20 key
  files, so a forged key file cannot make it allocate or spin without bound:
  `TestOpenRejectsUnknownKeyFiles`, `TestOpenTriesAtMostTwentyKeyFiles`.
- The config is plain JSON (version, id, created_at, chunker, compression), authenticated through
  `config_hash`: `TestConfigHoldsOnlyItsPlaintextFields`, `TestTamperedConfigIsRejected`.

## Guarantees at rest

- Without the passphrase no object name, body, listing or stat reveals file contents, paths, names,
  symlink targets, xattrs, exclude patterns or snapshot metadata, packs and index objects included:
  `TestBackendHoldsNoPlaintext`.
- Object names are keyed: the same files in two repositories share no name in any namespace, and no
  chunk is named by the SHA-256 of its content: `TestSameFilesInTwoRepositoriesShareNoNames`,
  `TestChunksAndTreesAreSealedUnderKeyedNames`.
- Snapshot IDs carry no timestamp; prefix resolution still works: `TestSnapshotIDsCarryNoTimestamp`.
- A wrong passphrase fails with `ErrWrongPassword` after reading only the config and the key files:
  `TestOpenChecksThePassphrase`, `TestWrongPasswordReadsOnlyConfigAndKeys`.
- Every key file, chunk, tree and snapshot that is flipped in its format byte, nonce, ciphertext or
  tag, truncated, swapped, moved, copied from another namespace or replaced by the same-name object
  of another repository is rejected, a chunk or tree in the place its pack holds it: Open, Restore
  and Verify fail with the namespace's sentinel, Check lists it under errors, no restore returns
  different bytes and no Verify reports OK: `TestTamperedObjectsAreRejected`.
- An index object under the same mutations is skipped, so the chunks or trees it lists count as
  missing: Restore and Verify fail with `ErrNotFound` or report the chunk missing, Check lists it
  under errors, and prune deletes nothing while a snapshot references a blob no index lists:
  `TestTamperedObjectsAreRejected`, `TestPruneAbortsWhenTheIndexHidesALiveBlob`.
- The blob table of a pack under the same mutations does not open, also when the pack is stored
  under the name its changed bytes hash to: RebuildIndex names the pack unreadable and takes no
  entry from it, so a rebuilt index never points a name at other bytes:
  `TestTamperedObjectsAreRejected`.
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
- A passphrase never travels in a URI: arkuri rejects `user:pass@` with an error naming
  `ARK_SFTP_PASSWORD`, and no parse error echoes the URI: `TestOpenRejects`, `TestOpenNeverEchoesAPassword`.
- No command takes a passphrase as a flag, so it never lands in argv; the inputs are `ARK_PASSWORD`,
  `--password-file` or `ARK_PASSWORD_FILE`, and the prompt: `TestNoPasswordFlag`.
- The passphrase never appears in the library's or the CLI's logs, debug level included, during
  Init, Open, a failed Open and ChangePassword: `TestPassphraseNeverInTheLogs`.
- `ark serve` speaks plain HTTP and binds `127.0.0.1:8080` by default; it warns at start when it
  binds anything but a loopback IP and when CSRF protection is off. Any other address belongs behind
  a TLS reverse proxy: `TestServeDefaultsToLoopback`, `TestServeWarnsOnANonLoopbackAddress`,
  `TestServeWarnsWhenCSRFIsOff`.

## Accepted risks

ark keeps no freshness anchor, so a writer on the backend can roll the repository back to an older
state. A rollback never restores bytes a snapshot did not hold, but it is not detected.

- Forgotten snapshots come back when their manifests are put back, until a prune has removed their
  data: `TestReplayedManifestRestoresOnlyItsOwnBytes`.
- ChangePassword does not revoke the old passphrase: the old key file, put back, opens the
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
  index sizes the number of chunks and trees. A sealed chunk is its zstd frame plus 41 bytes and a
  pack is its sealed chunks plus a sealed table, so the stored bytes show, while the boundaries
  between the blobs of a pack do not, they sit in the sealed table. A stored lost-pack marker shows
  that an index load found a data pack missing or a data index object that does not open:
  `TestListingRevealsCountsAndSizes`.
- The number of packs shows the size of what a backup added: a pack closes once its blobs reach
  16 MiB, so the number of full packs and the size of the run's last, short one give it:
  `TestPackWriterClosesPacksAtTheTargetSize`.
- The argon2id parameters and the config are plaintext: `TestInitWritesOneKeyFile`,
  `TestConfigHoldsOnlyItsPlaintextFields`.
- Lock names carry the millisecond they were created: `TestLockNamesCarryTheirTime`.
- `ARK_PASSWORD` is visible to every process of the same user; `--password-file` is the safer
  input: `TestNoPasswordFlag`.

## Out of scope

- A compromised client holding the passphrase: it can read, forge and delete everything. An
  append-only mode against it is a Backlog item.
- Side channels on the client host: memory, swap, core dumps and timing. Key material is not zeroed
  in memory and core dumps are not disabled.
- A leaked master key: ChangePassword re-seals the same key, a leaked repository needs a new one.
  Key rotation is a Backlog item.
- Availability: a writer on the backend can delete objects; Check, Verify and Restore report what is
  missing, nothing prevents it.
- Repositories created with `WithoutPassword` or `--insecure-no-password`: the master key is stored
  unprotected, so anyone who reads the backend can read and forge them.
- TLS for `ark serve`: terminate it in a reverse proxy.
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
