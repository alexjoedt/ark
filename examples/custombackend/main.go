// Command custombackend keeps a repository somewhere ark has no backend for.
// It implements ark.Backend over a key-value store, the shape an object store
// such as S3 has: flat keys, whole-object writes, ranged reads. The store here
// is a map; a real backend calls the service's client in the same places.
//
//	go run ./custombackend
//	go test ./custombackend    # the conformance suite of package arktest
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/alexjoedt/ark"
)

var _ ark.Backend = (*kvBackend)(nil)

// kvBackend stores every object under the key "<namespace>/<name>". All
// methods are safe for concurrent use, which ark.Backend requires.
type kvBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func newKVBackend() *kvBackend {
	return &kvBackend{objects: make(map[string][]byte)}
}

// key maps a handle to its key. The namespace comes from Type.String() and not
// from a switch on the constants, so a handle type that a later version of ark
// adds needs no change here. ark.CheckName refuses a name that is no object
// name, such as one with a "/" that would reach into another key.
func key(h ark.Handle) (string, error) {
	ns := h.Type.String()
	if ns == ark.UnknownNamespace {
		return "", fmt.Errorf("kv: unknown handle type %d", h.Type)
	}
	if err := ark.CheckName(h.Name); err != nil {
		return "", fmt.Errorf("kv: %s: %w", ns, err)
	}
	return ns + "/" + h.Name, nil
}

// Put replaces the object in one step: a reader sees the old object or the
// new one, never a part of it.
func (b *kvBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := key(h)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("kv: read %s: %w", k, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[k] = data
	return nil
}

// Get returns the whole object. A missing object wraps [ark.ErrNotFound].
func (b *kvBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	data, err := b.load(ctx, h)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// GetRange returns exactly length bytes from offset. A range that ends past
// the object is an error wrapping [io.ErrUnexpectedEOF], never a short result.
func (b *kvBackend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("kv: invalid range %d+%d of %s", offset, length, h.Name)
	}
	data, err := b.load(ctx, h)
	if err != nil {
		return nil, err
	}
	if offset > int64(len(data)) || length > int64(len(data))-offset {
		return nil, fmt.Errorf("kv: range %d+%d of %s (%d bytes): %w",
			offset, length, h.Name, len(data), io.ErrUnexpectedEOF)
	}
	return bytes.Clone(data[offset : offset+length]), nil
}

func (b *kvBackend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
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

// Delete removes the object. A missing object is not an error.
func (b *kvBackend) Delete(ctx context.Context, h ark.Handle) error {
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

// Sync is the durability barrier: when it returns, a power loss cannot undo
// an earlier Put or Delete. A store that makes every write durable on its own,
// as most object stores do, has nothing left to do here.
func (b *kvBackend) Sync(ctx context.Context) error {
	return ctx.Err()
}

// List calls fn for every object of one namespace. It collects the names
// first, so fn may call back into the backend.
func (b *kvBackend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	ns := t.String()
	if ns == ark.UnknownNamespace {
		return fmt.Errorf("kv: unknown handle type %d", t)
	}
	b.mu.RLock()
	var infos []ark.BlobInfo
	for k, data := range b.objects {
		if name, ok := strings.CutPrefix(k, ns+"/"); ok {
			infos = append(infos, ark.BlobInfo{Name: name, Size: int64(len(data))})
		}
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

func (b *kvBackend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	data, err := b.load(ctx, h)
	if err != nil {
		return ark.BlobInfo{}, err
	}
	return ark.BlobInfo{Name: h.Name, Size: int64(len(data))}, nil
}

func (b *kvBackend) Close() error {
	return nil
}

func (b *kvBackend) load(ctx context.Context, h ark.Handle) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k, err := key(h)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	data, ok := b.objects[k]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ark.ErrNotFound, k)
	}
	return data, nil
}

func main() {
	ctx := context.Background()

	b := newKVBackend()
	pw := ark.NewPassword("correct horse battery staple")
	if err := ark.Init(ctx, b, pw); err != nil {
		log.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, pw)
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()

	snap, err := repo.Backup(ctx, ark.NewStreamSource("greeting", strings.NewReader("hello from a custom backend\n")))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := repo.Restore(ctx, snap.ID, ark.NewStreamSink(os.Stdout)); err != nil {
		log.Fatal(err)
	}

	// The backend holds sealed objects under opaque names and nothing else.
	fmt.Println("\nwhat the backend stores:")
	for _, t := range []ark.HandleType{
		ark.ConfigFile, ark.KeyFile, ark.SnapshotFile,
		ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
	} {
		var n int
		var size int64
		err := b.List(ctx, t, func(info ark.BlobInfo) error {
			n++
			size += info.Size
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%-10s %d objects, %4d bytes\n", t, n, size)
	}
}
