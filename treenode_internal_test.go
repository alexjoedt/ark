package ark

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// nodeID is a chunk id or tree hash of one repeated byte.
func nodeID(b byte) string { return strings.Repeat(fmt.Sprintf("%02x", b), 32) }

var (
	nodeMTime = time.Unix(1_700_000_000, 123_456_789).UTC()
	nodeCTime = time.Unix(1_700_000_001, 987_654_321).UTC()
)

// nodeEntries is one entry of every file type a node holds, as the decoder
// returns it, plus a file without content.
func nodeEntries() []treeEntry {
	base := func(name string, typ FileType) treeEntry {
		return treeEntry{Name: name, Type: typ, Mode: 0o644, UID: 1000, GID: 100, ModTime: nodeMTime, Inode: 4711}
	}
	file := base("file", FileTypeRegular)
	file.Size = 305
	file.Chunks = []chunkRef{{ID: nodeID(0x11), Length: 5}, {ID: nodeID(0x22), Length: 300, Offset: 5}}
	empty := base("empty", FileTypeRegular)
	dir := base("dir", FileTypeDirectory)
	dir.TreeHash = nodeID(0xab)
	symlink := base("caf\xe9", FileTypeSymlink)
	symlink.LinkDest = "../a/\xff"
	hardlink := base("hardlink", FileTypeHardlink)
	hardlink.LinkDest = "dir/file"
	chardev := base("chardev", fileTypeCharDevice)
	chardev.DevMajor, chardev.DevMinor = 1, 3
	blockdev := base("blockdev", fileTypeBlockDevice)
	blockdev.DevMajor, blockdev.DevMinor = 259, 1<<20
	return []treeEntry{file, empty, dir, symlink, hardlink, base("fifo", fileTypeFifo), chardev, blockdev}
}

// fullNode holds every entry of nodeEntries plain and with ctime and xattrs.
func fullNode() *treeNode {
	node := &treeNode{}
	for _, e := range nodeEntries() {
		with := e
		with.Name += ".meta"
		with.ChangeTime = nodeCTime
		with.Xattrs = map[string][]byte{"user.b": []byte("v"), "user.a\xfe": {}, "security.x": {0, 1, 2}}
		node.Entries = append(node.Entries, e, with)
	}
	slices.SortFunc(node.Entries, func(a, b treeEntry) int { return strings.Compare(a.Name, b.Name) })
	return node
}

func mustEncodeNode(t testing.TB, node *treeNode) []byte {
	t.Helper()
	b, err := encodeTreeNode(node)
	if err != nil {
		t.Fatalf("encodeTreeNode: %v", err)
	}
	return b
}

func TestTreeNodeRoundTrips(t *testing.T) {
	t.Parallel()
	cases := map[string]*treeNode{"empty node": {Entries: []treeEntry{}}, "every entry": fullNode()}
	for _, e := range nodeEntries() {
		cases[string(e.Type)+" "+e.Name] = &treeNode{Entries: []treeEntry{e}}
		ctime, xattrs, both := e, e, e
		ctime.ChangeTime = nodeCTime
		xattrs.Xattrs = map[string][]byte{"user.k": []byte("value")}
		both.ChangeTime, both.Xattrs = nodeCTime, xattrs.Xattrs
		cases[string(e.Type)+" "+e.Name+" with ctime"] = &treeNode{Entries: []treeEntry{ctime}}
		cases[string(e.Type)+" "+e.Name+" with xattrs"] = &treeNode{Entries: []treeEntry{xattrs}}
		cases[string(e.Type)+" "+e.Name+" with both"] = &treeNode{Entries: []treeEntry{both}}
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stored := mustEncodeNode(t, node)
			got, err := decodeTreeNode(stored)
			if err != nil {
				t.Fatalf("decodeTreeNode: %v", err)
			}
			if !reflect.DeepEqual(got, node) {
				t.Errorf("decoded node differs:\n got %+v\nwant %+v", got, node)
			}
			if again := mustEncodeNode(t, got); !bytes.Equal(again, stored) {
				t.Errorf("the decoded node encodes to other bytes:\n got %x\nwant %x", again, stored)
			}
		})
	}
}

