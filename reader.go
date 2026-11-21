package ark

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// treeNodeCacheEntries caps the tree entries a Reader keeps parsed, about
// 50 MiB at a few hundred bytes per entry. The tree packs under them have
// their own cache.
const treeNodeCacheEntries = 1 << 18

// Reader is a read session over a repository: one shared lock, one tree and
// chunk index and one chunk cache, shared by every snapshot it opens. A
// mount or a file server browsing many snapshots opens one Reader instead of
// taking a lock per call.
//
// The shared lock is held until Close, so a Prune, which needs the exclusive
// lock, waits or fails for as long as the Reader is open. A Forget still
// runs: it deletes the manifest only, and what the Reader reads stays stored.
//
// The file systems the Reader hands out take no context: the context passed
// to OpenReader bounds every read of the session, and Close cancels the reads
// still running. A Reader is safe for concurrent use.
type Reader struct {
	repo    *Repository
	ctx     context.Context
	cancel  context.CancelFunc
	lock    *repoLock
	trees   *objectStore
	fetcher *chunkFetcher
	nodes   *nodeCache

	// mu is held shared by every call that reads and exclusively by Close,
	// so Close waits for the calls running and no call starts after it.
	mu     sync.RWMutex
	closed bool
	once   sync.Once
}

// OpenReader takes the shared lock and returns a read session that holds it
// until Close. The lock blocks Prune for the session's lifetime; see Reader.
// Canceling ctx ends the session's reads, Close still has to be called.
func (r *Repository) OpenReader(ctx context.Context) (*Reader, error) {
	lockCtx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(lockCtx)
	chunks, trees := r.stores()
	return &Reader{
		repo:    r,
		ctx:     ctx,
		cancel:  cancel,
		lock:    lock,
		trees:   trees,
		fetcher: r.newChunkFetcher(ctx, chunks, newChunkCache(defaultRestoreCache)),
		nodes:   newNodeCache(treeNodeCacheEntries),
	}, nil
}

// Close cancels the reads still running, waits for the calls in progress and
// releases the shared lock, which unblocks Prune. No goroutine of the session
// outlives it. Files opened from the session fail with [fs.ErrClosed] after it.
// Close is safe to call more than once.
func (rd *Reader) Close() error {
	rd.once.Do(func() {
		rd.cancel()
		rd.mu.Lock()
		rd.closed = true
		rd.mu.Unlock()
		rd.fetcher.close()
		rd.lock.release(rd.ctx)
	})
	return nil
}

func (rd *Reader) enter() error {
	rd.mu.RLock()
	if rd.closed {
		rd.mu.RUnlock()
		return fs.ErrClosed
	}
	return nil
}

func (rd *Reader) leave() { rd.mu.RUnlock() }

// Snapshots returns the snapshots matching filter, newest first, as
// Repository.Snapshots does, under the session's lock. Canceling ctx or the
// session's context, or closing the Reader, ends the call.
func (rd *Reader) Snapshots(ctx context.Context, filter SnapshotFilter) ([]*Snapshot, error) {
	if err := rd.enter(); err != nil {
		return nil, fmt.Errorf("ark: reader: %w", err)
	}
	defer rd.leave()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(rd.ctx, cancel)()
	return rd.repo.Snapshots(ctx, filter)
}

