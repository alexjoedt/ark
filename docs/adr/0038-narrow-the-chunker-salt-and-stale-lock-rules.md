# 0038: Narrow the chunker, salt and stale lock rules before the freeze

Status: accepted
Date: 2026-11-22

## Context

Writing docs/format.md for an implementer showed three rules of format 1 that a reader built
from the page alone could not match, or that hurt ark itself:

- A config's chunker sizes had no upper bound. A config could demand a `max` of 1 TiB, and every
  backup into it allocates a buffer of `max` bytes. `avg` could be any number; the masks rounded
  it down to a power of two, so two configs that differ in `avg` cut the same chunks.
- A key file's argon2id salt could have any length but 0. golang.org/x/crypto/argon2 takes that,
  libsodium's `crypto_pwhash` takes exactly 16 bytes. A key file with another salt would open in
  ark and in no reader built on libsodium. ark never wrote another length.
- A lock of the reader's own host counted as stale at once when no process with its `pid`
  existed. Host name and PID don't identify a process across PID namespaces: a container that
  shares its host's name, or two containers given one name, see each other's running locks as
  stale, and a prune beside a running backup deletes the backup's packs.

Format 1 freezes with the first stable release. Narrowing what a reader accepts is cheap only
before then: nothing stored can be outside the new rules yet, and after the freeze a refusal
would lock users out of repositories they already have.

## Decision

- A config whose chunker `max` is above 64 MiB, or whose `avg` is not a power of two, is
  `ErrUnsupportedFormat` on `Open`; `Init` refuses it with an error and writes nothing. 64 MiB is
  eight times restic's largest chunk and sixteen times ark's default, and every reader can hold a
  buffer that size.
- A key file whose salt is not exactly 16 bytes in lowercase hex is `ErrUnsupportedFormat`, so every argon2id
  library reads every key file.
- A lock is stale only by its age, the stale age of 30 minutes. `hostname`, `username` and `pid`
  stay in the lock object; they name the holder for `ark unlock` and error messages and no longer
  decide anything.

Not chosen for the lock: a PID namespace field beside the PID. It is a new stored field with a
Linux-only meaning, and age-only staleness is the safe direction a later format version can
relax: a reader that finds such a field on both sides may apply the process check again.

## Consequences

- A crashed operation blocks conflicting operations on its own host for up to 30 minutes, as it
  always did on other hosts. That includes a lock a power loss left behind: ADR 0005 never syncs
  locks because such a lock is stale anyway, and after a quick reboot it now becomes stale only
  by age. `ark unlock --force` clears it sooner. The kill harness does that after every kill.
- The avg rounding is gone from the chunker spec; `b` is the logarithm of `avg`.
- Tests pin each rule: `TestInitRejectsInvalidChunkerConfig`, `TestInitAcceptsAChunkerMaxOf64MiB`,
  `TestOpenRejectsUnknownFormats`, `TestOpenTakesOnlyA16ByteSalt` and
  `TestLockOfThisHostWithADeadPIDIsStaleOnlyByAge`.
