package ark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// treeFixture is a three level directory tree of 84 directories with a file
// in each, plus an entry without its parents and a directory held by a file.
func treeFixture() []fileEntry {
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := func(path string, typ FileType) fileEntry {
		e := fileEntry{FileInfo: FileInfo{Path: path, Type: typ, Mode: 0o644, ModTime: mtime}}
		if typ == FileTypeRegular {
			e.Size = int64(len(path))
			e.Chunks = []chunkRef{{ID: "chunk-" + path, Size: len(path)}}
		}
		return e
	}
	var entries []fileEntry
	var add func(dir string, depth int)
	add = func(dir string, depth int) {
		entries = append(entries, entry(dir+"/file", FileTypeRegular))
		if depth == 3 {
			return
		}
		for i := range 4 {
			sub := fmt.Sprintf("%s/d%d", dir, i)
			entries = append(entries, entry(sub, FileTypeDirectory))
			add(sub, depth+1)
		}
	}
	add(".", 0)
	return append(entries,
		entry("x/y/z.txt", FileTypeRegular),
		entry("f", FileTypeRegular),
		entry("f/g", FileTypeDirectory),
		entry("f/g/h", FileTypeRegular),
	)
}

// buildTreeSequential is the depth-first build buildTree replaced: each node
// is stored right after its children, one at a time.
func buildTreeSequential(ctx context.Context, trees *objectStore, entries []fileEntry) (string, error) {
	dirChildren := treeChildren(entries)
	hashes := make(map[string]string)
	var build func(dir string) (string, error)
	build = func(dir string) (string, error) {
		node := &treeNode{}
		for _, child := range dirChildren[dir] {
			if child.Type == FileTypeDirectory {
				norm := normEntryPath(child.Path)
				hash, err := build(norm)
				if err != nil {
					return "", err
				}
				hashes[norm] = hash
			}
			node.Entries = append(node.Entries, newTreeEntry(child, hashes))
		}
		return storeTreeNode(ctx, trees, node)
	}
	return build("")
}

func TestBuildTreeMatchesSequential(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")

	seqBackend, parBackend := newStubBackend(), newStubBackend()
	want, err := buildTreeSequential(ctx, newObjectStore(seqBackend, TreeFile, codec, key), treeFixture())
	if err != nil {
		t.Fatalf("sequential build: %v", err)
	}
	for _, workers := range []int{1, 8} {
		trees := newObjectStore(parBackend, TreeFile, codec, key)
		got, err := buildTree(ctx, trees, treeFixture(), workers)
		if err != nil {
			t.Fatalf("workers %d: %v", workers, err)
		}
		if got != want {
			t.Errorf("workers %d: root %s, sequential root %s", workers, got, want)
		}
	}
	// Same objects, so a check of either repository reports the same.
	seq, par := slices.Sorted(maps.Keys(seqBackend.objects)), slices.Sorted(maps.Keys(parBackend.objects))
	if !slices.Equal(seq, par) {
		t.Errorf("stored %d tree nodes, sequential build stored %d", len(par), len(seq))
	}
	// 84 fixture directories, x, x/y and f/g, plus the root. f/g is held by
	// a file and is not reachable, so it is not stored.
	if len(par) != 87 {
		t.Errorf("stored %d tree nodes, want 87", len(par))
	}
}

// gatedBackend counts tree Puts in flight and runs hook on each before it
// stores.
type gatedBackend struct {
	*stubBackend
	inFlight, calls atomic.Int32
	hook            func(call int32) error

	mu   sync.Mutex
	peak int32
}

func (b *gatedBackend) Put(ctx context.Context, h Handle, r io.Reader) error {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	b.mu.Lock()
	b.peak = max(b.peak, n)
	b.mu.Unlock()
	if err := b.hook(b.calls.Add(1)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.stubBackend.Put(ctx, h, r)
}

func TestBuildTreeStopsOnFirstError(t *testing.T) {
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	errPut := errors.New("put failed")

	for _, tc := range []struct {
		name string
		fail func(cancel context.CancelFunc) error
		want error
	}{
		{"put error", func(context.CancelFunc) error { return errPut }, errPut},
		{"cancel", func(cancel context.CancelFunc) error { cancel(); return nil }, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var once sync.Once
			b := &gatedBackend{stubBackend: newStubBackend()}
			b.hook = func(call int32) error {
				// Fail in the middle of the deepest level, while other Puts
				// are running.
				var err error
				if call == 20 {
					once.Do(func() { err = tc.fail(cancel) })
				}
				time.Sleep(time.Millisecond)
				return err
			}

			_, err := buildTree(ctx, newObjectStore(b, TreeFile, codec, key), treeFixture(), 4)
			if !errors.Is(err, tc.want) {
				t.Fatalf("buildTree returned %v, want %v", err, tc.want)
			}
			if n := b.inFlight.Load(); n != 0 {
				t.Errorf("%d Puts still running after return", n)
			}
			calls := b.calls.Load()
			time.Sleep(10 * time.Millisecond)
			if b.calls.Load() != calls {
				t.Error("Puts started after buildTree returned")
			}
			// The failing call, the 3 others in flight and one scheduled
			// as it failed; the deepest level alone has 64 nodes.
			if calls > 28 {
				t.Errorf("%d Puts ran, the error did not stop the rest", calls)
			}
			if p := b.peak; p > 4 {
				t.Errorf("%d Puts ran at once, limit 4", p)
			}
		})
	}
}
