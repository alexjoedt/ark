package ark

import (
	"context"
	"encoding/json"
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
// in each, four empty directories whose nodes are byte-identical, an entry
// without its parents and a directory held by a file.
func treeFixture() []fileEntry {
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := func(path string, typ FileType) fileEntry {
		e := fileEntry{FileInfo: FileInfo{Path: path, Type: typ, Mode: 0o644, ModTime: mtime}}
		if typ == FileTypeRegular {
			e.Size = int64(len(path))
			e.Chunks = []chunkRef{{ID: "chunk-" + path, Length: int64(len(path))}}
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
	for i := range 4 {
		entries = append(entries, entry(fmt.Sprintf("e%d", i), FileTypeDirectory))
	}
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
		for _, child := range dirChildren[dir] {
			if child.Type == FileTypeDirectory {
				norm := normEntryPath(child.Path)
				hash, err := build(norm)
				if err != nil {
					return "", err
				}
				hashes[norm] = hash
			}
		}
		data, err := marshalTreeNode(dirChildren[dir], hashes)
		if err != nil {
			return "", err
		}
		hash, _, err := trees.Put(ctx, data)
		return hash, err
	}
	return build("")
}

// 84 fixture directories, one node for the 4 empty ones, x, x/y and f/g,
// plus the root. f/g is held by a file and is not reachable, so it is not
// stored.
const treeFixtureNodes = 88

func TestBuildTreeMatchesSequential(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")

	seqBackend := newStubBackend()
	want, err := buildTreeSequential(ctx, newObjectStore(seqBackend, TreeFile, codec, key), treeFixture())
	if err != nil {
		t.Fatalf("sequential build: %v", err)
	}
	seq := slices.Sorted(maps.Keys(seqBackend.objects))
	if len(seq) != treeFixtureNodes {
		t.Fatalf("sequential build stored %d tree nodes, want %d", len(seq), treeFixtureNodes)
	}

	for _, workers := range []int{1, 8} {
		b := &gatedBackend{stubBackend: newStubBackend(), hook: func(context.Context, int32) error { return nil }}
		got, err := buildTree(ctx, newObjectStore(b, TreeFile, codec, key), treeFixture(), workers)
		if err != nil {
			t.Fatalf("workers %d: %v", workers, err)
		}
		if got != want {
			t.Errorf("workers %d: root %s, sequential root %s", workers, got, want)
		}
		// Same objects, so a check of either repository reports the same.
		if par := slices.Sorted(maps.Keys(b.objects)); !slices.Equal(seq, par) {
			t.Errorf("workers %d: stored %d tree nodes, sequential build stored %d", workers, len(par), len(seq))
		}
		// Identical nodes in one level are stored once, not Put at the same
		// time to the same handle.
		if calls := b.calls.Load(); calls != treeFixtureNodes {
			t.Errorf("workers %d: %d Puts for %d distinct nodes", workers, calls, treeFixtureNodes)
		}
	}
}

// gatedBackend counts tree Puts in flight and runs hook on each before it
// stores.
type gatedBackend struct {
	*stubBackend
	inFlight, calls atomic.Int32
	hook            func(ctx context.Context, call int32) error

	mu   sync.Mutex
	peak int32
}

func (b *gatedBackend) Put(ctx context.Context, h Handle, r io.Reader) error {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	b.mu.Lock()
	b.peak = max(b.peak, n)
	b.mu.Unlock()
	if err := b.hook(ctx, b.calls.Add(1)); err != nil {
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
			b := &gatedBackend{stubBackend: newStubBackend()}
			b.hook = func(ctx context.Context, call int32) error {
				if call == 2 {
					return tc.fail(cancel)
				}
				// Every other Put stays in flight until buildTree cancels
				// it, so the count below does not depend on timing.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
					return errors.New("Put was not cancelled")
				}
			}

			_, err := buildTree(ctx, newObjectStore(b, TreeFile, codec, key), treeFixture(), 4)
			if !errors.Is(err, tc.want) {
				t.Fatalf("buildTree returned %v, want %v", err, tc.want)
			}
			if n := b.inFlight.Load(); n != 0 {
				t.Errorf("%d Puts still running after return", n)
			}
			// The 4 workers, plus the one put that may already be waiting
			// for a slot when the error lands; the deepest level alone has
			// 64 nodes.
			if calls := b.calls.Load(); calls > 5 {
				t.Errorf("%d Puts ran, the error did not stop the rest", calls)
			}
			if p := b.peak; p > 4 {
				t.Errorf("%d Puts ran at once, limit 4", p)
			}
		})
	}
}

// TestTreeNodeChunkRefsCarryNoStoredSize pins the node format: a chunk
// reference is id, length and offset, so a node of unchanged files is
// byte-identical to the one the parent snapshot stored. Nodes older builds
// wrote with a size per reference still load.
func TestTreeNodeChunkRefsCarryNoStoredSize(t *testing.T) {
	entries := []fileEntry{{
		FileInfo: FileInfo{Path: "f", Type: FileTypeRegular, Size: 3},
		Chunks:   []chunkRef{{ID: "c", Length: 3, Offset: 0}},
	}}
	data, err := marshalTreeNode(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Entries []struct {
			Chunks []map[string]any `json:"chunks"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(raw.Entries[0].Chunks[0]))
	if want := []string{"id", "length", "offset"}; !slices.Equal(got, want) {
		t.Errorf("chunk ref keys %v, want %v", got, want)
	}

	legacy := `{"entries":[{"name":"f","type":"regular","mode":420,"uid":0,"gid":0,"mtime":"2026-01-02T03:04:05Z",` +
		`"size":3,"chunks":[{"id":"c","size":40,"length":3,"offset":0}]}]}`
	var node treeNode
	if err := json.Unmarshal([]byte(legacy), &node); err != nil {
		t.Fatalf("legacy node: %v", err)
	}
	if len(node.Entries) != 1 || len(node.Entries[0].Chunks) != 1 || node.Entries[0].Chunks[0].ID != "c" {
		t.Errorf("legacy node loaded as %+v", node)
	}
}