// TestTreeNodeLayout pins the stored bytes against a node written out by
// hand, so a change to the layout fails even when encoder and decoder agree.
func TestTreeNodeLayout(t *testing.T) {
	t.Parallel()
	stored := unhex(t, `
		01 02
		01 61  01 03  ed 09  e8 07  00
		08 07 06 05 04 03 02 01
		ff ff ff ff ff ff ff ff
		ac 02
		01  1111111111111111111111111111111111111111111111111111111111111111  80 80 04
		01  06 75 73 65 72 2e 6b  01 76
		01 62  06 00  80 01  00  00
		00 00 00 00 00 00 00 00
		00
		01 03`)
	node := &treeNode{Entries: []treeEntry{
		{
			Name: "a", Type: FileTypeRegular, Mode: 0o4755, UID: 1000,
			ModTime: time.Unix(0, 0x0102030405060708).UTC(), ChangeTime: time.Unix(0, -1).UTC(), Inode: 300,
			Size: 65536, Chunks: []chunkRef{{ID: nodeID(0x11), Length: 65536}},
			Xattrs: map[string][]byte{"user.k": []byte("v")},
		},
		{Name: "b", Type: fileTypeCharDevice, Mode: 0o600, ModTime: time.Unix(0, 0).UTC(), DevMajor: 1, DevMinor: 3},
	}}
	if got := mustEncodeNode(t, node); !bytes.Equal(got, stored) {
		t.Errorf("encoded:\n got %x\nwant %x", got, stored)
	}
	got, err := decodeTreeNode(stored)
	if err != nil {
		t.Fatalf("decodeTreeNode: %v", err)
	}
	if !reflect.DeepEqual(got, node) {
		t.Errorf("decoded:\n got %+v\nwant %+v", got, node)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// raw builds node bytes by hand, with none of the encoder's checks.
type raw []byte

func (b raw) uv(v uint64) raw   { return binary.AppendUvarint(b, v) }
func (b raw) str(s string) raw  { return append(b.uv(uint64(len(s))), s...) }
func (b raw) add(p ...byte) raw { return append(b, p...) }
func (b raw) id() raw           { return append(b, make([]byte, 32)...) }

// rawHead is an entry up to and including its mtime.
func rawHead(name string, typ, flags byte, mode uint16, uid, gid uint64) raw {
	b := binary.LittleEndian.AppendUint16(raw{}.str(name).add(typ, flags), mode)
	return binary.LittleEndian.AppendUint64(raw(b).uv(uid).uv(gid), 0)
}

// rawEntry is an entry of a type and flags with mode 0644, no owner and no
// inode: what follows is the part its type and flags ask for.
func rawEntry(name string, typ, flags byte) raw { return rawHead(name, typ, flags, 0o644, 0, 0).uv(0) }

// rawFifo is a whole entry: a fifo holds nothing more.
func rawFifo(name string) raw { return rawEntry(name, 5, 0) }

func rawNode(count uint64, entries ...raw) []byte {
	b := raw{treeNodeVersion}.uv(count)
	for _, e := range entries {
		b = append(b, e...)
	}
	return b
}

type nodeRefusal struct {
	name   string
	stored []byte
	want   error
	text   string
}

// nodeRefusals is one input per way a stored node is refused.
func nodeRefusals() []nodeRefusal {
	const tooWide = 1 << 32
	overlong := append(bytes.Repeat([]byte{0xff}, 9), 0x02)
	fifo := func(flags byte) raw { return rawEntry("a", 5, flags) }
	// Clipped, so that each case appends to a copy of its own.
	file, head := slices.Clip(rawEntry("a", 1, 0)), slices.Clip(rawHead("a", 5, 0, 0o644, 0, 0))
	corrupt, unsupported := ErrCorruptTree, ErrUnsupportedFormat
	return []nodeRefusal{
		{name: "unknown version", stored: []byte{2, 0}, want: unsupported},
		{name: "version zero", stored: []byte{0, 0}, want: unsupported},
		{name: "file type zero", stored: rawNode(1, rawEntry("a", 0, 0)), want: unsupported},
		{name: "unknown file type", stored: rawNode(1, rawEntry("a", 8, 0)), want: unsupported},
		{name: "unknown flag bit", stored: rawNode(1, fifo(4)), want: unsupported},
		{name: "unknown high flag bit", stored: rawNode(1, fifo(0x80)), want: unsupported},

		{name: "non-minimal uvarint as count", stored: append([]byte{1, 0x81, 0x00}, rawFifo("a")...), want: corrupt},
		{name: "non-minimal uvarint as zero", stored: []byte{1, 0x80, 0x00}, want: corrupt},
		{name: "non-minimal uvarint in an entry", stored: rawNode(1, head.add(0x80, 0x00)), want: corrupt},
		{name: "uvarint above 64 bits", stored: rawNode(1, head.add(overlong...)), want: corrupt},
		{name: "entries descending", stored: rawNode(2, rawFifo("b"), rawFifo("a")), want: corrupt},
		{name: "entries of one name", stored: rawNode(2, rawFifo("a"), rawFifo("a")), want: corrupt},
		{
			name:   "xattrs descending",
			stored: rawNode(1, fifo(2).uv(2).str("b").str("").str("a").str("")),
			want:   corrupt,
		},
		{
			name:   "xattrs of one name",
			stored: rawNode(1, fifo(2).uv(2).str("a").str("").str("a").str("")),
			want:   corrupt,
		},
		{name: "empty name", stored: rawNode(2, rawFifo(""), rawFifo("ab")), want: corrupt},
		{name: "name with a slash", stored: rawNode(1, rawFifo("a/b")), want: corrupt},
		{name: "name with a NUL", stored: rawNode(1, rawFifo("a\x00")), want: corrupt},
		{name: "name that is a dot", stored: rawNode(1, rawFifo(".")), want: corrupt, text: `".."`},
		{name: "name that is two dots", stored: rawNode(1, rawFifo("..")), want: corrupt, text: `".."`},
		{
			name:   "symlink without a target",
			stored: rawNode(1, rawEntry("a", 3, 0).str("")),
			want:   corrupt,
			text:   "no target",
		},
		{
			name:   "hardlink without a target",
			stored: rawNode(1, rawEntry("a", 4, 0).str("")),
			want:   corrupt,
			text:   "no target",
		},
		{name: "mode above 07777", stored: rawNode(1, rawHead("a", 5, 0, 0o10000, 0, 0).uv(0)), want: corrupt},
		{name: "uid above 32 bits", stored: rawNode(1, rawHead("a", 5, 0, 0o644, tooWide, 0).uv(0)), want: corrupt},
		{name: "gid above 32 bits", stored: rawNode(1, rawHead("a", 5, 0, 0o644, 0, tooWide).uv(0)), want: corrupt},
		{name: "device major above 32 bits", stored: rawNode(1, rawEntry("a", 6, 0).uv(tooWide).uv(0)), want: corrupt},
		{name: "device minor above 32 bits", stored: rawNode(1, rawEntry("a", 7, 0).uv(0).uv(tooWide)), want: corrupt},
		{name: "zero-length chunk", stored: rawNode(1, file.uv(1).id().uv(0)), want: corrupt},
		{name: "xattr flag with count zero", stored: rawNode(1, fifo(2).uv(0)), want: corrupt},
		{
			name:   "entry count the input cannot hold",
			stored: rawNode(2, rawFifo("a")),
			want:   corrupt,
			text:   "do not fit",
		},
		{name: "huge entry count", stored: rawNode(1 << 62), want: corrupt, text: "do not fit"},
		{
			name:   "chunk count the input cannot hold",
			stored: rawNode(1, file.uv(2).id().uv(1)),
			want:   corrupt,
			text:   "do not fit",
		},
		{name: "huge chunk count", stored: rawNode(1, file.uv(1<<62)), want: corrupt, text: "do not fit"},
		{
			name:   "xattr count the input cannot hold",
			stored: rawNode(1, fifo(2).uv(2).str("a").str("")),
			want:   corrupt,
			text:   "do not fit",
		},
		{name: "huge xattr count", stored: rawNode(1, fifo(2).uv(1<<62)), want: corrupt, text: "do not fit"},
		{name: "name longer than the input", stored: rawNode(1, raw{}.uv(1<<40).add(rawFifo("a")...)), want: corrupt},
		{
			name:   "link target longer than the input",
			stored: rawNode(1, rawEntry("a", 3, 0).uv(5).add('x')),
			want:   corrupt,
		},
		{name: "trailing bytes", stored: append(rawNode(1, rawFifo("a")), 0), want: corrupt},
		{name: "trailing bytes after an empty node", stored: []byte{1, 0, 0}, want: corrupt},
		{name: "empty input", stored: nil, want: corrupt},
		{name: "chunk length above an int64", stored: rawNode(1, file.uv(1).id().uv(1<<63)), want: corrupt},
		{
			name:   "chunk lengths above an int64 together",
			stored: rawNode(1, file.uv(2).id().uv(math.MaxInt64).id().uv(1)),
			want:   corrupt,
		},
	}
}

func TestTreeNodeDecodeRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range nodeRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node, err := decodeTreeNode(tc.stored)
			if !errors.Is(err, tc.want) {
				t.Fatalf("decodeTreeNode(%x) = %+v, %v; want %v", tc.stored, node, err, tc.want)
			}
			if errors.Is(err, ErrCorruptTree) && errors.Is(err, ErrUnsupportedFormat) {
				t.Errorf("error %v is both corrupt and unsupported", err)
			}
			// A count is refused where it is read, before anything is sized by it.
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q, want one that says %q", err, tc.text)
			}
		})
	}
}

