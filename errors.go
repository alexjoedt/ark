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

	// ErrIncompatibleSourceType is returned when restoring a snapshot into a
	// Sink type that is incompatible with the snapshot's SourceType.
	ErrIncompatibleSourceType = errors.New("ark: incompatible source type")

	// ErrCorruptManifest is returned when a snapshot manifest does not
	// authenticate: it was changed, truncated or moved from another snapshot.
	// Snapshots, Prune and a Forget policy fail while one exists; remove it
	// by naming its id to Forget without a policy, then Prune.
	ErrCorruptManifest = errors.New("ark: corrupt snapshot manifest")

	// ErrCorruptTree is returned by Verify when a tree object of the snapshot
	// does not authenticate or does not match its name.
	ErrCorruptTree = errors.New("ark: corrupt snapshot tree")

	// ErrCorruptChunk is returned when a stored chunk fails its integrity check.
	ErrCorruptChunk = errors.New("ark: corrupt chunk")

	// ErrMissingChunk is returned when a required chunk is absent from the CAS.
	ErrMissingChunk = errors.New("ark: missing chunk")

	// ErrRepoLocked is returned when a repo-level lock cannot be acquired.
	ErrRepoLocked = errors.New("ark: repository is locked by another operation")

	// ErrNotInitialized is returned by Open when the backend holds no
	// repository config. Call Init first.
	ErrNotInitialized = errors.New("ark: repository is not initialized")

	// ErrAlreadyInitialized is returned by Init when the backend already
	// holds a repository config.
	ErrAlreadyInitialized = errors.New("ark: repository is already initialized")

	// ErrUnsupportedFormat is returned by Open when the repository config
	// names a format this build does not read.
	ErrUnsupportedFormat = errors.New("ark: unsupported repository format")

	// ErrWrongPassword is returned by Open when no key file of the
	// repository opens with the given password.
	ErrWrongPassword = errors.New("ark: wrong password or no key file matches")

	// ErrPasswordRequired is returned by Open when WithoutPassword is given
	// for a password protected repository.
	ErrPasswordRequired = errors.New("ark: repository is password protected")

	// ErrNotPasswordProtected is returned by Open when a password is given
	// for a repository created with WithoutPassword.
	ErrNotPasswordProtected = errors.New("ark: repository is not password protected")
)
