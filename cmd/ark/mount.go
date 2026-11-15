//go:build !(linux && (386 || arm || mips || mipsle))

package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkuri"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/urfave/cli/v3"
)

// mountCacheTimeout is how long the kernel may cache entries and attributes:
// a snapshot never changes and the snapshot list is fixed at mount start.
const mountCacheTimeout = time.Hour

func mountCommand() *cli.Command {
	return &cli.Command{
		Name:      "mount",
		Usage:     "serve every snapshot read-only through FUSE",
		ArgsUsage: "<dir>",
		UsageText: `ark mount [options] <dir>

   Mounts the repository's snapshots read-only at dir until Ctrl-C:
   ids/<id>/, names/<name>/<time>/ with a latest link, tags/<tag>/<time>/ and
   hosts/<host>/<time>/. The snapshot list is the one at mount start. The mount
   holds a shared lock, so prune waits until it is unmounted.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 1 {
				return exitcode.Usagef("usage: ark mount [options] <dir>")
			}
			dir := cmd.Args().First()
			uri, err := repoURI(ctx, cmd)
			if err != nil {
				return err
			}
			if err := checkMountpoint(dir, uri); err != nil {
				return err
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			// ctx ends on Ctrl-C, which only starts the unmount: reads keep
			// running until the kernel lets go of the mount.
			rd, err := repo.OpenReader(context.WithoutCancel(ctx))
			if err != nil {
				return err
			}
			defer rd.Close()

			snaps, err := rd.Snapshots(ctx, ark.SnapshotFilter{})
			if err != nil {
				return err
			}
			root := &mountRoot{layout: buildLayout(snaps), fsys: map[string]*ark.SnapshotFS{}}
			for _, s := range snaps {
				if root.fsys[s.ID], err = rd.FS(s.ID); err != nil {
					return err
				}
			}

			p := printer(cmd)
			p.Notef("the mount holds a shared lock: prune is blocked until %s is unmounted", dir)
			timeout := mountCacheTimeout
			srv, err := gofs.Mount(dir, root, &gofs.Options{
				FsName:          "ark",
				Name:            "ark",
				Options:         []string{"ro"},
				EntryTimeout:    &timeout,
				AttrTimeout:     &timeout,
				NegativeTimeout: &timeout,
				NullPermissions: true,
			})
			if err != nil {
				return fmt.Errorf("mount %s: %w", dir, err)
			}

			served := make(chan struct{})
			go func() {
				srv.Wait()
				close(served)
			}()
			select {
			case <-served:
			case <-ctx.Done():
				if err := srv.Unmount(); err != nil {
					return fmt.Errorf("unmount %s: %w", dir, err)
				}
				<-served
			}
			return nil
		},
	}
}

// checkMountpoint refuses a mountpoint and a local repository of which one
// contains the other: the mount would list itself, or hide the repository.
func checkMountpoint(dir, uri string) error {
	repo, ok := arkuri.LocalPath(uri)
	if !ok {
		return nil
	}
	m, err := realPath(dir)
	if err != nil {
		return fmt.Errorf("mount %s: %w", dir, err)
	}
	r, err := realPath(repo)
	if err != nil {
		return fmt.Errorf("repository %s: %w", repo, err)
	}
	if within(m, r) || within(r, m) {
		return exitcode.Usagef("mount %s: the mountpoint and the repository %s must not contain each other", dir, repo)
	}
	return nil
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// within reports whether p is parent or below it.
func within(p, parent string) bool {
	rel, err := filepath.Rel(parent, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// vdir is a directory of the mount's fixed layout above the snapshots.
type vdir struct {
	dirs  map[string]*vdir
	snaps map[string]string // entry name to snapshot ID
	links map[string]string // entry name to symlink target
}

func newVdir() *vdir {
	return &vdir{dirs: map[string]*vdir{}, snaps: map[string]string{}, links: map[string]string{}}
}

func (d *vdir) dir(name string) *vdir {
	sub, ok := d.dirs[name]
	if !ok {
		sub = newVdir()
		d.dirs[name] = sub
	}
	return sub
}

// buildLayout lays out snaps as ids/, names/, tags/ and hosts/.
func buildLayout(snaps []*ark.Snapshot) *vdir {
	root := newVdir()
	ids := root.dir("ids")
	shorts := map[string]int{}
	for _, s := range snaps {
		shorts[out.ShortID(s.ID)]++
	}
	for _, s := range snaps {
		id := out.ShortID(s.ID)
		if shorts[id] > 1 {
			id = s.ID
		}
		ids.snaps[escapeName(id)] = s.ID
	}

	groups := map[string]map[string][]*ark.Snapshot{"names": {}, "tags": {}, "hosts": {}}
	for _, s := range snaps {
		add := func(kind, key string) {
			key = escapeName(key)
			groups[kind][key] = append(groups[kind][key], s)
		}
		add("names", s.SourceName)
		add("hosts", s.Host)
		for _, t := range slices.Compact(slices.Sorted(slices.Values(s.Tags))) {
			add("tags", t)
		}
	}
	for kind, byKey := range groups {
		top := root.dir(kind)
		for key, list := range byKey {
			d := top.dir(key)
			newest := ""
			for _, s := range sortByTime(list) {
				newest = timeName(d, s.CreatedAt)
				d.snaps[newest] = s.ID
			}
			if kind == "names" {
				d.links["latest"] = newest
			}
		}
	}
	return root
}

func sortByTime(list []*ark.Snapshot) []*ark.Snapshot {
	return slices.SortedFunc(slices.Values(list), func(a, b *ark.Snapshot) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// timeName is the entry name of a snapshot taken at t in d, with a -1, -2
// suffix when an older snapshot already took the same second.
func timeName(d *vdir, t time.Time) string {
	base := out.TimeLong(t)
	name := base
	for i := 1; d.snaps[name] != ""; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// escapeName makes s a valid, visible directory name: "/" becomes "_", a
// leading "." gets a "_" in front and the empty name is "_".
func escapeName(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	if s == "" || s[0] == '.' {
		s = "_" + s
	}
	return s
}

// inode derives a stable inode number from parent and name. 0 is invalid and
// 1 is the root.
func inode(parent uint64, name string) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for i := range b {
		b[i] = byte(parent >> (8 * i))
	}
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(name))
	ino := h.Sum64()
	if ino < 2 {
		ino += 2
	}
	return ino
}

// mountRoot builds the fixed layout when the mount starts.
type mountRoot struct {
	gofs.Inode
	layout *vdir
	fsys   map[string]*ark.SnapshotFS
}

var _ gofs.NodeOnAdder = (*mountRoot)(nil)

func (r *mountRoot) OnAdd(ctx context.Context) {
	r.add(ctx, &r.Inode, r.layout, 1)
}

func (r *mountRoot) add(ctx context.Context, parent *gofs.Inode, d *vdir, ino uint64) {
	for name, sub := range d.dirs {
		cino := inode(ino, name)
		ch := parent.NewPersistentInode(ctx, &layoutDir{}, gofs.StableAttr{Mode: syscall.S_IFDIR, Ino: cino})
		parent.AddChild(name, ch, false)
		r.add(ctx, ch, sub, cino)
	}
	for name, id := range d.snaps {
		cino := inode(ino, name)
		n := &snapNode{fsys: r.fsys[id], name: ".", root: cino}
		ch := parent.NewPersistentInode(ctx, n, gofs.StableAttr{Mode: syscall.S_IFDIR, Ino: cino})
		parent.AddChild(name, ch, false)
	}
	for name, target := range d.links {
		l := &gofs.MemSymlink{Attr: fuse.Attr{Mode: 0o777}, Data: []byte(target)}
		ch := parent.NewPersistentInode(ctx, l, gofs.StableAttr{Mode: syscall.S_IFLNK, Ino: inode(ino, name)})
		parent.AddChild(name, ch, false)
	}
}

// layoutDir is a directory of the layout; go-fuse serves its children.
type layoutDir struct{ gofs.Inode }

var _ gofs.NodeGetattrer = (*layoutDir)(nil)

func (*layoutDir) Getattr(_ context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

// snapNode is an entry of one snapshot, name its io/fs path. Inode numbers
// hash the snapshot root's inode and the path, so a hardlink, numbered by its
// target's path, shares its target's inode.
type snapNode struct {
	gofs.Inode
	fsys *ark.SnapshotFS
	name string
	root uint64
}

var (
	_ gofs.NodeLookuper   = (*snapNode)(nil)
	_ gofs.NodeReaddirer  = (*snapNode)(nil)
	_ gofs.NodeGetattrer  = (*snapNode)(nil)
	_ gofs.NodeOpener     = (*snapNode)(nil)
	_ gofs.NodeReadlinker = (*snapNode)(nil)
)

func (n *snapNode) child(name string) string {
	if n.name == "." {
		return name
	}
	return n.name + "/" + name
}

// ino is the inode of the entry at name with info.
func (n *snapNode) ino(name string, info fs.FileInfo) uint64 {
	if name == "." {
		return n.root
	}
	if fi, ok := info.Sys().(ark.FileInfo); ok && fi.Type == ark.FileTypeHardlink {
		name = path.Clean(strings.TrimPrefix(fi.LinkDest, "./"))
	}
	return inode(n.root, name)
}

func (n *snapNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p := n.child(name)
	info, err := n.fsys.Lstat(p)
	if err != nil {
		return nil, errno(p, err)
	}
	ino := n.ino(p, info)
	fillAttr(&out.Attr, info, ino)
	ch := &snapNode{fsys: n.fsys, name: p, root: n.root}
	return n.NewInode(ctx, ch, gofs.StableAttr{Mode: out.Mode & syscall.S_IFMT, Ino: ino}), 0
}

func (n *snapNode) Readdir(context.Context) (gofs.DirStream, syscall.Errno) {
	list, err := n.fsys.ReadDir(n.name)
	if err != nil {
		return nil, errno(n.name, err)
	}
	entries := make([]fuse.DirEntry, 0, len(list))
	for _, e := range list {
		p := n.child(e.Name())
		ino := inode(n.root, p)
		if info, err := e.Info(); err == nil {
			ino = n.ino(p, info)
		}
		entries = append(entries, fuse.DirEntry{Name: e.Name(), Mode: fileMode(e.Type()), Ino: ino})
	}
	return gofs.NewListDirStream(entries), 0
}

func (n *snapNode) Getattr(_ context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	info, err := n.fsys.Lstat(n.name)
	if err != nil {
		return errno(n.name, err)
	}
	fillAttr(&out.Attr, info, n.ino(n.name, info))
	return 0
}

func (n *snapNode) Open(context.Context, uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	f, err := n.fsys.Open(n.name)
	if err != nil {
		return nil, 0, errno(n.name, err)
	}
	ra, ok := f.(io.ReaderAt)
	if !ok {
		_ = f.Close()
		return nil, 0, syscall.EISDIR
	}
	return &snapFile{f: f, ra: ra, name: n.name}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *snapNode) Readlink(context.Context) ([]byte, syscall.Errno) {
	target, err := n.fsys.ReadLink(n.name)
	if err != nil {
		return nil, errno(n.name, err)
	}
	return []byte(target), 0
}

// snapFile is an open regular file of a snapshot.
type snapFile struct {
	f    fs.File
	ra   io.ReaderAt
	name string
}

var (
	_ gofs.FileReader   = (*snapFile)(nil)
	_ gofs.FileReleaser = (*snapFile)(nil)
)

func (f *snapFile) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.ra.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errno(f.name, err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *snapFile) Release(context.Context) syscall.Errno {
	_ = f.f.Close()
	return 0
}

func fillAttr(a *fuse.Attr, info fs.FileInfo, ino uint64) {
	a.Ino = ino
	a.Mode = fileMode(info.Mode())
	a.Size = uint64(max(info.Size(), 0))
	a.Nlink = 1
	mtime := info.ModTime()
	a.SetTimes(&mtime, &mtime, &mtime)
	if fi, ok := info.Sys().(ark.FileInfo); ok {
		a.Uid = uint32(max(fi.UID, 0))
		a.Gid = uint32(max(fi.GID, 0))
		if !fi.ChangeTime.IsZero() {
			a.SetTimes(nil, nil, &fi.ChangeTime)
		}
	}
}

// fileMode converts an io/fs mode to the stat mode FUSE expects.
func fileMode(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	switch {
	case m.IsDir():
		mode |= syscall.S_IFDIR
	case m&fs.ModeSymlink != 0:
		mode |= syscall.S_IFLNK
	default:
		mode |= syscall.S_IFREG
	}
	if m&fs.ModeSetuid != 0 {
		mode |= syscall.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= syscall.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= syscall.S_ISVTX
	}
	return mode
}

// errno maps a read error to the errno the kernel gets. Anything but a
// missing or invalid entry is logged: the kernel only sees EIO.
func errno(name string, err error) syscall.Errno {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, fs.ErrInvalid):
		return syscall.EINVAL
	}
	slog.Error("mount: read failed", "path", name, "err", err)
	return syscall.EIO
}
