package ark

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
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

// TestTreeNodeStoresNoSizeAndNoOffset pins the node format: an entry holds no
// size and a chunk reference is id and length, and a loaded node derives
// both from the lengths.
func TestTreeNodeStoresNoSizeAndNoOffset(t *testing.T) {
	entries := []fileEntry{{
		FileInfo: FileInfo{Path: "f", Type: FileTypeRegular, Size: 7},
		Chunks:   []chunkRef{{ID: "c", Length: 3}, {ID: "d", Length: 4, Offset: 3}},
	}}
	data, err := marshalTreeNode(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Entries[0]["size"]; ok {
		t.Errorf("a stored entry has a size: %s", data)
	}
	for _, c := range raw.Entries[0]["chunks"].([]any) {
		if got := slices.Sorted(maps.Keys(c.(map[string]any))); !slices.Equal(got, []string{"id", "length"}) {
			t.Errorf("chunk ref keys %v, want [id length]", got)
		}
	}

	var node treeNode
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatal(err)
	}
	e := &node.Entries[0]
	if err := fillChunkOffsets(e); err != nil {
		t.Fatal(err)
	}
	if e.Size != 7 || e.Chunks[0].Offset != 0 || e.Chunks[1].Offset != 3 {
		t.Errorf("size %d, offsets %d and %d, want 7, 0 and 3", e.Size, e.Chunks[0].Offset, e.Chunks[1].Offset)
	}
	bad := treeEntry{Chunks: []chunkRef{{ID: "c", Length: math.MaxInt64}, {ID: "d", Length: 1}}}
	if err := fillChunkOffsets(&bad); err == nil {
		t.Error("lengths past int64 were accepted")
	}
}

// TestTreeNodeModeIsModeTAnd07777 pins the meaning of mode in a node: the
// permission bits with setuid 04000, setgid 02000 and sticky 01000, whatever
// else the source left in FileInfo.Mode, and the same bits in Go's form on
// the way back out.
func TestTreeNodeModeIsModeTAnd07777(t *testing.T) {
	for _, tc := range []struct {
		in   fs.FileMode
		want uint32
		back fs.FileMode
	}{
		{0o644, 0o644, 0o644},
		{fs.ModeSetuid | 0o755, 0o4755, fs.ModeSetuid | 0o755},
		{fs.ModeSetgid | 0o775, 0o2775, fs.ModeSetgid | 0o775},
		{fs.ModeSticky | 0o777, 0o1777, fs.ModeSticky | 0o777},
		{fs.ModeDir | fs.ModeSetgid | 0o775, 0o2775, fs.ModeSetgid | 0o775},
		{fs.ModeSymlink | 0o777, 0o777, 0o777},
		{fs.ModeDevice | fs.ModeCharDevice | fs.ModeAppend | fs.ModeTemporary | 0o600, 0o600, 0o600},
	} {
		typ := FileTypeRegular
		if tc.in.IsDir() {
			typ = FileTypeDirectory
		}
		entry := fileEntry{Path: "e", Type: typ, Mode: tc.in}
		te, err := newTreeEntry(entry, map[string]string{"e": "hash"})
		if err != nil {
			t.Fatal(err)
		}
		if te.Mode != tc.want {
			t.Errorf("mode %v is stored as %#o, want %#o", tc.in, te.Mode, tc.want)
		}
		if got := te.fileInfo("e").Mode; got != tc.back {
			t.Errorf("stored mode %#o reads as %v, want %v", te.Mode, got, tc.back)
		}
	}
}

// TestLoadTreeNodeRefusesAModeOutside07777: a type bit or any other bit above
// 07777 in a stored mode is not something this format writes.
func TestLoadTreeNodeRefusesAModeOutside07777(t *testing.T) {
	ctx := context.Background()
	codec := newObjectCodec()
	defer codec.Close()
	session := testSession(newStubBackend(), codec, newMasterKey("repo"))

	wantErr := map[uint32]bool{0o7777: false, 0o10000: true, uint32(fs.ModeDir) | 0o755: true}
	hashes := make(map[uint32]string)
	for mode := range wantErr {
		data, err := json.Marshal(treeNode{Entries: []treeEntry{{Name: "e", Type: FileTypeRegular, Mode: mode}}})
		if err != nil {
			t.Fatal(err)
		}
		hashes[mode] = session.trees.name(data)
		if _, err := session.trees.putNamed(ctx, hashes[mode], data); err != nil {
			t.Fatalf("putNamed: %v", err)
		}
	}
	if err := session.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	for mode, want := range wantErr {
		_, err := loadTreeNode(ctx, session.trees, hashes[mode])
		if want != errors.Is(err, ErrCorruptTree) || (!want && err != nil) {
			t.Errorf("mode %#o: loadTreeNode returned %v, want ErrCorruptTree: %v", mode, err, want)
		}
	}
}

func TestLinkTargetRefusesWhatIsNotAFile(t *testing.T) {
	t.Parallel()
	nodes := map[string]*treeNode{
		"root": {Entries: []treeEntry{
			{Name: "dir", Type: FileTypeDirectory, TreeHash: "dir"},
			{Name: "file", Type: FileTypeRegular, Chunks: []chunkRef{{ID: "c", Length: 4}}, Size: 4},
			{Name: "sym", Type: FileTypeSymlink},
		}},
		"dir": {},
	}
	load := func(hash string) (*treeNode, error) {
		if n, ok := nodes[hash]; ok {
			return n, nil
		}
		return nil, fmt.Errorf("get %s: %w", hash, os.ErrNotExist)
	}
	if size, err := linkSize(load, "root", "file"); err != nil || size != 4 {
		t.Errorf("linkSize(file) = %d, %v, want 4", size, err)
	}
	for _, link := range []string{"missing", "dir/missing", "sym", "dir"} {
		if _, err := linkSize(load, "root", link); !errors.Is(err, ErrCorruptTree) {
			t.Errorf("linkSize(%q) = %v, want ErrCorruptTree", link, err)
		}
	}
	if _, err := linkSize(load, "gone", "file"); errors.Is(err, ErrCorruptTree) || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a node that does not load = %v, want its own error", err)
	}
}
