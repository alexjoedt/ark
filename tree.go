package ark

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// treeNode is a directory node in the snapshot Merkle tree. It lives in the
// trees namespace, named by the HMAC of its canonical JSON: entries sorted
// by name, marshalled without indentation.
type treeNode struct {
	Entries []treeEntry `json:"entries"`
}

// treeEntry is one child inside a treeNode.
type treeEntry struct {
	Name     string      `json:"name"`
	Type     FileType    `json:"type"`
	Mode     os.FileMode `json:"mode"`
	UID      int         `json:"uid"`
	GID      int         `json:"gid"`
	ModTime  time.Time   `json:"mtime"`
	Size     int64       `json:"size,omitempty"`
	LinkDest string      `json:"link_dest,omitempty"`
	TreeHash string      `json:"tree_hash,omitempty"`
	Chunks   []chunkRef  `json:"chunks,omitempty"`

	// Xattrs maps an extended attribute name to its value. encoding/json
	// writes the values base64 encoded, which is the v1 wire format.
	Xattrs map[string][]byte `json:"xattrs,omitempty"`
}

// storeTreeNode canonicalises node and writes it to the trees namespace.
// Sorting here rather than at every call site keeps the canonical form an
// invariant of the write path.
func storeTreeNode(ctx context.Context, trees *objectStore, node *treeNode) (string, error) {
	slices.SortFunc(node.Entries, func(a, b treeEntry) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.Marshal(node)
	if err != nil {
		return "", fmt.Errorf("tree: failed to marshal node: %w", err)
	}
	hash, _, err := trees.Put(ctx, data)
	if err != nil {
		return "", fmt.Errorf("tree: failed to store node: %w", err)
	}
	return hash, nil
}

func loadTreeNode(ctx context.Context, trees *objectStore, hash string) (*treeNode, error) {
	data, err := trees.Get(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("tree: failed to load node %s: %w", hash, err)
	}
	var node treeNode
	if err := json.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("tree: failed to unmarshal node %s: %w", hash, err)
	}
	return &node, nil
}

// buildTree organises the flat fileEntry list into a Merkle directory tree,
// stores every treeNode blob in the CAS, and returns the root tree hash.
func buildTree(ctx context.Context, trees *objectStore, entries []fileEntry) (string, error) {
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

	var buildNode func(dirPath string) (string, error)
	buildNode = func(dirPath string) (string, error) {
		node := &treeNode{Entries: make([]treeEntry, 0, len(dirChildren[dirPath]))}
		for _, child := range dirChildren[dirPath] {
			norm := normEntryPath(child.Path)
			te := treeEntry{
				Name:     filepath.Base(norm),
				Type:     child.Type,
				Mode:     child.Mode,
				UID:      child.UID,
				GID:      child.GID,
				ModTime:  child.ModTime,
				Size:     child.Size,
				LinkDest: child.LinkDest,
				Chunks:   child.Chunks,
				Xattrs:   child.Xattrs,
			}
			if child.Type == FileTypeDirectory {
				hash, err := buildNode(norm)
				if err != nil {
					return "", err
				}
				te.TreeHash = hash
				te.Chunks = nil
			}
			node.Entries = append(node.Entries, te)
		}
		return storeTreeNode(ctx, trees, node)
	}

	return buildNode("")
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
func walkTree(ctx context.Context, trees *objectStore, rootHash string, fn func(fileEntry) error) error {
	return walkTreeWithPrefix(ctx, trees, rootHash, "", pathFilter{}, fn)
}

func walkTreeWithPrefix(
	ctx context.Context,
	trees *objectStore,
	hash, prefix string,
	filter pathFilter,
	fn func(fileEntry) error,
) error {
	node, err := loadTreeNode(ctx, trees, hash)
	if err != nil {
		return err
	}
	for _, e := range node.Entries {
		fullPath := e.Name
		if prefix != "" {
			fullPath = prefix + "/" + e.Name
		}
		if err := visitTreeEntry(ctx, trees, e, fullPath, filter, fn); err != nil {
			return err
		}
	}
	return nil
}

// visitTreeEntry hands one tree entry to fn, or skips it when the filter does
// not want it, and descends into a directory's own node.
func visitTreeEntry(
	ctx context.Context,
	trees *objectStore,
	e treeEntry,
	fullPath string,
	filter pathFilter,
	fn func(fileEntry) error,
) error {
	info := FileInfo{
		Path:     fullPath,
		Type:     e.Type,
		Mode:     e.Mode,
		UID:      e.UID,
		GID:      e.GID,
		ModTime:  e.ModTime,
		Size:     e.Size,
		LinkDest: e.LinkDest,
		Xattrs:   e.Xattrs,
	}

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
		return walkTreeWithPrefix(ctx, trees, e.TreeHash, fullPath, filter, fn)

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
		// Never written: the backup drops these before the tree is built.
	}
	return nil
}

// lookupTreeEntry returns the entry at path, a slash path relative to the
// tree rooted at rootHash.
func lookupTreeEntry(ctx context.Context, trees *objectStore, rootHash, path string) (treeEntry, error) {
	hash := rootHash
	parts := strings.Split(normEntryPath(path), "/")
	for i, name := range parts {
		node, err := loadTreeNode(ctx, trees, hash)
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
	return treeEntry{}, fmt.Errorf("tree: no entry at %s", path)
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
