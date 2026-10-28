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