// FS returns the file system of one snapshot. snapshotID is a full snapshot
// ID or a unique prefix; an unknown one wraps ErrNotFound. The file system
// stays usable until the Reader is closed, also when the snapshot is
// forgotten in the meantime. The session loads each index once, at its first
// read, so a snapshot stored after that may hold trees and chunks it does not
// find; a new Reader reads it.
func (rd *Reader) FS(snapshotID SnapshotID) (*SnapshotFS, error) {
	if err := rd.enter(); err != nil {
		return nil, fmt.Errorf("ark: reader: %w", err)
	}
	defer rd.leave()
	id, err := rd.repo.resolveID(rd.ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	m, err := rd.repo.loadManifest(rd.ctx, id)
	if err != nil {
		return nil, err
	}
	return &SnapshotFS{rd: rd, root: m.RootTree, time: m.CreatedAt}, nil
}

// SnapshotFS is the read-only file system of one snapshot, from
// Reader.FS. Names are io/fs paths relative to the snapshot root, "." for
// the root itself.
//
// Every entry's FileInfo carries mode, size and modification time from the
// snapshot, and Sys returns the entry as an ark.FileInfo, with Path set to
// the io/fs name. The root directory has no stored entry: it reads as mode
// 0755 with the snapshot's creation time.
//
// Regular files implement [io.ReaderAt], [io.Seeker] and [io.Reader]; ReadAt is
// safe for concurrent use and reads only the chunks it overlaps. A hardlink
// reads as a regular file with its target's content. Symlinks are not
// followed: Lstat and ReadLink serve them, while Open and Stat on one fail
// with an error wrapping [fs.ErrInvalid], because a link's target is often a
// path of the host the snapshot was taken on.
type SnapshotFS struct {
	rd   *Reader
	root string
	time time.Time
}

var (
	_ fs.StatFS     = (*SnapshotFS)(nil)
	_ fs.ReadDirFS  = (*SnapshotFS)(nil)
	_ fs.ReadLinkFS = (*SnapshotFS)(nil)
)

// Open opens the named file or directory.
func (s *SnapshotFS) Open(name string) (fs.File, error) {
	e, err := s.resolve("open", name)
	if err != nil {
		return nil, err
	}
	switch e.te.Type {
	case FileTypeDirectory:
		return &dirFile{fsys: s, e: e}, nil
	case FileTypeSymlink:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	case FileTypeRegular, FileTypeHardlink, FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice,
		FileTypeUnsupported:
	}
	c := &content{rd: s.rd, chunks: e.chunks}
	return &file{sr: io.NewSectionReader(c, 0, e.size), e: e}, nil
}

// Stat returns the FileInfo of the named entry. It fails on a symlink, as
// Open does; Lstat describes one.
func (s *SnapshotFS) Stat(name string) (fs.FileInfo, error) {
	e, err := s.resolve("stat", name)
	if err != nil {
		return nil, err
	}
	if e.te.Type == FileTypeSymlink {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	return e, nil
}

// Lstat returns the FileInfo of the named entry without following a symlink.
func (s *SnapshotFS) Lstat(name string) (fs.FileInfo, error) {
	return s.resolve("lstat", name)
}

// ReadLink returns the target of the named symlink as stored.
func (s *SnapshotFS) ReadLink(name string) (string, error) {
	e, err := s.resolve("readlink", name)
	if err != nil {
		return "", err
	}
	if e.te.Type != FileTypeSymlink {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	return e.te.LinkDest, nil
}

// ReadDir returns the entries of the named directory, sorted by name.
func (s *SnapshotFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	if err := s.rd.enter(); err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	defer s.rd.leave()
	e, err := s.find(name)
	if err == nil {
		var list []fs.DirEntry
		if list, err = s.list(e); err == nil {
			return list, nil
		}
	}
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
}

// resolve returns the entry at name for the call op.
func (s *SnapshotFS) resolve(op, name string) (*fsEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if err := s.rd.enter(); err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	defer s.rd.leave()
	e, err := s.find(name)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return e, nil
}

// find returns the entry at name, a valid io/fs path. The caller has entered.
func (s *SnapshotFS) find(name string) (*fsEntry, error) {
	if name == "." {
		root := treeEntry{Type: FileTypeDirectory, Mode: 0o755, ModTime: s.time, TreeHash: s.root}
		return &fsEntry{path: ".", te: root}, nil
	}
	te, err := s.entryAt(name)
	if err != nil {
		return nil, err
	}
	return s.complete(name, te)
}

// entryAt walks the tree to the stored entry at name.
func (s *SnapshotFS) entryAt(name string) (treeEntry, error) {
	return lookupTreeEntry(s.node, s.root, name)
}

// complete makes the entry at name out of its stored entry, taking a
// hardlink's content from its target.
func (s *SnapshotFS) complete(name string, te treeEntry) (*fsEntry, error) {
	e := &fsEntry{path: name, te: te, size: te.Size, chunks: te.Chunks}
	switch te.Type {
	case FileTypeDirectory, FileTypeSymlink:
		e.size = 0
	case FileTypeHardlink:
		target, err := linkTarget(s.node, s.root, te.LinkDest)
		if err != nil {
			return nil, fmt.Errorf("hardlink %s to %s: %w", name, te.LinkDest, err)
		}
		e.size, e.chunks = target.Size, target.Chunks
	case FileTypeRegular, FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice, FileTypeUnsupported:
	}
	return e, nil
}

// list returns the entries of the directory e. A hardlink's target is
// resolved only by its entry's Info, so a listing does not walk to every
// target and a broken link fails alone.
func (s *SnapshotFS) list(e *fsEntry) ([]fs.DirEntry, error) {
	if e.te.Type != FileTypeDirectory {
		return nil, errors.New("not a directory")
	}
	node, err := s.node(e.te.TreeHash)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(node.Entries))
	for _, child := range node.Entries {
		name := child.Name
		if e.path != "." {
			name = e.path + "/" + name
		}
		if child.Type == FileTypeHardlink {
			out = append(out, &linkEntry{fsys: s, e: &fsEntry{path: name, te: child}})
			continue
		}
		ce, err := s.complete(name, child)
		if err != nil {
			return nil, err
		}
		out = append(out, ce)
	}
	return out, nil
}

// node returns the parsed tree node hash, the empty node for no hash.
func (s *SnapshotFS) node(hash string) (*treeNode, error) {
	if hash == "" {
		return &treeNode{}, nil
	}
	return s.rd.nodes.load(hash, func() (*treeNode, error) {
		return loadTreeNode(s.rd.ctx, s.rd.trees, hash)
	})
}

// fsEntry is one entry of a SnapshotFS, as [fs.FileInfo] and [fs.DirEntry].
type fsEntry struct {
	path   string
	te     treeEntry
	size   int64      // content size, a hardlink's target's
	chunks []chunkRef // content, a hardlink's target's
}

func (e *fsEntry) Name() string       { return path.Base(e.path) }
func (e *fsEntry) Size() int64        { return e.size }
func (e *fsEntry) ModTime() time.Time { return e.te.ModTime }
func (e *fsEntry) IsDir() bool        { return e.te.Type == FileTypeDirectory }
func (e *fsEntry) Type() fs.FileMode  { return e.Mode().Type() }

func (e *fsEntry) Mode() fs.FileMode {
	m := goMode(e.te.Mode)
	switch e.te.Type {
	case FileTypeDirectory:
		m |= fs.ModeDir
	case FileTypeSymlink:
		m |= fs.ModeSymlink
	case FileTypeRegular, FileTypeHardlink, FileTypeFifo, FileTypeCharDevice, FileTypeBlockDevice,
		FileTypeUnsupported:
	}
	return m
}

// Sys returns the stored entry as an ark.FileInfo, Path the io/fs name and
// Size the content size.
func (e *fsEntry) Sys() any {
	info := e.te.fileInfo(e.path)
	info.Size = e.size
	return info
}

func (e *fsEntry) Info() (fs.FileInfo, error) { return e, nil }

// linkEntry is a hardlink in a directory listing, its target not yet
// resolved.
type linkEntry struct {
	fsys *SnapshotFS
	e    *fsEntry // the stored entry
}

func (l *linkEntry) Name() string      { return l.e.Name() }
func (l *linkEntry) IsDir() bool       { return false }
func (l *linkEntry) Type() fs.FileMode { return l.e.Type() }

// Info resolves the link's target, failing as Stat does on a broken link.
func (l *linkEntry) Info() (fs.FileInfo, error) {
	if err := l.fsys.rd.enter(); err != nil {
		return nil, &fs.PathError{Op: "stat", Path: l.e.path, Err: err}
	}
	defer l.fsys.rd.leave()
	e, err := l.fsys.complete(l.e.path, l.e.te)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: l.e.path, Err: err}
	}
	return e, nil
}

