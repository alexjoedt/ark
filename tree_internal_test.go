package ark

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
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
		hash := trees.name(data)
		_, err = trees.putNamed(ctx, hash, data)
		return hash, err
	}
	return build("")
}

// 84 fixture directories, one node for the 4 empty ones, x, x/y and f/g,
// plus the root. f/g is held by a file and is not reachable, so it is not
// stored.
const treeFixtureNodes = 88

// storedNodes finishes session and returns the name of every tree node in
// the tree packs of b, one per copy.
func storedNodes(t *testing.T, b *stubBackend, session *packSession) []string {
	t.Helper()
	if err := session.finish(context.Background()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var names []string
	for key, pack := range b.objects {
		if !strings.HasPrefix(key, TreePackFile.String()+"/") {
			continue
		}
		entries, err := session.trees.space.parse(pack)
		if err != nil {
			t.Fatalf("parse %s: %v", key, err)
		}
		for _, e := range entries {
			names = append(names, hex.EncodeToString(e.name[:]))
		}
	}
	slices.Sort(names)
	return names
}

func TestBuildTreeMatchesSequential(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")

	seqBackend := newStubBackend()
	session := testSession(seqBackend, codec, key)
	want, err := buildTreeSequential(ctx, session.trees, treeFixture())
	if err != nil {
		t.Fatalf("sequential build: %v", err)
	}
	seq := storedNodes(t, seqBackend, session)
	if len(seq) != treeFixtureNodes {
		t.Fatalf("sequential build stored %d tree nodes, want %d", len(seq), treeFixtureNodes)
	}

	for _, workers := range []int{1, 8} {
		b := newStubBackend()
		session := testSession(b, codec, key)
		got, err := buildTree(ctx, session.trees, treeFixture(), workers)
		if err != nil {
			t.Fatalf("workers %d: %v", workers, err)
		}
		if got != want {
			t.Errorf("workers %d: root %s, sequential root %s", workers, got, want)
		}
		// Same nodes, so a check of either repository reports the same, and
		// each once: identical nodes of one level or of two go into a pack
		// one time.
		if par := storedNodes(t, b, session); !slices.Equal(seq, par) {
			t.Errorf("workers %d: stored %d tree nodes, sequential build stored %d", workers, len(par), len(seq))
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

// failListBackend fails every List, which is the first request a tree build
// issues: the lookup of its first node loads the index.
type failListBackend struct {
	Backend
	err error
}

func (b failListBackend) List(context.Context, HandleType, func(BlobInfo) error) error { return b.err }

func TestBuildTreeStopsOnFirstError(t *testing.T) {
	codec := newObjectCodec()
	defer codec.Close()
	key := newMasterKey("repo")
	errList := errors.New("list failed")

	t.Run("backend error", func(t *testing.T) {
		b := failListBackend{Backend: newStubBackend(), err: errList}
		_, err := buildTree(context.Background(), testSession(b, codec, key).trees, treeFixture(), 4)
		if !errors.Is(err, errList) {
			t.Fatalf("buildTree returned %v, want %v", err, errList)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := newIndexBackend(newStubBackend())
		b.onList = func(HandleType) { cancel() }
		_, err := buildTree(ctx, testSession(b, codec, key).trees, treeFixture(), 4)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("buildTree returned %v, want %v", err, context.Canceled)
		}
		if _, _, total := b.calls(TreePackFile); total != 1 {
			t.Errorf("%d requests after the cancel, want the one List", total)
		}
	})
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