// TestRawNodeBuilderWritesValidNodes keeps the refusal cases honest: the
// entries they are built from decode when nothing is wrong with them.
func TestRawNodeBuilderWritesValidNodes(t *testing.T) {
	t.Parallel()
	stored := rawNode(5,
		rawEntry("a", 1, 0).uv(1).id().uv(1),
		rawEntry("b", 3, 0).str("x"),
		rawEntry("c", 6, 0).uv(1).uv(math.MaxUint32),
		rawEntry("d", 5, 2).uv(2).str("a").str("").str("b").str(""),
		rawHead("e", 5, 0, 0o7777, math.MaxUint32, math.MaxUint32).uv(math.MaxUint64),
	)
	node, err := decodeTreeNode(stored)
	if err != nil {
		t.Fatalf("decodeTreeNode: %v", err)
	}
	if got := mustEncodeNode(t, node); !bytes.Equal(got, stored) {
		t.Errorf("encoded:\n got %x\nwant %x", got, stored)
	}
}

// TestTreeNodeHasOneEncoding cuts and damages a node holding every kind of
// entry: no prefix of it decodes, and whatever a changed bit leaves decodable
// encodes back to exactly the bytes that were read.
func TestTreeNodeHasOneEncoding(t *testing.T) {
	t.Parallel()
	stored := mustEncodeNode(t, fullNode())
	for n := range stored {
		if _, err := decodeTreeNode(stored[:n]); !errors.Is(err, ErrCorruptTree) {
			t.Fatalf("the first %d of %d bytes: got %v, want ErrCorruptTree", n, len(stored), err)
		}
	}
	accepted := 0
	for i := range stored {
		for bit := range 8 {
			changed := bytes.Clone(stored)
			changed[i] ^= 1 << bit
			node, err := decodeTreeNode(changed)
			if err != nil {
				if !errors.Is(err, ErrCorruptTree) && !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("byte %d bit %d: error %v is neither corrupt nor unsupported", i, bit, err)
				}
				continue
			}
			accepted++
			again, err := encodeTreeNode(node)
			if err != nil {
				t.Fatalf("byte %d bit %d: the decoder accepts what the encoder refuses: %v", i, bit, err)
			}
			if !bytes.Equal(again, changed) {
				t.Fatalf("byte %d bit %d: decoded and encoded again gives other bytes", i, bit)
			}
		}
	}
	if accepted == 0 {
		t.Error("no changed bit left the node decodable, the second half tested nothing")
	}
}

