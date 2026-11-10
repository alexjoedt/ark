# 0006: Threat model boundaries

Status: accepted
Date: 2026-11-08

## Context

The goal of the security review named confidentiality without the passphrase and
rejection of modified, swapped and replayed objects, but not where the model stops. Open were the
attacker, whether the plain-JSON config is authenticated, which replays are rejected and with what
freshness anchor, which metadata may leak, and what ark promises about the passphrase.

A survey of restic, borg and kopia shaped the answers. Only borg detects a rolled-back repository,
and only through a client-side anchor, the last manifest timestamp it saw in a local security
directory. restic and kopia keep no anchor and leave rollback and deletion to append-only storage
and object lock. All three show object sizes and counts and none pads by default. restic encrypts
and authenticates its config, kopia seals the secret half of its format blob, borg's config is
plaintext and unauthenticated.

## Decision

The attacker is a passive reader of the backend, an active writer on the backend and a network
attacker between the client and an sftp server or between a browser and `ark serve`. A compromised
client holding the passphrase and side channels on the client host (memory, swap, timing) are out
of scope; `docs/SECURITY.md` lists them.

The config is bound into the master key: the key file holds the SHA-256 of the exact config bytes
Init stored as `config_hash`, and Open rejects a config that does not match, or a master key
without the field, with `ErrCorruptConfig`. There is no legacy path and no key file version bump,
since v0 has no users to migrate. A wrong chunker, compression or version in a swapped config can
therefore no longer pass as the repository's own.

ark keeps no freshness anchor. A replay never makes a restore return different bytes, because
objects are bound to their name and namespace by their associated data, but a rollback of the
repository state is accepted and not detected: a forgotten snapshot manifest comes back until a
prune removed its data, an old key file reopens the repository under the old passphrase after
ChangePassword (restic documents the same), a replayed lock blocks until it is stale and a
replayed marker forces repair mode.

Snapshot IDs are 128 random bits, so a listing reveals no snapshot time; the encoding is a detail
the ID format may change. Locks and markers keep a millisecond timestamp in their name, since
staleness and cleanup read it. Object sizes and counts per namespace, the chunk-size distribution,
the argon2id parameters and the config plaintext are accepted and documented at their exact extent.

The passphrase never appears in a log (debug included), in a URI or in argv. It is read from
`ARK_PASSWORD`, `--password-file` or the prompt; `ARK_PASSWORD` is visible to processes of the same
user, and zeroing memory or disabling core dumps is not promised. `ark serve` binds loopback by
default and warns on a non-loopback address; TLS is a reverse proxy's job.

- Not a client-side anchor like borg's, because it needs state per client and per repository that a
  second machine or a lost config directory silently resets, and it protects only the client that
  saw the newer state.
- Not a signed snapshot index, because it would add a head object every writer must serialise on,
  against the lock-free concurrent backups of the repository.
- Not padding of chunk sizes, because it costs space on every object and borg's opt-in
  `obfuscate`, kopia's 4 KiB rounding and restic's pack shuffling each leave counts visible anyway.
- Not an authenticated copy of the config beside the hash, because the hash in the master key
  already makes any change detectable and keeps the config readable for tools.

## Consequences

Every guarantee and accepted risk of `docs/SECURITY.md` ends with a test that pins it, and
`security_doc_test.go` fails when one cites none; the tests pin this decision:
`TestTamperedConfigIsRejected` and `TestOpenRejectsAMasterKeyWithoutConfigHash` for the config
hash, `TestReplayedManifestRestoresOnlyItsOwnBytes`, `TestReplayedKeyFileOpensUnderTheOldPassword`
and `TestReplayedConfigOpensOnlyWithItsKeyFile` for the replays, `TestSnapshotIDsCarryNoTimestamp`
in `encryption_test.go` for the IDs, `TestListingRevealsCountsAndSizes` for the sizes and
`TestPassphraseNeverInTheLogs` for the passphrase. A user who needs protection against rollback or
a leaked master key has no answer yet: append-only mode and key rotation stay open items. A cluster
service that wants rollback detection must keep its own record of the snapshot set.
