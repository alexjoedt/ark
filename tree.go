package ark

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// treeNode is a directory node in the snapshot Merkle tree. It lives in the
// trees namespace, named by the HMAC of its stored bytes, which treenode.go
// defines.
type treeNode struct {
	Entries []treeEntry
}

// treeEntry is one child inside a treeNode.
type treeEntry struct {
	Name string
	Type FileType
	// Mode is mode_t & 07777: the permission bits with setuid 04000, setgid
	// 02000 and sticky 01000. The file type is in Type, never here.
	Mode    uint32
	UID     int
	GID     int
	ModTime time.Time
	// Size is the sum of the chunk lengths and is not stored; a hardlink's is
	// its target's, which Walk and Restore fill in.
	Size     int64
	LinkDest string
	TreeHash string
	Chunks   []chunkRef

	// Xattrs maps an extended attribute name to its value.
	Xattrs map[string][]byte

	// ChangeTime and Inode are what a later backup compares to skip the
	// file; tar sources carry neither.
	ChangeTime time.Time
	Inode      uint64

	// DevMajor and DevMinor are the device numbers of a character or block
	// device.
	DevMajor uint32
	DevMinor uint32
}

// storedModeMask is every bit a stored mode may carry.
const storedModeMask = 0o7777

// UnixMode returns m as mode_t & 07777, the form a snapshot stores and a tar
// header holds: the permission bits plus setuid, setgid and sticky. Type bits
// and every other bit of m are dropped.
func UnixMode(m os.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// goMode is the inverse of UnixMode: the permission bits plus ModeSetuid,
// ModeSetgid and ModeSticky, and no type bit.
func goMode(mode uint32) os.FileMode {
	m := os.FileMode(mode).Perm()
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// fileInfo is the entry as a FileInfo under path.
func (e treeEntry) fileInfo(path string) FileInfo {
	return FileInfo{
		Path:     path,
		Type:     e.Type,
		Mode:     goMode(e.Mode),
		UID:      e.UID,
		GID:      e.GID,
		ModTime:  e.ModTime,
		Size:     e.Size,
		LinkDest: e.LinkDest,
		Xattrs:   e.Xattrs,

		ChangeTime: e.ChangeTime,
		Inode:      e.Inode,
	}
}

// marshalTreeNode is the stored form of the node of dir holding children, the
// bytes buildTree stores and the input of the node's name. Sorting here rather
// than at every call site keeps the canonical form an invariant of the write
// path. Two children of one name are an error: a node holds a name once, and
// leaving one of them out would drop an entry the source yielded.
func marshalTreeNode(dir string, children []fileEntry, hashes map[string]string) ([]byte, error) {
	node := treeNode{Entries: make([]treeEntry, 0, len(children))}
	for _, child := range children {
		te, err := newTreeEntry(child, hashes)
		if err != nil {
			return nil, err
		}
		node.Entries = append(node.Entries, te)
	}
	slices.SortFunc(node.Entries, func(a, b treeEntry) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(node.Entries); i++ {
		if name := node.Entries[i].Name; name == node.Entries[i-1].Name {
			return nil, fmt.Errorf("tree: the source yielded %q more than once", path.Join(dir, name))
		}
	}
	data, err := encodeTreeNode(&node)
	if err != nil {
		return nil, fmt.Errorf("tree: directory %q: %w", dir, err)
	}
	return data, nil
}

// storedType reports whether t is a file type the readers of a tree handle.
// The node format also holds fifos and devices, which no reader does yet.
func storedType(t FileType) bool {
	return t == FileTypeRegular || t == FileTypeDirectory || t == FileTypeSymlink || t == FileTypeHardlink
}

func loadTreeNode(ctx context.Context, trees *objectStore, hash string) (*treeNode, error) {
	data, err := trees.Get(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("tree: failed to load node %s: %w", hash, err)
	}
	node, err := decodeTreeNode(data)
	if err != nil {
		return nil, fmt.Errorf("tree: node %s: %w", hash, err)
	}
	// Every reader of a tree loads its nodes here, so none of them meets a
	// type it would have to skip: a restore that leaves the entry out and a
	// prune that counts it as holding nothing are both wrong.
	for i := range node.Entries {
		if e := &node.Entries[i]; !storedType(e.Type) {
			return nil, fmt.Errorf("%w: node %s: %q has the file type %q, which this build does not know",
				ErrUnsupportedFormat, hash, e.Name, e.Type)
		}
	}
	return node, nil
}

// linkTarget is the regular file a hardlink at link names. A tree that holds
// no entry there, or one that is not a regular file, is corrupt; a node on the
// way that does not load keeps its own error.
func linkTarget(load func(hash string) (*treeNode, error), root, link string) (treeEntry, error) {
	target, err := lookupTreeEntry(load, root, link)
	switch {
	case errors.Is(err, errNoTreeEntry):
		err = fmt.Errorf("%w: %w", ErrCorruptTree, err)
	case err == nil && target.Type != FileTypeRegular:
		err = fmt.Errorf("%w: target is not a regular file", ErrCorruptTree)
	}
	return target, err
}

// linkSize is the size of the file a hardlink at link names.
func linkSize(load func(hash string) (*treeNode, error), root, link string) (int64, error) {
	target, err := linkTarget(load, root, link)
	return target.Size, err
}

// buildTree organises the flat fileEntry list into a Merkle directory tree,
// seals every treeNode into the tree packs with at most workers at work, and
// returns the root tree hash.
func buildTree(ctx context.Context, trees *objectStore, entries []fileEntry, workers int) (string, error) {
	dirChildren := treeChildren(entries)

	// Every directory reachable from the root, by depth. A directory held by
	// a non-directory entry is not reachable and is not stored.
	levels := [][]string{{""}}
	seen := map[string]struct{}{"": {}}
	for {
		var next []string
		for _, dir := range levels[len(levels)-1] {
			for _, child := range dirChildren[dir] {
				norm := normEntryPath(child.Path)
				if _, ok := seen[norm]; ok || child.Type != FileTypeDirectory {
					continue
				}
				seen[norm] = struct{}{}
				next = append(next, norm)
			}
		}
		if len(next) == 0 {
			break
		}
		levels = append(levels, next)
	}

	// The deepest level is stored first, so every node is stored after its
	// children and the root last. Within a level the nodes are independent.
	hashes := make(map[string]string, len(seen))
	for _, level := range slices.Backward(levels) {
		if err := storeTreeLevel(ctx, trees, level, dirChildren, hashes, workers); err != nil {
			return "", err
		}
	}
	return hashes[""], nil
}

// treeChildren groups entries by their parent directory, "" for the root,
// and adds the directories a source left out.
func treeChildren(entries []fileEntry) map[string][]fileEntry {
	present := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if norm := normEntryPath(e.Path); norm != "" {
			present[norm] = struct{}{}
		}
	}

	dirChildren := make(map[string][]fileEntry, 64)
	for _, e := range entries {
		norm := normEntryPath(e.Path)
		if norm == "" {
			continue
		}
		parent := normParent(norm)
		dirChildren[parent] = append(dirChildren[parent], e)

		// A source may yield an entry without its directories, as many tar
		// archives do. The missing ones are created, or the entry would be
		// unreachable from the root. A path held by a non-directory is left
		// alone.
		for dir := parent; dir != ""; dir = normParent(dir) {
			if _, ok := present[dir]; ok {
				break
			}
			present[dir] = struct{}{}
			up := normParent(dir)
			dirChildren[up] = append(dirChildren[up], fileEntry{FileInfo: FileInfo{
				Path:    dir,
				Type:    FileTypeDirectory,
				Mode:    0o755,
				UID:     e.UID,
				GID:     e.GID,
				ModTime: e.ModTime,
			}})
		}
	}
	return dirChildren
}

// storeTreeLevel stores the nodes of dirs through a chunkWriter, which bounds
// the nodes being sealed, stores identical nodes once and stops the rest on
// the first error. The hashes of their child directories must be in hashes
// already; the level's own hashes land there once every node is stored.
func storeTreeLevel(
	ctx context.Context,
	trees *objectStore,
	dirs []string,
	dirChildren map[string][]fileEntry,
	hashes map[string]string,
	workers int,
) error {
	w := newChunkWriter(ctx, trees, workers)
	stored := make(map[string]string, len(dirs))
	for _, dir := range dirs {
		data, err := marshalTreeNode(dir, dirChildren[dir], hashes)
		if err == nil {
			name := trees.name(data)
			stored[dir] = name
			err = w.put(chunker.Chunk{Hash: name, Data: data})
		}
		if err != nil {
			_ = w.fail(err)
			break
		}
	}
	if err := w.wait(); err != nil {
		return fmt.Errorf("tree: failed to store node: %w", err)
	}
	maps.Copy(hashes, stored)
	return nil
}

// newTreeEntry is the tree entry of child. A directory gets the hash of its
// node from hashes; a directory without one would leave an empty TreeHash in
// the snapshot, so it is an error here rather than a finding at restore time.
func newTreeEntry(child fileEntry, hashes map[string]string) (treeEntry, error) {
	norm := normEntryPath(child.Path)
	te := treeEntry{
		Name:    filepath.Base(norm),
		Type:    child.Type,
		Mode:    UnixMode(child.Mode),
		UID:     child.UID,
		GID:     child.GID,
		ModTime: child.ModTime,
		Xattrs:  child.Xattrs,

		ChangeTime: child.ChangeTime,
		Inode:      child.Inode,
	}
	// What the type has no place for is left out, as a directory's size is.
	switch child.Type {
	case FileTypeRegular:
		te.Chunks = child.Chunks
		te.Size = refsSize(child.Chunks)
	case FileTypeSymlink, FileTypeHardlink:
		te.LinkDest = child.LinkDest
	case FileTypeDirectory, FileTypeUnsupported:
	}
	if child.Type == FileTypeDirectory {
		hash, ok := hashes[norm]
		if !ok {
			return treeEntry{}, fmt.Errorf("tree: directory %s has no stored node", norm)
		}
		te.TreeHash = hash
	}
	return te, nil
}

// pathFilter restricts a tree walk to a set of path prefixes. The zero value
// matches everything.
type pathFilter struct{ prefixes []string }

func newPathFilter(paths []string) pathFilter {
	if len(paths) == 0 {
		return pathFilter{}
	}
	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = normEntryPath(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return pathFilter{prefixes: prefixes}
}

func (f pathFilter) all() bool { return len(f.prefixes) == 0 }

// keep reports whether an entry at path was asked for.
func (f pathFilter) keep(path string) bool {
	if f.all() {
		return true
	}
	for _, p := range f.prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// descend reports whether a directory can still lead to a wanted path. It is
// what prunes whole subtrees before a single chunk is fetched, and it is also
// why a directory on the way to a wanted path is restored itself: the sink
// needs the parents before the children.
func (f pathFilter) descend(dir string) bool {
	if f.all() {
		return true
	}
	for _, p := range f.prefixes {
		if p == dir || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
}

// walkTree visits every fileEntry in the Merkle tree rooted at rootHash.
func walkTree(load func(hash string) (*treeNode, error), rootHash string, fn func(fileEntry) error) error {
	return walkTreeWithPrefix(load, rootHash, "", pathFilter{}, fn)
}

func walkTreeWithPrefix(
	load func(hash string) (*treeNode, error),
	hash, prefix string,
	filter pathFilter,
	fn func(fileEntry) error,
) error {
	node, err := load(hash)
	if err != nil {
		return err
	}
	for _, e := range node.Entries {
		fullPath := e.Name
		if prefix != "" {
			fullPath = prefix + "/" + e.Name
		}
		if err := visitTreeEntry(load, e, fullPath, filter, fn); err != nil {
			return err
		}
	}
	return nil
}

// visitTreeEntry hands one tree entry to fn, or skips it when the filter does
// not want it, and descends into a directory's own node.
func visitTreeEntry(
	load func(hash string) (*treeNode, error),
	e treeEntry,
	fullPath string,
	filter pathFilter,
	fn func(fileEntry) error,
) error {
	info := e.fileInfo(fullPath)

	switch e.Type {
	case FileTypeDirectory:
		if !filter.descend(fullPath) {
			return nil
		}
		info.Path += "/"
		info.Size = 0
		if err := fn(fileEntry{FileInfo: info}); err != nil {
			return err
		}
		if e.TreeHash == "" {
			return nil
		}
		return walkTreeWithPrefix(load, e.TreeHash, fullPath, filter, fn)

	case FileTypeSymlink, FileTypeHardlink:
		if !filter.keep(fullPath) {
			return nil
		}
		return fn(fileEntry{FileInfo: info})

	case FileTypeRegular:
		if !filter.keep(fullPath) {
			return nil
		}
		return fn(fileEntry{FileInfo: info, Chunks: e.Chunks})

	case FileTypeUnsupported:
		// Never stored, and loadTreeNode refuses a node that holds one.
	}
	return nil
}

// errNoTreeEntry marks a lookup that found no entry, as opposed to one whose
// node failed to load, whatever that error wraps.
var errNoTreeEntry = fmt.Errorf("%w", os.ErrNotExist)

// lookupTreeEntry returns the entry at path, a slash path relative to the
// tree rooted at rootHash, loading each node with load. A missing entry wraps
// [os.ErrNotExist] and errNoTreeEntry; a node that does not load wraps neither.
func lookupTreeEntry(load func(hash string) (*treeNode, error), rootHash, path string) (treeEntry, error) {
	hash := rootHash
	parts := strings.Split(normEntryPath(path), "/")
	for i, name := range parts {
		node, err := load(hash)
		if err != nil {
			return treeEntry{}, err
		}
		idx, found := slices.BinarySearchFunc(node.Entries, name, func(e treeEntry, n string) int {
			return strings.Compare(e.Name, n)
		})
		if !found {
			break
		}
		e := node.Entries[idx]
		if i == len(parts)-1 {
			return e, nil
		}
		if e.Type != FileTypeDirectory || e.TreeHash == "" {
			break
		}
		hash = e.TreeHash
	}
	return treeEntry{}, fmt.Errorf("tree: no entry at %s: %w", path, errNoTreeEntry)
}

func normEntryPath(path string) string {
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimSuffix(path, "/")
	if path == "." {
		return ""
	}
	return path
}

func normParent(path string) string {
	dir := filepath.Dir(path)
	if dir == "." {
		return ""
	}
	return dir
}

// collectLiveObjects walks the tree rooted at hash and records every object it
// reaches, trees and chunks in their own set because they live in separate
// namespaces.
func collectLiveObjects(
	ctx context.Context,
	trees *objectStore,
	hash string,
	liveTrees, liveChunks map[string]struct{},
) error {
	if _, seen := liveTrees[hash]; seen {
		return nil
	}
	liveTrees[hash] = struct{}{}
	node, err := loadTreeNode(ctx, trees, hash)
	if err != nil {
		return err
	}
	for _, e := range node.Entries {
		for _, c := range e.Chunks {
			liveChunks[c.ID] = struct{}{}
		}
		if e.TreeHash != "" {
			if err := collectLiveObjects(ctx, trees, e.TreeHash, liveTrees, liveChunks); err != nil {
				return err
			}
		}
	}
	return nil
}