func TestTreeNodeEncodeRefuses(t *testing.T) {
	t.Parallel()
	file := func(change func(e *treeEntry)) *treeNode {
		e := treeEntry{Name: "a", Type: FileTypeRegular, Mode: 0o644, ModTime: nodeMTime}
		change(&e)
		return &treeNode{Entries: []treeEntry{e}}
	}
	fifos := func(names ...string) *treeNode {
		node := &treeNode{}
		for _, name := range names {
			node.Entries = append(node.Entries, treeEntry{Name: name, Type: fileTypeFifo})
		}
		return node
	}
	cases := map[string]*treeNode{
		"entries descending":    fifos("b", "a"),
		"entries of one name":   fifos("a", "a"),
		"empty name":            fifos(""),
		"name with a slash":     fifos("a/b"),
		"name with a NUL":       fifos("a\x00b"),
		"name that is a dot":    fifos("."),
		"name that is two dots": fifos(".."),
		"no file type":          file(func(e *treeEntry) { e.Type = "" }),
		"file type of no node":  file(func(e *treeEntry) { e.Type = FileTypeUnsupported }),
		"mode above 07777":      file(func(e *treeEntry) { e.Mode = 0o10000 }),
		"mode with a type bit":  file(func(e *treeEntry) { e.Mode = 0o100644 }),
		"zero-length chunk":     file(func(e *treeEntry) { e.Chunks = []chunkRef{{ID: nodeID(1)}} }),
		"negative chunk length": file(func(e *treeEntry) { e.Chunks = []chunkRef{{ID: nodeID(1), Length: -1}} }),
		"chunk id not hex":      file(func(e *treeEntry) { e.Chunks = []chunkRef{{ID: "chunk-a", Length: 1}} }),
		"chunk id in upper case": file(
			func(e *treeEntry) { e.Chunks = []chunkRef{{ID: strings.ToUpper(nodeID(0xab)), Length: 1}} },
		),
		"directory without hash": file(func(e *treeEntry) { e.Type = FileTypeDirectory }),
		"directory with chunks": file(func(e *treeEntry) {
			e.Type, e.TreeHash, e.Chunks = FileTypeDirectory, nodeID(1), []chunkRef{{ID: nodeID(1), Length: 1}}
		}),
		"file with a tree hash":     file(func(e *treeEntry) { e.TreeHash = nodeID(1) }),
		"file with a link target":   file(func(e *treeEntry) { e.LinkDest = "x" }),
		"fifo with a device":        file(func(e *treeEntry) { e.Type, e.DevMinor = fileTypeFifo, 1 }),
		"symlink without a target":  file(func(e *treeEntry) { e.Type = FileTypeSymlink }),
		"hardlink without a target": file(func(e *treeEntry) { e.Type = FileTypeHardlink }),
		"size above the chunks": file(func(e *treeEntry) {
			e.Size, e.Chunks = 2, []chunkRef{{ID: nodeID(1), Length: 1}}
		}),
		"size without chunks": file(func(e *treeEntry) { e.Size = 1 }),
		"chunks without a size": file(func(e *treeEntry) {
			e.Chunks = []chunkRef{{ID: nodeID(1), Length: 1}}
		}),
		"chunk lengths above an int64 together": file(func(e *treeEntry) {
			e.Chunks = []chunkRef{{ID: nodeID(1), Length: math.MaxInt64}, {ID: nodeID(2), Length: 1}}
		}),
	}
	if math.MaxInt > math.MaxUint32 {
		var wide64 int64 = math.MaxUint32 + 1
		wide := int(wide64)
		cases["uid above 32 bits"] = file(func(e *treeEntry) { e.UID = wide })
		cases["gid above 32 bits"] = file(func(e *treeEntry) { e.GID = wide })
		var narrow64 int64 = math.MinInt32 - 1
		narrow := int(narrow64)
		cases["uid below 32 bits"] = file(func(e *treeEntry) { e.UID = narrow })
		cases["gid below 32 bits"] = file(func(e *treeEntry) { e.GID = narrow })
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if b, err := encodeTreeNode(node); err == nil {
				t.Errorf("encodeTreeNode stored %x, want an error", b)
			}
		})
	}
}

