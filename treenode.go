package ark

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// A stored tree node is a binary object. Integers are little endian, a
// uvarint is the one of encoding/binary in its shortest form:
//
//	node  = version (1 byte, value 1), count (uvarint), entry * count
//	entry = name_len (uvarint), name
//	        type (1 byte)      1 file, 2 dir, 3 symlink, 4 hardlink, 5 fifo, 6 chardev, 7 blockdev
//	        flags (1 byte)     bit 0 ctime present, bit 1 xattrs present
//	        mode (uint16)      mode_t & 07777
//	        uid (uvarint), gid (uvarint)
//	        mtime (int64)      nanoseconds since the epoch
//	        ctime (int64)      only if flagged
//	        inode (uvarint)    0 means none
//	        by type:
//	          file               chunk_count (uvarint), { id (32 bytes), length (uvarint) } each
//	          dir                tree_hash (32 bytes)
//	          symlink, hardlink  target_len (uvarint), target
//	          chardev, blockdev  dev_major (uvarint), dev_minor (uvarint)
//	          fifo               nothing
//	        xattr_count (uvarint), { name_len, name, value_len, value } each, only if flagged
//
// Entries are sorted by the bytes of their names and the xattrs of an entry by
// the bytes of theirs, both without duplicates. Names, link targets and xattr
// names are raw bytes. A node has exactly one encoding: decodeTreeNode refuses
// every other spelling of it, so encodeTreeNode(decodeTreeNode(b)) is b for
// every b it accepts. docs/adr/0037-binary-tree-node.md has the reasons.

// treeNodeVersion is the first byte of every stored tree node. Any change to
// the layout is another version.
const treeNodeVersion = 1

// Flag bits of a stored tree entry. Every other bit is refused.
const (
	nodeFlagChangeTime = 1 << iota
	nodeFlagXattrs
)

// The file types a tree node holds beside the exported ones. They are untyped
// so that the switches over FileType, none of which handles them yet, stay
// exhaustive.
const (
	fileTypeFifo        = "fifo"
	fileTypeCharDevice  = "chardev"
	fileTypeBlockDevice = "blockdev"
)

// nodeTypes maps the type byte of a stored entry to its FileType. Zero is no
// type.
var nodeTypes = [...]FileType{
	1: FileTypeRegular,
	2: FileTypeDirectory,
	3: FileTypeSymlink,
	4: FileTypeHardlink,
	5: fileTypeFifo,
	6: fileTypeCharDevice,
	7: fileTypeBlockDevice,
}

// The fewest bytes an entry, a chunk ref and an xattr take. A count is
// checked against them before anything is allocated for it.
const (
	nodeEntryMin = 17 // name_len, one name byte, type, flags, mode, uid, gid, mtime, inode
	nodeChunkMin = sha256.Size + 1
	nodeXattrMin = 2
)

// The first and the last instant a stored time can hold.
var (
	minNodeTime = time.Unix(0, math.MinInt64)
	maxNodeTime = time.Unix(0, math.MaxInt64)
)

