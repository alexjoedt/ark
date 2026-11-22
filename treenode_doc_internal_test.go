package ark

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const treeNodePage = "docs/format/tree-node.md"

// pageDump returns the bytes of the first fenced block after the heading
// "## Worked example": each line is hex bytes separated by single spaces,
// then two spaces and a comment.
func pageDump(t *testing.T, page string) []byte {
	t.Helper()
	_, after, ok := strings.Cut(page, "\n## Worked example\n")
	if !ok {
		t.Fatalf("%s has no section Worked example", treeNodePage)
	}
	_, after, ok = strings.Cut(after, "\n```text\n")
	if !ok {
		t.Fatalf("%s: Worked example has no text block", treeNodePage)
	}
	block, _, ok := strings.Cut(after, "\n```\n")
	if !ok {
		t.Fatalf("%s: the text block of Worked example does not end", treeNodePage)
	}
	var stored []byte
	for line := range strings.Lines(block) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field, _, _ := strings.Cut(line, "  ")
		for h := range strings.FieldsSeq(field) {
			b, err := hex.DecodeString(h)
			if err != nil || len(b) != 1 {
				t.Fatalf("%s: %q in the line %q is not one byte in hex", treeNodePage, h, line)
			}
			stored = append(stored, b[0])
		}
	}
	return stored
}

// pageExampleNode is the node the comments of the page's dump describe.
func pageExampleNode() *treeNode {
	mtime := time.Date(2026, 11, 21, 18, 35, 7, 123456789, time.UTC)
	e := func(name string, typ FileType, mode uint32, uid, gid int, inode uint64) treeEntry {
		return treeEntry{Name: name, Type: typ, Mode: mode, UID: uid, GID: gid, ModTime: mtime, Inode: inode}
	}
	dir := e("dir", FileTypeDirectory, 0o755, 1000, 1000, 0)
	dir.TreeHash = nodeID(0xcc)
	dir.Xattrs = map[string][]byte{"user.tag": []byte("ok")}
	file := e("file", FileTypeRegular, 0o644, 1000, 1000, 4242)
	file.ChangeTime = time.Date(2026, 11, 21, 18, 35, 8, 0, time.UTC)
	file.Chunks = []chunkRef{{ID: nodeID(0xaa), Length: 1 << 20}, {ID: nodeID(0xbb), Length: 300, Offset: 1 << 20}}
	file.Size = 1_048_876
	link := e("link", FileTypeHardlink, 0o644, 1000, 1000, 4242)
	link.LinkDest = "file"
	null := e("null", FileTypeCharDevice, 0o666, 0, 0, 0)
	null.DevMajor, null.DevMinor = 1, 3
	sda := e("sda", FileTypeBlockDevice, 0o660, 0, 6, 0)
	sda.DevMajor = 8
	sym := e("sym", FileTypeSymlink, 0o777, 1000, 1000, 0)
	sym.LinkDest = "dir/x"
	pipe := e("pipe", FileTypeFifo, 0o600, 1000, 1000, 0)
	return &treeNode{Entries: []treeEntry{dir, file, link, null, pipe, sda, sym}}
}

func TestTreeNodePageExampleRoundTrips(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(treeNodePage)
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	stored := pageDump(t, page)

	node, err := decodeTreeNode(stored)
	if err != nil {
		t.Fatalf("decodeTreeNode(the example): %v", err)
	}
	if want := pageExampleNode(); !reflect.DeepEqual(node, want) {
		t.Errorf("the example is not the node its comments describe:\n got %+v\nwant %+v", node, want)
	}
	again := mustEncodeNode(t, node)
	if !bytes.Equal(again, stored) {
		t.Fatalf("the example encodes to other bytes:\n got %x\nwant %x", again, stored)
	}
	if want := fmt.Sprintf("The node is %d bytes.", len(stored)); !strings.Contains(page, want) {
		t.Errorf("%s does not say %q", treeNodePage, want)
	}

	types := map[FileType]bool{}
	var ctime, xattrs bool
	for _, e := range node.Entries {
		types[e.Type] = true
		ctime = ctime || !e.ChangeTime.IsZero()
		xattrs = xattrs || len(e.Xattrs) > 0
	}
	for _, typ := range nodeTypes[1:] {
		if !types[typ] {
			t.Errorf("the example has no entry of type %s", typ)
		}
	}
	if !ctime || !xattrs {
		t.Errorf("the example has an entry with a ctime: %v, with xattrs: %v; want both", ctime, xattrs)
	}
}

