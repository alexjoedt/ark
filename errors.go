package ark

import "errors"

// Sentinel errors returned by Repository methods.
var (
	// ErrNotFound is returned when a requested snapshot does not exist.
	ErrNotFound = errors.New("ark: snapshot not found")

	// ErrAmbiguousID is returned when a snapshot id prefix matches more than
	// one snapshot. The message names the candidates.
	ErrAmbiguousID = errors.New("ark: snapshot id prefix is ambiguous")

	// ErrEmptyPolicy is returned by Forget when the policy keeps nothing and
	// the filter names no ids, which would remove every matching snapshot.
	ErrEmptyPolicy = errors.New("ark: forget policy keeps nothing and no ids given")

	// ErrInvalidPolicy is returned by Forget, before it reads anything, for
	// a policy with a negative count other than Unlimited, a keep-within
	// window (KeepWithin to KeepWithinYearly) outside the bounds of [Within],
	// a KeepTags list without a tag, or one with the empty tag next to others.
	ErrInvalidPolicy = errors.New("ark: invalid forget policy")

	// ErrNoSnapshotKept is returned by Forget when the policy keeps no
	// snapshot of a series among the matches, which only a policy of KeepTags
	// alone can do. Nothing is removed, of no series; the message names the
	// series. A snapshot Forget keeps for being dated in the future does not
	// count as kept, and the message names those of the series too.
	ErrNoSnapshotKept = errors.New("ark: forget policy keeps no snapshot of a series")

	// ErrInvalidLabel is returned by ValidateLabels, and by Backup before it
	// writes anything, for a label key or value a caller may not set, or a
	// key the source sets as well.
	ErrInvalidLabel = errors.New("ark: invalid label")

	// ErrInvalidTime is returned by Backup and ValidateBackupOptions for a
	// WithTime that is the zero time or lies in the future.
	ErrInvalidTime = errors.New("ark: invalid snapshot time")

	// ErrIncompatibleSourceType is returned when restoring a snapshot into a
	// Sink type that is incompatible with the snapshot's SourceType.
	ErrIncompatibleSourceType = errors.New("ark: incompatible source type")

	// ErrCorruptManifest is returned when a snapshot manifest does not
	// authenticate: it was changed, truncated or moved from another snapshot.
	// Snapshots, Prune and a Forget policy fail while one exists; remove it
	// by naming its id to Forget without a policy, then Prune.
	ErrCorruptManifest = errors.New("ark: corrupt snapshot manifest")

	// ErrCorruptConfig is returned by Open when a key file opens but the
	// repository config is not the one its master key was created with: the
	// config was changed or replaced, or the key file belongs to another
	// repository. The message says which of the two the config's id points to.
	ErrCorruptConfig = errors.New("ark: repository config does not match its master key")

	// ErrCorruptTree is returned by Restore, Export and Verify when a tree
	// object of the snapshot does not authenticate or does not match its name.
	ErrCorruptTree = errors.New("ark: corrupt snapshot tree")

	// ErrCorruptChunk is returned by Restore and Export when a stored chunk
	// fails its integrity check. Verify lists such a chunk in CorruptChunks.
	ErrCorruptChunk = errors.New("ark: corrupt chunk")

	// ErrCorruptIndex is the error of an index object that does not
	// authenticate or does not describe itself: it was changed, truncated or
	// moved from another name or namespace. An operation that only looks
	// blobs up does not return it: it skips such an object with a warning and
	// reports the blobs only that object lists as missing.
	ErrCorruptIndex = errors.New("ark: corrupt index object")

	// ErrMissingChunk is returned by Restore, Export, Prune and the reads of a
	// Reader when a chunk a snapshot needs is not stored. It matches
	// ErrNotFound too. Verify lists such a chunk in MissingChunks.
	ErrMissingChunk error = missingError("ark: missing chunk")

	// ErrMissingTree is ErrMissingChunk for a tree object.
	ErrMissingTree error = missingError("ark: missing tree")

	// ErrRepoLocked is returned when a repo-level lock cannot be acquired.
	ErrRepoLocked = errors.New("ark: repository is locked by another operation")

	// ErrNotInitialized is returned by Open when the backend holds no
	// repository config. Call Init first.
	ErrNotInitialized = errors.New("ark: repository is not initialized")

	// ErrAlreadyInitialized is returned by Init when the backend already
	// holds a repository config.
	ErrAlreadyInitialized = errors.New("ark: repository is already initialized")

	// ErrUnsupportedFormat is returned when the repository holds something
	// this build does not read: by Open for a config or key file of another
	// version, and by every operation for a sealed object under an unknown
	// format byte, a tree entry of an unknown file type, or a config, key
	// file, manifest or lock with a JSON key the build does not know. Such
	// an object is never skipped and never reported as corrupt; a later
	// build wrote it.
	ErrUnsupportedFormat = errors.New("ark: unsupported repository format")

	// ErrWrongPassword is returned by Open when no key file of the
	// repository opens with the given password.
	ErrWrongPassword = errors.New("ark: wrong password or no key file matches")

	// ErrPasswordRequired is returned by Open when NoPassword is given
	// for a password protected repository.
	ErrPasswordRequired = errors.New("ark: repository is password protected")

	// ErrNotPasswordProtected is returned by Open when a password is given
	// for a repository created with NoPassword.
	ErrNotPasswordProtected = errors.New("ark: repository is not password protected")
)

// missingError is the error of a blob that is not stored. A read of one
// wrapped the backend's ErrNotFound before the two had errors of their own,
// so it still matches that.
type missingError string

func (e missingError) Error() string { return string(e) }

func (missingError) Is(target error) bool { return target == ErrNotFound }