// dirFile is an open directory of a SnapshotFS.
type dirFile struct {
	fsys   *SnapshotFS
	e      *fsEntry
	list   []fs.DirEntry
	read   bool
	closed bool
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.e, nil }

func (d *dirFile) Close() error {
	if d.closed {
		return &fs.PathError{Op: "close", Path: d.e.path, Err: fs.ErrClosed}
	}
	d.closed = true
	return nil
}

func (d *dirFile) Read([]byte) (int, error) {
	err := fs.ErrInvalid
	if d.closed {
		err = fs.ErrClosed
	}
	return 0, &fs.PathError{Op: "read", Path: d.e.path, Err: err}
}

// ReadDir returns the next n entries, all that are left for n <= 0.
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, &fs.PathError{Op: "readdir", Path: d.e.path, Err: fs.ErrClosed}
	}
	if !d.read {
		if err := d.fsys.rd.enter(); err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: d.e.path, Err: err}
		}
		list, err := d.fsys.list(d.e)
		d.fsys.rd.leave()
		if err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: d.e.path, Err: err}
		}
		d.list, d.read = list, true
	}
	if n <= 0 {
		out := d.list
		d.list = nil
		return out, nil
	}
	if len(d.list) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.list))
	out := d.list[:n:n]
	d.list = d.list[n:]
	return out, nil
}

// file is an open regular file or hardlink of a SnapshotFS.
type file struct {
	sr     *io.SectionReader
	e      *fsEntry
	closed atomic.Bool
}

func (f *file) Stat() (fs.FileInfo, error) { return f.e, nil }

func (f *file) Close() error {
	if f.closed.Swap(true) {
		return f.errClosed("close")
	}
	return nil
}