// nodeRefusalRows names, per case of nodeRefusals, the row of the page's
// tables that states the rule it breaks.
var nodeRefusalRows = map[string]string{
	"unknown version":       "a version byte other than `0x01`",
	"version zero":          "a version byte other than `0x01`",
	"file type zero":        "a type byte of `0x00` or above `0x07`",
	"unknown file type":     "a type byte of `0x00` or above `0x07`",
	"unknown flag bit":      "a flag bit other than bit 0 and bit 1",
	"unknown high flag bit": "a flag bit other than bit 0 and bit 1",

	"non-minimal uvarint as count":          "a uvarint that isn't in its shortest form",
	"non-minimal uvarint as zero":           "a uvarint that isn't in its shortest form",
	"non-minimal uvarint in an entry":       "a uvarint that isn't in its shortest form",
	"uvarint above 64 bits":                 "a uvarint that doesn't fit 64 bits",
	"entries descending":                    "entries not in strictly ascending order, two of one name too",
	"entries of one name":                   "entries not in strictly ascending order, two of one name too",
	"xattrs descending":                     "xattrs of an entry not in strictly ascending order, two of one name too",
	"xattrs of one name":                    "xattrs of an entry not in strictly ascending order, two of one name too",
	"empty name":                            "an empty name",
	"name with a slash":                     "a name that holds `/` or NUL",
	"name with a NUL":                       "a name that holds `/` or NUL",
	"name that is a dot":                    "the name `.` or `..`",
	"name that is two dots":                 "the name `.` or `..`",
	"symlink without a target":              "a symlink or hardlink with an empty target",
	"hardlink without a target":             "a symlink or hardlink with an empty target",
	"mode above 07777":                      "a mode above `07777`",
	"uid above 32 bits":                     "a uid or gid above `4294967295`",
	"gid above 32 bits":                     "a uid or gid above `4294967295`",
	"device major above 32 bits":            "a device major or minor above `4294967295`",
	"device minor above 32 bits":            "a device major or minor above `4294967295`",
	"zero-length chunk":                     "a chunk of length 0",
	"xattr flag with count zero":            "flag bit 1 with an xattr count of 0",
	"entry count the input cannot hold":     "an entry count above the bytes after it divided by 17",
	"huge entry count":                      "an entry count above the bytes after it divided by 17",
	"chunk count the input cannot hold":     "a chunk count above the bytes after it divided by 33",
	"huge chunk count":                      "a chunk count above the bytes after it divided by 33",
	"xattr count the input cannot hold":     "an xattr count above the bytes after it divided by 2",
	"huge xattr count":                      "an xattr count above the bytes after it divided by 2",
	"name longer than the input":            "a name, target, xattr name or value longer than the rest of the input",
	"link target longer than the input":     "a name, target, xattr name or value longer than the rest of the input",
	"trailing bytes":                        "bytes after the last entry",
	"trailing bytes after an empty node":    "bytes after the last entry",
	"empty input":                           "an empty input, or one that ends inside a field",
	"chunk length above an int64":           "chunk lengths of one file whose sum is above `9223372036854775807`",
	"chunk lengths above an int64 together": "chunk lengths of one file whose sum is above `9223372036854775807`",
}

// TestTreeNodePageStatesEveryRefusal ties each refusal the decoder is tested
// for to a row of the page with the error it returns, so no rule lives only
// in the Go source.
func TestTreeNodePageStatesEveryRefusal(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(treeNodePage)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for line := range strings.Lines(string(b)) {
		cells := strings.Split(strings.TrimSpace(line), " | ")
		if len(cells) == 2 && strings.HasPrefix(cells[0], "| ") {
			rows[strings.TrimPrefix(cells[0], "| ")] = strings.TrimSuffix(cells[1], " |")
		}
	}
	cases := map[string]bool{}
	for _, tc := range nodeRefusals() {
		cases[tc.name] = true
		row, ok := nodeRefusalRows[tc.name]
		if !ok {
			t.Errorf("refusal %q has no row in nodeRefusalRows", tc.name)
			continue
		}
		want := "`ErrCorruptTree`"
		if errors.Is(tc.want, ErrUnsupportedFormat) {
			want = "`ErrUnsupportedFormat`"
		}
		switch got, ok := rows[row]; {
		case !ok:
			t.Errorf("refusal %q: %s has no row %q", tc.name, treeNodePage, row)
		case got != want:
			t.Errorf("refusal %q: the row %q gives %s, want %s", tc.name, row, got, want)
		}
	}
	for name := range nodeRefusalRows {
		if !cases[name] {
			t.Errorf("nodeRefusalRows names %q, which nodeRefusals has no case for", name)
		}
	}
}