// TestTreeNodeStoresANegativeIDAsItsBits pins what a source without owners
// gets: -1 is stored as the id 4294967295 and read back as -1, where int has
// 32 bits and where it has 64.
func TestTreeNodeStoresANegativeIDAsItsBits(t *testing.T) {
	t.Parallel()
	entry := func(uid, gid int) *treeNode {
		return &treeNode{Entries: []treeEntry{
			{Name: "a", Type: fileTypeFifo, ModTime: time.Unix(0, 0), UID: uid, GID: gid},
		}}
	}
	stored := mustEncodeNode(t, entry(-1, math.MinInt32))
	if want := rawNode(1, rawHead("a", 5, 0, 0, math.MaxUint32, 1<<31).uv(0)); !bytes.Equal(stored, want) {
		t.Fatalf("encoded:\n got %x\nwant %x", stored, want)
	}
	got, err := decodeTreeNode(stored)
	if err != nil {
		t.Fatalf("decodeTreeNode: %v", err)
	}
	if e := got.Entries[0]; e.UID != -1 || uint32(e.GID) != 1<<31 {
		t.Errorf("decoded uid %d and gid %d, want -1 and the id 2147483648", e.UID, e.GID)
	}
}

func TestTreeNodeTimesHaveNoZoneAndSaturate(t *testing.T) {
	t.Parallel()
	entry := func(mtime, ctime time.Time) *treeNode {
		return &treeNode{Entries: []treeEntry{{Name: "a", Type: fileTypeFifo, ModTime: mtime, ChangeTime: ctime}}}
	}
	tokyo := time.FixedZone("JST", 9*60*60)
	utc := mustEncodeNode(t, entry(nodeMTime, nodeCTime))
	if got := mustEncodeNode(t, entry(nodeMTime.In(tokyo), nodeCTime.In(tokyo))); !bytes.Equal(got, utc) {
		t.Errorf("the same instants in another zone encode to other bytes:\n got %x\nwant %x", got, utc)
	}

	early, late := time.Date(1500, 1, 1, 0, 0, 0, 0, tokyo), time.Date(3000, 1, 1, 0, 0, 0, 0, tokyo)
	for name, tc := range map[string]struct {
		mtime, ctime time.Time
		wantM, wantC int64
	}{
		"in range":      {nodeMTime.In(tokyo), nodeCTime.In(tokyo), nodeMTime.UnixNano(), nodeCTime.UnixNano()},
		"before":        {early, early, math.MinInt64, math.MinInt64},
		"after":         {late, late, math.MaxInt64, math.MaxInt64},
		"zero mtime":    {time.Time{}, late, math.MinInt64, math.MaxInt64},
		"at the bounds": {time.Unix(0, math.MinInt64), time.Unix(0, math.MaxInt64), math.MinInt64, math.MaxInt64},
	} {
		got, err := decodeTreeNode(mustEncodeNode(t, entry(tc.mtime, tc.ctime)))
		if err != nil {
			t.Fatalf("%s: decodeTreeNode: %v", name, err)
		}
		e := got.Entries[0]
		if e.ModTime.Location() != time.UTC || e.ChangeTime.Location() != time.UTC {
			t.Errorf(
				"%s: decoded times are in %v and %v, want UTC",
				name,
				e.ModTime.Location(),
				e.ChangeTime.Location(),
			)
		}
		// The lowest mtime is no mtime: a source that knows none stores it.
		gotM := e.ModTime.UnixNano()
		if tc.wantM == math.MinInt64 {
			if !e.ModTime.IsZero() {
				t.Errorf("%s: decoded the mtime %v, want the zero Time", name, e.ModTime)
			}
			gotM = math.MinInt64
		}
		if gotM != tc.wantM || e.ChangeTime.UnixNano() != tc.wantC {
			t.Errorf("%s: decoded %d and %d, want %d and %d", name, gotM, e.ChangeTime.UnixNano(), tc.wantM, tc.wantC)
		}
	}
}

