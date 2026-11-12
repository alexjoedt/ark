package arklocal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/alexjoedt/ark"
)

func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestRoundTripPerNamespace(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)

	// Pack names long enough to shard, and one object per remaining type.
	handles := []ark.Handle{
		{Type: ark.ConfigFile, Name: "config"},
		{Type: ark.DataPackFile, Name: "abcdef0123456789"},
		{Type: ark.TreePackFile, Name: "0011223344556677"},
		{Type: ark.DataIndexFile, Name: "abcdef0123456789"},
		{Type: ark.SnapshotFile, Name: "01hxyz"},
		{Type: ark.LockFile, Name: "lock-1"},
	}

	for _, h := range handles {
		want := []byte("payload of " + h.Name)
		if err := b.Put(ctx, h, bytes.NewReader(want)); err != nil {
			t.Fatalf("Put %v: %v", h, err)
		}

		rc, err := b.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get %v: %v", h, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %v: %v", h, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get %v = %q, want %q", h, got, want)
		}

		ok, err := b.Exists(ctx, h)
		if err != nil || !ok {
			t.Errorf("Exists %v = %v, %v; want true, nil", h, ok, err)
		}

		info, err := b.Stat(ctx, h)
		if err != nil {
			t.Fatalf("Stat %v: %v", h, err)
		}
		if info.Name != h.Name || info.Size != int64(len(want)) {
			t.Errorf("Stat %v = %+v, want name %q size %d", h, info, h.Name, len(want))
		}

		// Every type lands in the directory named by its namespace.
		ns := filepath.Join(b.BasePath(), h.Type.String())
		if _, err := os.Stat(ns); err != nil {
			t.Errorf("namespace dir %s: %v", ns, err)
		}
	}

	// Packs are sharded one level deep, nothing else is.
	for _, p := range []string{
		"datapacks/ab/abcdef0123456789", "treepacks/00/0011223344556677", "dataindex/abcdef0123456789",
	} {
		if _, err := os.Stat(filepath.Join(b.BasePath(), filepath.FromSlash(p))); err != nil {
			t.Errorf("expected object at %s: %v", p, err)
		}
	}
}

func TestPutIsIdempotentAndAtomic(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	h := ark.Handle{Type: ark.SnapshotFile, Name: "s1"}

	if err := b.Put(ctx, h, strings.NewReader("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Put(ctx, h, strings.NewReader("second")); err != nil {
		t.Fatalf("Put again: %v", err)
	}

	rc, err := b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second" {
		t.Errorf("Get = %q, want %q", got, "second")
	}

	// A failed Put leaves no temp file behind.
	failing := ark.Handle{Type: ark.SnapshotFile, Name: "s2"}
	if err := b.Put(ctx, failing, iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Fatal("Put with a failing reader: want error, got nil")
	}
	var names []string
	if err := b.List(ctx, ark.SnapshotFile, func(i ark.BlobInfo) error {
		names = append(names, i.Name)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "s1" {
		t.Errorf("List after failed Put = %v, want [s1]", names)
	}
	entries, _ := os.ReadDir(filepath.Join(b.BasePath(), "snapshots"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestListReportsSizesAndDelete(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)

	want := map[string]int64{"aabbccdd": 3, "eeff0011": 5}
	for name, size := range want {
		h := ark.Handle{Type: ark.BlobFile, Name: name}
		if err := b.Put(ctx, h, bytes.NewReader(make([]byte, size))); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	got := map[string]int64{}
	if err := b.List(ctx, ark.BlobFile, func(i ark.BlobInfo) error {
		got[i.Name] = i.Size
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for name, size := range want {
		if got[name] != size {
			t.Errorf("List size of %s = %d, want %d", name, got[name], size)
		}
	}

	// Listing an empty namespace is not an error.
	if err := b.List(ctx, ark.LockFile, func(ark.BlobInfo) error {
		t.Error("unexpected object in empty namespace")
		return nil
	}); err != nil {
		t.Errorf("List on empty namespace: %v", err)
	}

	h := ark.Handle{Type: ark.BlobFile, Name: "aabbccdd"}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Errorf("Delete of a missing object: %v", err)
	}
	if _, err := b.Get(ctx, h); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

// TestListFailsWhenAShardVanishesMidWalk moves a pack shard away while List
// walks the one before it. The listing then lacks packs that are stored, and
// a caller that took it as complete would count them as lost: List fails.
func TestListFailsWhenAShardVanishesMidWalk(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	for _, name := range []string{"aa01", "bb01"} {
		if err := b.Put(ctx, ark.Handle{Type: ark.DataPackFile, Name: name}, strings.NewReader(name)); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	ns := filepath.Join(b.BasePath(), ark.DataPackFile.String())
	err := b.List(ctx, ark.DataPackFile, func(ark.BlobInfo) error {
		return os.Rename(filepath.Join(ns, "bb"), filepath.Join(b.BasePath(), "moved"))
	})
	if err == nil {
		t.Fatal("List after a shard vanished mid-walk = nil, want an error")
	}
}

func TestRejectsUnknownTypeAndEscapingNames(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)

	if err := b.Put(ctx, ark.Handle{Type: ark.HandleType(200), Name: "x"}, strings.NewReader("x")); err == nil {
		t.Error("Put with an unknown handle type: want error, got nil")
	}
	for _, name := range []string{"", ".", "..", "../escape", "sub/dir", tmpPrefix + "x"} {
		h := ark.Handle{Type: ark.SnapshotFile, Name: name}
		if err := b.Put(ctx, h, strings.NewReader("x")); err == nil {
			t.Errorf("Put with name %q: want error, got nil", name)
		}
	}
}

// TestPutAndDeleteNeverSync pins the durability rule: Put and Delete flush
// nothing, Sync flushes the filesystem of the root once.
func TestPutAndDeleteNeverSync(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	calls := 0
	orig := syncFS
	t.Cleanup(func() { syncFS = orig })
	syncFS = func(root *os.File) error {
		if root.Name() != b.BasePath() {
			t.Errorf("syncFS on %s, want the root %s", root.Name(), b.BasePath())
		}
		calls++
		return nil
	}
	h := ark.Handle{Type: ark.BlobFile, Name: "abcdef0123456789"}

	if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Put(ctx, h, strings.NewReader("y")); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 0 {
		t.Fatalf("Put and Delete flushed %d times, want 0", calls)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if calls != 1 {
		t.Errorf("Sync flushed %d times, want 1", calls)
	}

	syncFS = func(*os.File) error { return syscall.EIO }
	if err := b.Sync(ctx); !errors.Is(err, syscall.EIO) {
		t.Errorf("Sync with a failing flush = %v, want EIO", err)
	}
}