func (f *file) Read(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, f.errClosed("read")
	}
	return f.sr.Read(p)
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, f.errClosed("read")
	}
	return f.sr.ReadAt(p, off)
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.closed.Load() {
		return 0, f.errClosed("seek")
	}
	return f.sr.Seek(offset, whence)
}

func (f *file) errClosed(op string) error {
	return &fs.PathError{Op: op, Path: f.e.path, Err: fs.ErrClosed}
}

// content is the bytes of one file, read by chunk through the Reader's
// fetcher.
type content struct {
	rd     *Reader
	chunks []chunkRef
}

// ReadAt fills p from the chunks that overlap [off, off+len(p)), which it
// wants all at once so their reads run side by side. [io.SectionReader] keeps p
// within the file, so a short read means the chunks end before the size.
func (c *content) ReadAt(p []byte, off int64) (int, error) {
	if err := c.rd.enter(); err != nil {
		return 0, fmt.Errorf("ark: reader: %w", err)
	}
	defer c.rd.leave()
	if len(p) == 0 {
		return 0, nil
	}
	end := off + int64(len(p))
	first := sort.Search(len(c.chunks), func(i int) bool { return c.chunks[i].Offset+c.chunks[i].Length > off })
	last := first
	for last < len(c.chunks) && c.chunks[last].Offset < end {
		last++
	}
	flights := make([]*flight, last-first)
	for i := range flights {
		flights[i] = c.rd.fetcher.want(c.chunks[first+i].ID)
	}
	n := 0
	for i, fl := range flights {
		ref := c.chunks[first+i]
		<-fl.done
		var err error
		at := off + int64(n) - ref.Offset
		switch {
		case fl.err != nil:
			err = fmt.Errorf("ark: failed to fetch chunk: %w", fl.err)
		case at < 0 || at >= ref.Length || int64(len(fl.data)) != ref.Length:
			err = fmt.Errorf("%w: chunk %s does not continue the file at %d", ErrCorruptTree, ref.ID, off+int64(n))
		}
		if err != nil {
			for _, rest := range flights[i+1:] {
				c.rd.fetcher.drop(rest)
			}
			return n, err
		}
		n += copy(p[n:], fl.data[at:])
	}
	if n < len(p) {
		return n, fmt.Errorf("%w: file content ends at %d: %w", ErrCorruptTree, off+int64(n), io.ErrUnexpectedEOF)
	}
	return n, nil
}

// nodeCache is an LRU of parsed tree nodes keyed by tree hash, bounded by the
// entries the nodes hold. A node larger than the bound is held alone.
type nodeCache struct {
	mu      sync.Mutex
	limit   int
	size    int
	order   *list.List // front is most recently used, values are *cachedNode
	items   map[string]*list.Element
	loading map[string]*nodeLoad
}

// nodeLoad is one load of a node, shared by every caller missing it.
type nodeLoad struct {
	done chan struct{}
	node *treeNode
	err  error
}

type cachedNode struct {
	hash string
	node *treeNode
}

func newNodeCache(limit int) *nodeCache {
	return &nodeCache{
		limit:   limit,
		order:   list.New(),
		items:   make(map[string]*list.Element),
		loading: make(map[string]*nodeLoad),
	}
}

// load returns the node hash from the cache, or from fn, which runs once for
// all the callers that miss the same hash at the same time.
func (c *nodeCache) load(hash string, fn func() (*treeNode, error)) (*treeNode, error) {
	c.mu.Lock()
	if n, ok := c.get(hash); ok {
		c.mu.Unlock()
		return n, nil
	}
	if l, ok := c.loading[hash]; ok {
		c.mu.Unlock()
		<-l.done
		return l.node, l.err
	}
	l := &nodeLoad{done: make(chan struct{})}
	c.loading[hash] = l
	c.mu.Unlock()

	l.node, l.err = fn()
	c.mu.Lock()
	delete(c.loading, hash)
	if l.err == nil {
		c.put(hash, l.node)
	}
	c.mu.Unlock()
	close(l.done)
	return l.node, l.err
}

// get and put run under c.mu.
func (c *nodeCache) get(hash string) (*treeNode, bool) {
	el, ok := c.items[hash]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	cn, ok := el.Value.(*cachedNode)
	if !ok {
		return nil, false
	}
	return cn.node, true
}

func (c *nodeCache) put(hash string, n *treeNode) {
	if _, ok := c.items[hash]; ok {
		return
	}
	c.items[hash] = c.order.PushFront(&cachedNode{hash: hash, node: n})
	c.size += len(n.Entries)
	for c.size > c.limit && c.order.Len() > 1 {
		old, ok := c.order.Remove(c.order.Back()).(*cachedNode)
		if !ok {
			return
		}
		delete(c.items, old.hash)
		c.size -= len(old.node.Entries)
	}
}
