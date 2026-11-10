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

- Chunks, trees, manifests, locks and markers are sealed under the data key as `0x02 | 24-byte
  nonce | XChaCha20-Poly1305(zstd frame)` with the associated data `<namespace>/<name>`; data is
  compressed with zstd before it is sealed, and an object with any other format byte is rejected:
  `TestUnknownFormatByteIsRejected`. A key file's `key` field is sealed the same way.
- Chunk and tree names are the hex HMAC-SHA256 of their plaintext under the repository's ID key,
  manifests, locks and markers are sealed under chosen names: snapshot IDs are 128 random bits in
  26 Crockford base32 characters, lock and marker names a millisecond timestamp plus 80 random bits:
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
  symlink targets, xattrs or snapshot metadata, markers included: `TestBackendHoldsNoPlaintext`.
- Object names are keyed: the same files in two repositories share no name in any namespace, and no
  chunk is named by the SHA-256 of its content: `TestSameFilesInTwoRepositoriesShareNoNames`,
  `TestChunksAndTreesAreSealedUnderKeyedNames`.
- Snapshot IDs carry no timestamp; prefix resolution still works: `TestSnapshotIDsCarryNoTimestamp`.
- A wrong passphrase fails with `ErrWrongPassword` after reading only the config and the key files:
  `TestOpenChecksThePassphrase`, `TestWrongPasswordReadsOnlyConfigAndKeys`.
- Every key file, chunk, tree and snapshot that is flipped in its format byte, nonce, ciphertext or
  tag, truncated, swapped, moved, copied from another namespace or replaced by the same-name object
  of another repository is rejected: Open, Restore and Verify fail with the namespace's sentinel,
  Check lists it under errors, no restore returns different bytes and no Verify reports OK:
  `TestTamperedObjectsAreRejected`.
- A lock or marker under the same mutations is treated as absent, Check lists it under warnings,
  and only a forced unlock deletes it: `TestTamperedObjectsAreRejected`,
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
- A replayed marker puts the next backup into repair mode, which re-reads the chunks it reuses,
  until a prune clears it: `TestReplayedMarkerForcesRepairMode`.
- Object sizes and counts per namespace show; a sealed chunk is its zstd frame plus 41 bytes, so
  the chunk-size distribution and the rough size of small files show: `TestListingRevealsCountsAndSizes`.
- The argon2id parameters and the config are plaintext: `TestInitWritesOneKeyFile`,
  `TestConfigHoldsOnlyItsPlaintextFields`.
- Lock and marker names carry the millisecond they were created: `TestLockAndMarkerNamesCarryTheirTime`.
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

## Reporting a vulnerability

Report it privately through GitHub Private Vulnerability Reporting: the repository's Security tab,
"Report a vulnerability" (https://github.com/alexjoedt/ark/security/advisories/new). Do not open a
public issue. There is no promised response time.

## Dependency scanning

The CI step `Vulnerability check` in `.github/workflows/go.yml` runs govulncheck against every
go.work module (root, arkdocker, cmd/ark, examples) and fails on a finding in reachable code;
`task vuln` runs the same loop locally. gosec runs as part of golangci-lint.
