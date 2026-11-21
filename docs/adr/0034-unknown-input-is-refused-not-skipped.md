# 0034: What a build does not know it refuses, per object

Status: accepted
Date: 2026-11-21

## Context

Repository format version 1 is frozen with the first stable release, and ark has no migration: a
version 2 is a new repository for every user. So the question of how version 1 can still grow had
to be answered before the freeze, together with what an older build does when it meets something
a later build of the same release line wrote.

Until now it did one of three things, none of them right:

- A sealed object under a format byte other than `0x02` or `0x03` was reported as corrupt. A
  codec added later would have made every earlier build call a sound repository damaged, and its
  index load and index rebuild would have skipped the object.
- A tree entry of a file type the build did not know matched no case of the walk and was left
  out. A restore then wrote a snapshot with a file missing and returned no error.
- Every JSON reader dropped the keys it had no field for. A build that prunes does not have to
  understand a key to destroy what the key refers to.

The pack table and the index object are fixed-size binary records without a version or a
reserved field. The config cannot change after Init, because the master key seals its hash, so a
list of features there would say no more than the version number does.

## Decision

A build fails closed, object by object. Whatever it does not know is `ErrUnsupportedFormat`. It
is never skipped and never reported as corruption:

- A sealed object that authenticates under a format byte the build does not know. The message
  names the byte. The byte is part of the associated data (0032), so the tag is checked first
  and the byte is judged after it (`unseal` in `objectstore.go`): only a holder of the key can
  have put it there. An object that does not authenticate is corrupt, whatever its first byte.
- A tree entry of a file type the build does not know. Every reader of a tree loads its nodes
  through `loadTreeNode`, which refuses the node, so restore, verify, check, export, the reader
  behind mount, and prune all stop.
- A config, a key file, the master key inside it, a manifest with its statistics, or a lock with
  a JSON key the build does not know (`decodeStrict` in `config.go`). The message names the key.
- A config, key file or manifest of another version, as before.

A check does not list such an object among its findings. It fails, because a build that cannot
read what is stored cannot say whether it is sound.

A lock the build cannot read in full stops every operation that would take a lock, and `Locks`
too. A lock that fails its tag is still passed over, as before. `Unlock` with `WithForce` removes
both kinds.

A repository can carry one more sealed object, `features`, a JSON object of the shape
`{"required": ["name", ...]}`. A build that finds a name it does not know there refuses to open
the repository with `ErrUnsupportedFormat`. A repository without the object requires nothing. No
build of version 1 writes it. It exists so that a later release can change what a whole
repository means, where refusing single objects is not enough, and still be refused by every
earlier build. The reader for this object is not in the code yet when this record is written.

A later format byte keeps the envelope of a sealed object: one byte, a 24-byte nonce,
XChaCha20-Poly1305 under the same key, the byte and `<namespace>/<name>` as associated data.
That is what lets an older build tell a later codec from damage. A change to the envelope
itself is a required feature.

The pack table and the index object get no version field and no reserved bytes. A change to
either is a new format byte on the sealed object that carries it, or a required feature.

What the release line promises follows from this: every 1.x release reads what an earlier 1.x
release wrote, and an older release refuses what it does not know instead of misreading it.
`docs/versioning.md` says so.

- Not ignoring unknown JSON keys, because "optional" is a claim only the later build can make,
  and an older build that prunes, forgets or copies an object acts on it without the key.
- Not a list of features in the config, because the config is bound to the master key at Init
  and cannot gain an entry afterwards.
- Not version fields in the pack table and the index object, because the format byte of the
  sealed object already tells a reader that the layout is one it does not know, and a field no
  build has ever read is a field no build is known to check.
- Not corruption for an unknown format byte on an object that authenticates. Corruption is
  skipped by an index load, listed by a verify and repaired around by a rebuild, and none of
  that may happen to an object a later build wrote.
- Not the check of the byte before the tag, which would also cover a later build that changes
  the envelope. Everything that damages the first byte of an object would then read as a format
  this build lacks: an object filled with zeros, or a table read from the wrong place because
  the trailer of its pack was changed. The paths that skip, list and repair corruption would
  not take such an object, a manifest in that state could not be removed with a forget, and
  whoever can write to the storage could stop every operation with one byte in `locks/`.
- Not passing over a lock with an unknown key the way a lock that does not open is passed over,
  because the key may be the very thing that tells a reader what the lock guards.

## Consequences

A later 1.x release can add a codec, a file type, or a JSON key, and each older build refuses
exactly the objects that use it. A repository stays readable by an older build until the first
such object is stored. From then on that build fails on the snapshots, or on the whole
repository, that the object touches.

Adding a JSON key is no longer free. An older build refuses a manifest, lock, key file or config
that carries it, so a key is added only when older builds have to stop.

`ErrUnsupportedFormat` is returned by more than Open. Callers that matched it only there now see
it from every operation.

A format byte that was changed on a stored object fails the tag and is corruption, as before:
`TestTamperedObjectsAreRejected` flips it to the other codec's, and
`TestUnknownFormatByteIsRejected` sets it to one no build writes. Nobody without the key can
make an object read as unsupported.

A later build that seals objects in another envelope and does not mark the repository with a
required feature is read as corrupt by older builds. The rule above is what prevents that.

A manifest a later build wrote cannot be removed with a forget by an older build: only a
manifest that is corrupt is removed without being read.

The encoding of a tree node is as lenient as before in every other respect. It accepts keys it
does not know. Only the file type is checked here.

Tests: `TestUnknownFormatByteIsRejected`, `TestUnknownFileTypeFailsEveryReader`,
`TestManifestIsDecodedStrictly`, `TestLockWithAnUnknownKeyStopsTheOperation`,
`TestOpenRefusesUnknownKeys`, `TestOpenRejectsUnknownKeyFiles`, `TestDecodeStrict`.
