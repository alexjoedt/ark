package ark_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"golang.org/x/sys/unix"
)

// roundTrip backs up root into a fresh in-memory repository and restores the
// snapshot into a new directory. The repository stays open: arkmem drops every
// object on Close, so a second Open would read an empty backend.
func roundTrip(t *testing.T, root string) (*ark.Repository, *ark.Snapshot, string) {
	t.Helper()
	ctx := context.Background()

	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := t.TempDir()
	sink, err := arkfs.NewSink(dst)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return repo, snap, dst
}

// entriesOf lists the snapshot entries by path.
func entriesOf(t *testing.T, repo *ark.Repository, id ark.SnapshotID) map[string]ark.FileInfo {
	t.Helper()
	byPath := make(map[string]ark.FileInfo)
	if err := repo.Walk(context.Background(), id, func(f ark.FileInfo) error {
		byPath[f.Path] = f
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return byPath
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Lstat(a)
	if err != nil {
		t.Fatalf("lstat %s: %v", a, err)
	}
	fb, err := os.Lstat(b)
	if err != nil {
		t.Fatalf("lstat %s: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

func TestHardlinkIsStoredOnceAndRestoredAsALink(t *testing.T) {
	root := t.TempDir()
	// "a.txt" sorts before "z-link.txt", so the walk sees the content first.
	content := []byte("shared contents")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "z-link.txt")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}

	repo, snap, dst := roundTrip(t, root)

	entries := entriesOf(t, repo, snap.ID)
	link, ok := entries["z-link.txt"]
	if !ok {
		t.Fatalf("snapshot has no z-link.txt, got %v", entries)
	}
	if link.Type != ark.FileTypeHardlink {
		t.Errorf("z-link.txt has type %q, want %q", link.Type, ark.FileTypeHardlink)
	}
	if link.LinkDest != "a.txt" {
		t.Errorf("z-link.txt points at %q, want %q", link.LinkDest, "a.txt")
	}
	if link.Size != int64(len(content)) || entries["a.txt"].Size != link.Size {
		t.Errorf(
			"sizes are %d for the link and %d for the file, want %d",
			link.Size,
			entries["a.txt"].Size,
			len(content),
		)
	}
	if entries["a.txt"].Type != ark.FileTypeRegular {
		t.Errorf("a.txt has type %q, want %q", entries["a.txt"].Type, ark.FileTypeRegular)
	}

	// Both names count, the bytes are read once.
	if snap.Stats.FileCount != 2 {
		t.Errorf("stats count %d files, want 2", snap.Stats.FileCount)
	}
	if snap.Stats.TotalSize != int64(len(content)) {
		t.Errorf("stats total size %d, want %d, the content is stored once",
			snap.Stats.TotalSize, len(content))
	}

	if !sameInode(t, filepath.Join(dst, "a.txt"), filepath.Join(dst, "z-link.txt")) {
		t.Error("restored a.txt and z-link.txt are not the same inode")
	}
	got, err := os.ReadFile(filepath.Join(dst, "z-link.txt"))
	if err != nil {
		t.Fatalf("read restored link: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("restored link holds %q, want %q", got, content)
	}
}

func TestXattrsSurviveTheRoundTrip(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("body"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := unix.Setxattr(file, "user.ark", []byte("value"), 0); err != nil {
		t.Skipf("xattrs unsupported here: %v", err)
	}
	if err := unix.Setxattr(root, "user.ark_dir", []byte("dir value"), 0); err != nil {
		t.Skipf("directory xattrs unsupported here: %v", err)
	}

	repo, snap, dst := roundTrip(t, root)

	entries := entriesOf(t, repo, snap.ID)
	if got := string(entries["file.txt"].Xattrs["user.ark"]); got != "value" {
		t.Errorf("snapshot entry carries user.ark = %q, want %q", got, "value")
	}

	// The node holds name and value as they are, each behind its length.
	stored := []byte("\x08user.ark\x05value")
	if !slices.ContainsFunc(blobNames(t, repo, ark.TreeFile), func(name string) bool {
		return bytes.Contains(openObject(t, repo, ark.TreeFile, name), stored)
	}) {
		t.Errorf("no tree node holds %q", stored)
	}

	var buf [64]byte
	n, err := unix.Getxattr(filepath.Join(dst, "file.txt"), "user.ark", buf[:])
	if err != nil {
		t.Fatalf("getxattr on the restored file: %v", err)
	}
	if got := string(buf[:n]); got != "value" {
		t.Errorf("restored file carries user.ark = %q, want %q", got, "value")
	}
}

// A socket is the one file type the format does not model: the backup drops
// and counts it. A fifo beside it is stored and restored.
func TestSocketsAreSkippedAndCounted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("fifos unsupported here: %v", err)
	}
	l, err := net.Listen("unix", filepath.Join(root, "sock"))
	if err != nil {
		t.Skipf("unix sockets unsupported here: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	repo, snap, dst := roundTrip(t, root)

	if snap.Stats.Skipped != 1 {
		t.Errorf("stats count %d skipped entries, want 1", snap.Stats.Skipped)
	}
	if snap.Stats.FileCount != 1 {
		t.Errorf("stats count %d files, want 1, the fifo is not one", snap.Stats.FileCount)
	}
	entries := entriesOf(t, repo, snap.ID)
	if _, ok := entries["sock"]; ok {
		t.Error("the socket made it into the snapshot tree")
	}
	if e := entries["pipe"]; e.Type != ark.FileTypeFifo {
		t.Errorf("the fifo is in the snapshot as %q, want %q", e.Type, ark.FileTypeFifo)
	}
	if _, err := os.Lstat(filepath.Join(dst, "sock")); !os.IsNotExist(err) {
		t.Errorf("restore recreated the socket: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "pipe")); err != nil || fi.Mode().Type() != os.ModeNamedPipe {
		t.Errorf("restore did not recreate the fifo: %v", err)
	}
}

func TestExportKeepsHardlinksAndXattrs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("shared"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "z-link.txt")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	xattrOK := unix.Setxattr(filepath.Join(root, "a.txt"), "user.ark", []byte("value"), 0) == nil

	repo, snap, _ := roundTrip(t, root)

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}
	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	entries := entriesOf(t, target, imported.ID)
	link, ok := entries["z-link.txt"]
	if !ok {
		t.Fatalf("imported snapshot has no z-link.txt, got %v", entries)
	}
	if link.Type != ark.FileTypeHardlink {
		t.Errorf("z-link.txt imported as %q, want %q", link.Type, ark.FileTypeHardlink)
	}
	if link.LinkDest != "a.txt" {
		t.Errorf("z-link.txt points at %q, want %q", link.LinkDest, "a.txt")
	}
	if xattrOK {
		if got := string(entries["a.txt"].Xattrs["user.ark"]); got != "value" {
			t.Errorf("a.txt imported with user.ark = %q, want %q", got, "value")
		}
	}
}

// specialModes fills root with a setuid file, a setgid directory, a sticky
// directory and a symlink, and returns the mode of each of the first three.
func specialModes(t *testing.T, root string) map[string]os.FileMode {
	t.Helper()
	want := map[string]os.FileMode{
		"suid":   os.ModeSetuid | 0o755,
		"sgid":   os.ModeSetgid | 0o775,
		"sticky": os.ModeSticky | 0o777,
	}
	if err := os.WriteFile(filepath.Join(root, "suid"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, dir := range []string{"sgid", "sticky"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.Symlink("suid", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for name, mode := range want {
		path := filepath.Join(root, name)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
		if got := statMode(t, path); got != mode {
			t.Skipf("this filesystem keeps %v of %v on %s", got, mode, name)
		}
	}
	return want
}

// statMode is the mode of path without its type bits.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	return fi.Mode() &^ os.ModeType
}

func TestSetuidSetgidAndStickySurviveTheRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := specialModes(t, root)

	repo, snap, restored := roundTrip(t, root)

	// Walk and Export name a directory with a trailing slash.
	entries := make(map[string]ark.FileInfo)
	for path, info := range entriesOf(t, repo, snap.ID) {
		entries[strings.TrimSuffix(path, "/")] = info
	}
	for name, mode := range want {
		if got := entries[name].Mode; got != mode {
			t.Errorf("the snapshot holds %s with mode %v, want %v", name, got, mode)
		}
		if got := statMode(t, filepath.Join(restored, name)); got != mode {
			t.Errorf("%s restored with mode %v, want %v", name, got, mode)
		}
	}
}

// tarModes reads an uncompressed tar and returns the mode of every entry
// under data/, a directory without its trailing slash.
func tarModes(t *testing.T, archive []byte) map[string]int64 {
	t.Helper()
	modes := make(map[string]int64)
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return modes
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		if strings.HasPrefix(h.Name, "data/") {
			modes[strings.TrimSuffix(h.Name, "/")] = h.Mode
		}
	}
}

// TestExportOfAnImportedSnapshotWritesTheSameTarModes: a tar header holds
// mode_t & 07777 like the tree node, so an archive that went through Import
// comes out of Export with the modes it went in with.
func TestExportOfAnImportedSnapshotWritesTheSameTarModes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	specialModes(t, root)
	if err := os.WriteFile(filepath.Join(root, "sgid", "plain.txt"), []byte("y"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	repo, snap, _ := roundTrip(t, root)
	var original bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &original); err != nil {
		t.Fatalf("Export: %v", err)
	}
	want := tarModes(t, original.Bytes())
	// A symlink's mode is 0777 on Linux and follows the umask on darwin.
	linkMode := int64(statMode(t, filepath.Join(root, "link")))
	for name, mode := range map[string]int64{
		"data/suid": 0o4755, "data/sgid": 0o2775, "data/sticky": 0o1777, "data/sgid/plain.txt": 0o640, "data/link": linkMode,
	} {
		if want[name] != mode {
			t.Errorf("the export holds %s with tar mode %#o, want %#o", name, want[name], mode)
		}
	}

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(original.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	var again bytes.Buffer
	if err := target.Export(ctx, imported.ID, &again); err != nil {
		t.Fatalf("Export of the imported snapshot: %v", err)
	}
	if got := tarModes(t, again.Bytes()); !maps.Equal(got, want) {
		t.Errorf("the imported snapshot exports with modes %v, want %v", got, want)
	}
}

// TestHardlinkTargetIsStoredAsAPathFromTheRoot: the node holds a hardlink's
// target as names joined by "/", whatever spelling the source gave.
func TestHardlinkTargetIsStoredAsAPathFromTheRoot(t *testing.T) {
	repo := openRepo(t, arkmem.New())
	var opened int
	snap, err := repo.Backup(context.Background(), entrySource{opened: &opened, entries: []ark.FileInfo{
		{Path: "dir", Mode: 0o755, Type: ark.FileTypeDirectory},
		{Path: "dir/file", Size: 2, Mode: 0o644, Type: ark.FileTypeRegular},
		{Path: "dot", Mode: 0o644, Type: ark.FileTypeHardlink, LinkDest: "./dir/file"},
		{Path: "slash", Mode: 0o644, Type: ark.FileTypeHardlink, LinkDest: "dir/file/"},
	}})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	entries := entriesOf(t, repo, snap.ID)
	for _, name := range []string{"dot", "slash"} {
		if got := entries[name]; got.LinkDest != "dir/file" || got.Size != 2 {
			t.Errorf("%s: target %q, size %d, want %q, 2", name, got.LinkDest, got.Size, "dir/file")
		}
	}
}