// unixNano is t in nanoseconds since the epoch. A time outside the range of
// an int64, the zero Time among them, becomes the nearest bound. The lowest
// value is therefore what an entry without an mtime stores, and the decoder
// reads it back as the zero Time.
func unixNano(t time.Time) int64 {
	switch {
	case t.Before(minNodeTime):
		return math.MinInt64
	case t.After(maxNodeTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}

// id32 is a uid or gid as stored: the 32 bits of v. Where int has 32 bits an
// id above its positive range is a negative int of the same bits, and a
// source without owners reports -1, so a negative int32 is the id of its bits
// on every build. Anything else outside 32 bits is refused.
func id32(v int) (uint32, bool) {
	return uint32(v), int64(v) >= math.MinInt32 && int64(v) <= math.MaxUint32
}

// ownerID is a stored uid or gid as a FileInfo holds it. The id of 32 set
// bits owns no file: chown takes it as "keep the owner", and it is what the
// -1 of a source without owners is stored as, so it reads back as -1 where
// int has 64 bits as it does where int has 32.
func ownerID(v uint32) int {
	if v == math.MaxUint32 {
		return -1
	}
	return int(v)
}

// nodeName reports whether name can name an entry: one path element, so
// neither empty nor "." nor "..", without a slash or a NUL. Every path a tree
// yields is joined from such names and cannot leave the tree.
func nodeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// encodeTreeNode is the stored form of node. It refuses a node that
// decodeTreeNode would refuse, and an entry with a field its file type has no
// place for, so nothing a caller filled in is dropped. Size and the chunk
// offsets are not stored: both follow from the chunk lengths, and a regular
// file whose Size is not their sum is refused. The Size of any other entry is
// not looked at; that of a hardlink is its target's.
func encodeTreeNode(node *treeNode) ([]byte, error) {
	b := make([]byte, 0, 16+96*len(node.Entries))
	b = append(b, treeNodeVersion)
	b = binary.AppendUvarint(b, uint64(len(node.Entries)))
	for i := range node.Entries {
		e := &node.Entries[i]
		if i > 0 && e.Name <= node.Entries[i-1].Name {
			return nil, fmt.Errorf("tree: entry %q does not sort after %q", e.Name, node.Entries[i-1].Name)
		}
		var err error
		if b, err = appendTreeEntry(b, e); err != nil {
			return nil, fmt.Errorf("tree: entry %q: %w", e.Name, err)
		}
	}
	return b, nil
}

func appendTreeEntry(b []byte, e *treeEntry) ([]byte, error) {
	typ := slices.Index(nodeTypes[:], e.Type)
	uid, uidOK := id32(e.UID)
	gid, gidOK := id32(e.GID)
	isLink := e.Type == FileTypeSymlink || e.Type == FileTypeHardlink
	isDev := e.Type == fileTypeCharDevice || e.Type == fileTypeBlockDevice
	switch {
	case !nodeName(e.Name):
		return nil, errors.New(`the name is empty, ".", ".." or holds a slash or a NUL`)
	case typ < 1:
		return nil, fmt.Errorf("the file type %q has no place in a tree node", e.Type)
	case e.Mode&^storedModeMask != 0:
		return nil, fmt.Errorf("mode %#o has a bit outside 07777", e.Mode)
	case !uidOK || !gidOK:
		return nil, fmt.Errorf("uid %d or gid %d does not fit 32 bits", e.UID, e.GID)
	case len(e.Chunks) > 0 && e.Type != FileTypeRegular,
		e.TreeHash != "" && e.Type != FileTypeDirectory,
		e.LinkDest != "" && !isLink,
		e.DevMajor|e.DevMinor != 0 && !isDev:
		return nil, fmt.Errorf("the file type %q does not hold every field that is set", e.Type)
	case isLink && e.LinkDest == "":
		return nil, errors.New("the link has no target")
	}
	var flags byte
	if !e.ChangeTime.IsZero() {
		flags |= nodeFlagChangeTime
	}
	if len(e.Xattrs) > 0 {
		flags |= nodeFlagXattrs
	}

	b = appendNodeBytes(b, e.Name)
	b = append(b, byte(typ), flags)
	b = binary.LittleEndian.AppendUint16(b, uint16(e.Mode))
	b = binary.AppendUvarint(b, uint64(uid))
	b = binary.AppendUvarint(b, uint64(gid))
	b = binary.LittleEndian.AppendUint64(b, uint64(unixNano(e.ModTime)))
	if flags&nodeFlagChangeTime != 0 {
		b = binary.LittleEndian.AppendUint64(b, uint64(unixNano(e.ChangeTime)))
	}
	b = binary.AppendUvarint(b, e.Inode)

	switch {
	case e.Type == FileTypeRegular:
		b = binary.AppendUvarint(b, uint64(len(e.Chunks)))
		var size int64
		for _, c := range e.Chunks {
			id, ok := parseName(c.ID)
			switch {
			case !ok:
				return nil, fmt.Errorf("chunk id %q is not 64 lowercase hex characters", c.ID)
			case c.Length <= 0:
				return nil, fmt.Errorf("chunk %s has the length %d", c.ID, c.Length)
			case c.Length > math.MaxInt64-size:
				return nil, errors.New("the chunk lengths add up to more than an int64 holds")
			}
			size += c.Length
			b = append(b, id[:]...)
			b = binary.AppendUvarint(b, uint64(c.Length))
		}
		if e.Size != size {
			return nil, fmt.Errorf("the size %d is not the %d bytes of its chunks", e.Size, size)
		}
	case e.Type == FileTypeDirectory:
		hash, ok := parseName(e.TreeHash)
		if !ok {
			return nil, fmt.Errorf("tree hash %q is not 64 lowercase hex characters", e.TreeHash)
		}
		b = append(b, hash[:]...)
	case isLink:
		b = appendNodeBytes(b, e.LinkDest)
	case isDev:
		b = binary.AppendUvarint(b, uint64(e.DevMajor))
		b = binary.AppendUvarint(b, uint64(e.DevMinor))
	}

	if flags&nodeFlagXattrs != 0 {
		b = binary.AppendUvarint(b, uint64(len(e.Xattrs)))
		for _, name := range slices.Sorted(maps.Keys(e.Xattrs)) {
			b = appendNodeBytes(appendNodeBytes(b, name), e.Xattrs[name])
		}
	}
	return b, nil
}

// appendNodeBytes appends p behind its length.
func appendNodeBytes[T string | []byte](b []byte, p T) []byte {
	return append(binary.AppendUvarint(b, uint64(len(p))), p...)
}

// decodeTreeNode reads a stored tree node. A node of a version, a file type
// or a flag bit this build does not know is ErrUnsupportedFormat; everything
// else that is not the one encoding of a node is ErrCorruptTree. Nothing it
// returns shares memory with data. Size and the chunk offsets are the running
// sums of the chunk lengths; times are in UTC, and the lowest mtime a node can
// hold is the zero Time.
func decodeTreeNode(data []byte) (*treeNode, error) {
	r := nodeReader{b: data}
	if v := r.byte(); r.err == nil && v != treeNodeVersion {
		return nil, fmt.Errorf("%w: tree node of version %d, this build reads version %d",
			ErrUnsupportedFormat, v, treeNodeVersion)
	}
	count := r.count(nodeEntryMin)
	if r.err != nil {
		return nil, r.err
	}
	node := &treeNode{Entries: make([]treeEntry, count)}
	for i := range node.Entries {
		e := &node.Entries[i]
		r.entry(e)
		if i > 0 && r.err == nil && e.Name <= node.Entries[i-1].Name {
			r.corruptf("the name %q does not sort after %q", e.Name, node.Entries[i-1].Name)
		}
		if r.err != nil {
			return nil, fmt.Errorf("%w (entry %d of %d)", r.err, i+1, count)
		}
	}
	if len(r.b) != 0 {
		r.corruptf("%d bytes follow the last entry", len(r.b))
	}
	if r.err != nil {
		return nil, r.err
	}
	return node, nil
}

// nodeReader takes a stored tree node apart front to back. The first failure
// sticks and empties the input, so every later read returns zero and every
// later count is zero: a caller checks err once per entry, and inside a loop
// a count bounds.
type nodeReader struct {
	b   []byte
	err error
}

func (r *nodeReader) failf(kind error, format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: tree node: %s", kind, fmt.Sprintf(format, args...))
	}
	r.b = nil
}

func (r *nodeReader) corruptf(format string, args ...any) {
	r.failf(ErrCorruptTree, format, args...)
}

// take returns the next n bytes, or nil when the input does not have them.
func (r *nodeReader) take(n uint64) []byte {
	if n > uint64(len(r.b)) {
		r.corruptf("a field of %d bytes starts %d bytes before the end", n, len(r.b))
		return nil
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}

func (r *nodeReader) byte() byte {
	if p := r.take(1); len(p) == 1 {
		return p[0]
	}
	return 0
}

func (r *nodeReader) int64() int64 {
	if p := r.take(8); len(p) == 8 {
		return int64(binary.LittleEndian.Uint64(p))
	}
	return 0
}

// uvarint reads a uvarint in its shortest form. [binary.Uvarint] also takes
// the longer ones, which end in a zero byte.
func (r *nodeReader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	switch {
	case n == 0:
		r.corruptf("a number starts %d bytes before the end and does not end there", len(r.b))
	case n < 0:
		r.corruptf("a number does not fit 64 bits")
	case n > 1 && r.b[n-1] == 0:
		r.corruptf("the number %d is not stored in its shortest form", v)
	default:
		r.b = r.b[n:]
		return v
	}
	return 0
}

func (r *nodeReader) uint32(what string) uint32 {
	v := r.uvarint()
	if v > math.MaxUint32 {
		r.corruptf("%s %d does not fit 32 bits", what, v)
		return 0
	}
	return uint32(v)
}

// count reads how many items follow, each of at least size bytes, and
// refuses a count the rest of the input cannot hold.
func (r *nodeReader) count(size int) int {
	v := r.uvarint()
	if v > uint64(len(r.b)/size) {
		r.corruptf("%d items of at least %d bytes do not fit the %d bytes that follow", v, size, len(r.b))
		return 0
	}
	return int(v)
}

func (r *nodeReader) string() string { return string(r.take(r.uvarint())) }

// name reads the 32 bytes of a chunk id or a tree hash as lowercase hex.
func (r *nodeReader) name() string {
	var h [2 * sha256.Size]byte
	hex.Encode(h[:], r.take(sha256.Size))
	return string(h[:])
}

func (r *nodeReader) entry(e *treeEntry) {
	e.Name = r.string()
	if r.err == nil && !nodeName(e.Name) {
		r.corruptf(`the name %q is empty, ".", ".." or holds a slash or a NUL`, e.Name)
	}
	typ, flags := r.byte(), r.byte()
	switch {
	case r.err != nil:
	case typ == 0 || int(typ) >= len(nodeTypes):
		r.failf(ErrUnsupportedFormat, "%q has the file type %d, which this build does not know", e.Name, typ)
	case flags&^(nodeFlagChangeTime|nodeFlagXattrs) != 0:
		r.failf(ErrUnsupportedFormat, "%q has the flags %#02x, which this build does not know", e.Name, flags)
	default:
		e.Type = nodeTypes[typ]
	}
	if p := r.take(2); len(p) == 2 {
		e.Mode = uint32(binary.LittleEndian.Uint16(p))
		if e.Mode&^storedModeMask != 0 {
			r.corruptf("mode %#o of %q has a bit outside 07777", e.Mode, e.Name)
		}
	}
	e.UID = ownerID(r.uint32("uid"))
	e.GID = ownerID(r.uint32("gid"))
	if ns := r.int64(); ns != math.MinInt64 {
		e.ModTime = time.Unix(0, ns).UTC()
	}
	if flags&nodeFlagChangeTime != 0 {
		e.ChangeTime = time.Unix(0, r.int64()).UTC()
	}
	e.Inode = r.uvarint()

	switch e.Type {
	case FileTypeRegular:
		r.chunks(e)
	case FileTypeDirectory:
		e.TreeHash = r.name()
	case FileTypeSymlink, FileTypeHardlink:
		if e.LinkDest = r.string(); r.err == nil && e.LinkDest == "" {
			r.corruptf("the link %q has no target", e.Name)
		}
	case fileTypeCharDevice, fileTypeBlockDevice:
		e.DevMajor = r.uint32("device major")
		e.DevMinor = r.uint32("device minor")
	case fileTypeFifo, FileTypeUnsupported:
		// Nothing follows.
	}
	if flags&nodeFlagXattrs != 0 {
		r.xattrs(e)
	}
}

func (r *nodeReader) chunks(e *treeEntry) {
	n := r.count(nodeChunkMin)
	if n == 0 {
		return
	}
	e.Chunks = make([]chunkRef, n)
	for i := range e.Chunks {
		id, length := r.name(), r.uvarint()
		switch {
		case r.err != nil:
			return
		case length == 0:
			r.corruptf("chunk %s of %q has no length", id, e.Name)
			return
		case length > uint64(math.MaxInt64-e.Size):
			r.corruptf("the chunk lengths of %q add up to more than an int64 holds", e.Name)
			return
		}
		e.Chunks[i] = chunkRef{ID: id, Length: int64(length), Offset: e.Size}
		e.Size += int64(length)
	}
}

func (r *nodeReader) xattrs(e *treeEntry) {
	n := r.count(nodeXattrMin)
	if n == 0 {
		r.corruptf("%q is flagged as having xattrs and has none", e.Name)
		return
	}
	e.Xattrs = make(map[string][]byte, n)
	var last string
	for i := range n {
		name := r.string()
		value := bytes.Clone(r.take(r.uvarint()))
		switch {
		case r.err != nil:
			return
		case i > 0 && name <= last:
			r.corruptf("the xattr %q of %q does not sort after %q", name, e.Name, last)
			return
		}
		e.Xattrs[name] = value
		last = name
	}
}
