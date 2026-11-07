package ark

import "context"

// OpenObject returns the raw content of an object of r for the tests in
// package ark_test, which cannot unseal an object themselves. A chunk or tree
// also fails when its content does not hash to name.
func OpenObject(ctx context.Context, r *Repository, typ HandleType, name string) ([]byte, error) {
	//nolint:exhaustive // the other handle types share the sealed path
	switch typ {
	case BlobFile:
		return r.chunks.verifying().Get(ctx, name)
	case TreeFile:
		return r.trees.verifying().Get(ctx, name)
	}
	return r.getSealed(ctx, Handle{Type: typ, Name: name})
}

// PutObject seals data and stores it under h, the way r writes manifests and
// locks.
func PutObject(ctx context.Context, r *Repository, h Handle, data []byte) error {
	return r.putSealed(ctx, h, data)
}

// UnsealObject opens obj as r would read it back from h.
func UnsealObject(r *Repository, h Handle, obj []byte) ([]byte, error) {
	return r.codec.open((*[32]byte)(&r.key.DataKey), objectAAD(h), obj)
}

// ObjectName returns the name r stores a chunk or tree of this content under.
func ObjectName(r *Repository, data []byte) string { return r.chunks.name(data) }

// SetLockEvent installs f as the lock event hook of this process, see
// lockEvent. Set it before the first lock is taken.
func SetLockEvent(f func(event string, exclusive bool)) { lockEvent = f }

// The lock events SetLockEvent reports.
const (
	LockAcquired = lockAcquired
	LockRejected = lockRejected
	LockReleased = lockReleased
	LockLeftOver = lockLeftOver
)

// RootTree returns the root tree hash of snapshot id.
func RootTree(ctx context.Context, r *Repository, id string) (string, error) {
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return "", err
	}
	return m.RootTree, nil
}

// SetPrefetchBytes lowers the floor of the restore prefetch budget of r for
// a test.
func SetPrefetchBytes(r *Repository, n int64) { r.prefetchBytes = n }
