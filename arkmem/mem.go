// Package arkmem implements ark.Backend in memory.
//
// Objects live in a map keyed by "<namespace>/<name>", where the namespace is
// ark.HandleType.String(). Nothing is persisted; a Backend starts empty and is
// discarded with its process. It exists for unit tests and examples that need
// a repository without a filesystem.
package arkmem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/alexjoedt/ark"
)

// Compile-time check that *Backend satisfies ark.Backend.
var _ ark.Backend = (*Backend)(nil)

// Backend implements ark.Backend backed by a map. It is safe for concurrent
// use; the zero value is not, use New.
type Backend struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// New creates an empty in-memory backend.
func New() *Backend {
	return &Backend{objects: make(map[string][]byte)}
}

// key maps a handle to its map key. Names are opaque object names, never
// paths, so the same names arklocal rejects are rejected here.
func key(h ark.Handle) (string, error) {
	ns := h.Type.String()
	if ns == ark.UnknownNamespace {
		return "", fmt.Errorf("arkmem: unknown handle type %d", h.Type)
	}
	if h.Name == "" {
		return "", fmt.Errorf("arkmem: invalid object name %q", h.Name)
	}
	return ns + "/" + h.Name, nil
}

// Put reads r fully and stores it under h, replacing any existing object.
// The store is only updated once the read succeeded, so a failed Put leaves
// the previous object in place and no reader observes a partial one.
func (b *Backend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := key(h)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("arkmem: read %s: %w", h.Name, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[k] = data
	return nil
}

// Get returns a reader over a copy of the object stored under h.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k, err := key(h)
	if err != nil {
		return nil, err
	}

	b.mu.RLock()
	data, ok := b.objects[k]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}

// Exists reports whether the object identified by h is present.
func (b *Backend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	k, err := key(h)
	if err != nil {
		return false, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.objects[k]
	return ok, nil
}

// Delete removes the object identified by h. A missing object is not an error.
func (b *Backend) Delete(ctx context.Context, h ark.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := key(h)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, k)
	return nil
}

// List calls fn for every object in the namespace of t.
// An empty namespace is not an error.
func (b *Backend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ns := t.String()
	if ns == ark.UnknownNamespace {
		return fmt.Errorf("arkmem: unknown handle type %d", t)
	}
	prefix := ns + "/"

	// Snapshot the namespace so fn may call back into the backend.
	b.mu.RLock()
	infos := make([]ark.BlobInfo, 0, len(b.objects))
	for k, data := range b.objects {
		name, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		infos = append(infos, ark.BlobInfo{Name: name, Size: int64(len(data))})
	}
	b.mu.RUnlock()

	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

// Stat returns metadata for the object stored under h.
func (b *Backend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return ark.BlobInfo{}, err
	}
	k, err := key(h)
	if err != nil {
		return ark.BlobInfo{}, err
	}

	b.mu.RLock()
	data, ok := b.objects[k]
	b.mu.RUnlock()
	if !ok {
		return ark.BlobInfo{}, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
	}
	return ark.BlobInfo{Name: h.Name, Size: int64(len(data))}, nil
}

// Close drops every stored object. The backend stays usable and is empty
// afterwards.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects = make(map[string][]byte)
	return nil
}
