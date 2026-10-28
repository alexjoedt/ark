package arklocal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	// A chunk name long enough to shard, and one object per remaining type.
	handles := []ark.Handle{
		{Type: ark.ConfigFile, Name: "config"},
		{Type: ark.BlobFile, Name: "abcdef0123456789"},
		{Type: ark.TreeFile, Name: "0011223344556677"},
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

	// Chunks are sharded two levels deep.
	shard := filepath.Join(b.BasePath(), "chunks", "ab", "cd", "abcdef0123456789")
	if _, err := os.Stat(shard); err != nil {
		t.Errorf("expected sharded chunk at %s: %v", shard, err)
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