func TestTreeNodeDecodeSharesNoMemoryWithItsInput(t *testing.T) {
	t.Parallel()
	node := fullNode()
	stored := mustEncodeNode(t, node)
	got, err := decodeTreeNode(stored)
	if err != nil {
		t.Fatalf("decodeTreeNode: %v", err)
	}
	clear(stored)
	if !reflect.DeepEqual(got, node) {
		t.Error("the decoded node changed with the bytes it was read from")
	}
}

// jsonNode is the tree node as JSON stored it, with the size and the chunk
// offsets it carried. It stays here as what the binary decoder is measured
// against.
type jsonNode struct {
	Entries []jsonEntry `json:"entries"`
}

type jsonEntry struct {
	Name       string            `json:"name"`
	Type       FileType          `json:"type"`
	Mode       uint32            `json:"mode"`
	UID        int               `json:"uid"`
	GID        int               `json:"gid"`
	ModTime    time.Time         `json:"mtime"`
	Size       int64             `json:"size,omitempty"`
	LinkDest   string            `json:"link_dest,omitempty"`
	TreeHash   string            `json:"tree_hash,omitempty"`
	Chunks     []jsonChunk       `json:"chunks,omitempty"`
	Xattrs     map[string][]byte `json:"xattrs,omitempty"`
	ChangeTime time.Time         `json:"ctime,omitzero"`
	Inode      uint64            `json:"inode,omitempty"`
}

type jsonChunk struct {
	ID     string `json:"id"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

// benchNodes is a tree of 200 directories with 49 files and one directory
// each, as binary and as JSON nodes. Files have one chunk, mtime and ctime
// with nanoseconds and an inode; six names in ten are those of git objects.
func benchNodes(tb testing.TB) (binaryNodes, jsonNodes [][]byte) {
	tb.Helper()
	rng := rand.New(rand.NewPCG(29, 1))
	id := func() string {
		var b [32]byte
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return hex.EncodeToString(b[:])
	}
	words := []string{"backup", "config", "index", "internal", "main", "reader", "restore", "snapshot", "test", "tree"}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := func() time.Time { return base.Add(time.Duration(rng.Int64N(int64(2 * 365 * 24 * time.Hour)))) }
	for range 200 {
		names := map[string]bool{}
		for len(names) < 50 {
			name := id()[:38]
			if rng.IntN(10) >= 6 {
				name = fmt.Sprintf(
					"%s_%s_%d.go",
					words[rng.IntN(len(words))],
					words[rng.IntN(len(words))],
					rng.IntN(1000),
				)
			}
			names[name] = true
		}
		var node treeNode
		var ref jsonNode
		for i, name := range slices.Sorted(maps.Keys(names)) {
			e := treeEntry{Name: name, Type: FileTypeDirectory, Mode: 0o755, UID: 1000, GID: 1000, ModTime: stamp()}
			if i > 0 {
				size := 1 + rng.Int64N(40<<10)
				e.Type, e.Mode, e.Size = FileTypeRegular, 0o644, size
				e.Chunks = []chunkRef{{ID: id(), Length: size}}
				e.ChangeTime, e.Inode = stamp(), 1+rng.Uint64N(1<<24)
			} else {
				e.TreeHash = id()
			}
			node.Entries = append(node.Entries, e)
			j := jsonEntry{
				Name: e.Name, Type: e.Type, Mode: e.Mode, UID: e.UID, GID: e.GID, ModTime: e.ModTime,
				Size: e.Size, TreeHash: e.TreeHash, ChangeTime: e.ChangeTime, Inode: e.Inode,
			}
			for _, c := range e.Chunks {
				j.Chunks = append(j.Chunks, jsonChunk(c))
			}
			ref.Entries = append(ref.Entries, j)
		}
		asJSON, err := json.Marshal(ref)
		if err != nil {
			tb.Fatal(err)
		}
		binaryNodes, jsonNodes = append(binaryNodes, mustEncodeNode(tb, &node)), append(jsonNodes, asJSON)
	}
	return binaryNodes, jsonNodes
}

// TestBenchNodesDecodeAlike keeps the benchmark honest: both decoders read
// the same entries from the node set.
func TestBenchNodesDecodeAlike(t *testing.T) {
	t.Parallel()
	binaryNodes, jsonNodes := benchNodes(t)
	for i := range binaryNodes {
		node, err := decodeTreeNode(binaryNodes[i])
		if err != nil {
			t.Fatalf("decodeTreeNode: %v", err)
		}
		var ref jsonNode
		if err := json.Unmarshal(jsonNodes[i], &ref); err != nil {
			t.Fatal(err)
		}
		if len(node.Entries) != 50 || len(ref.Entries) != 50 {
			t.Fatalf("node %d: %d binary and %d JSON entries, want 50", i, len(node.Entries), len(ref.Entries))
		}
		for k, e := range node.Entries {
			j := ref.Entries[k]
			same := e.Name == j.Name && e.Type == j.Type && e.Mode == j.Mode && e.UID == j.UID && e.GID == j.GID &&
				e.ModTime.Equal(j.ModTime) && e.ChangeTime.Equal(j.ChangeTime) && e.Inode == j.Inode &&
				e.Size == j.Size && e.TreeHash == j.TreeHash && len(e.Chunks) == len(j.Chunks)
			for c := 0; same && c < len(e.Chunks); c++ {
				same = e.Chunks[c] == chunkRef(j.Chunks[c])
			}
			if !same {
				t.Fatalf("node %d entry %d: binary %+v, JSON %+v", i, k, e, j)
			}
		}
	}
}

// BenchmarkTreeNodeDecode decodes one node set, 10 000 entries, with the
// binary codec and with the JSON it replaced. The binary decoder has to stay
// at least four times faster.
func BenchmarkTreeNodeDecode(b *testing.B) {
	binaryNodes, jsonNodes := benchNodes(b)
	b.Run("binary", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, stored := range binaryNodes {
				if _, err := decodeTreeNode(stored); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, stored := range jsonNodes {
				var node jsonNode
				if err := json.Unmarshal(stored, &node); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

// FuzzDecodeTreeNode feeds the decoder arbitrary bytes. It must not panic,
// must refuse with one of the two named errors, and must accept only the one
// encoding of a node: what it returns encodes to the same bytes. The seeds are
// a node of every type and the input of every refusal, which plain go test
// runs; a crasher a fuzz run finds is committed to testdata/fuzz.
func FuzzDecodeTreeNode(f *testing.F) {
	f.Add(mustEncodeNode(f, &treeNode{Entries: []treeEntry{}}))
	f.Add(mustEncodeNode(f, fullNode()))
	for _, e := range nodeEntries() {
		f.Add(mustEncodeNode(f, &treeNode{Entries: []treeEntry{e}}))
	}
	for _, r := range nodeRefusals() {
		f.Add(r.stored)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		node, err := decodeTreeNode(data)
		if err != nil {
			if !errors.Is(err, ErrCorruptTree) && !errors.Is(err, ErrUnsupportedFormat) {
				t.Fatalf("decodeTreeNode(%x) = %v, want ErrCorruptTree or ErrUnsupportedFormat", data, err)
			}
			return
		}
		again, err := encodeTreeNode(node)
		if err != nil {
			t.Fatalf("decodeTreeNode(%x) accepted a node encodeTreeNode refuses: %v", data, err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("decodeTreeNode(%x) accepted a second encoding of %x", data, again)
		}
	})
}
