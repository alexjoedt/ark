package arkmem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/alexjoedt/ark"
)

func TestRoundTripPerNamespace(t *testing.T) {
	ctx := context.Background()
	b := New()

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
	}

	// Each type keeps its own namespace, so the same name in two types is two
	// objects.
	for _, h := range handles {
		var names []string
		if err := b.List(ctx, h.Type, func(info ark.BlobInfo) error {
			names = append(names, info.Name)
			return nil
		}); err != nil {
			t.Fatalf("List %v: %v", h.Type, err)
		}
		if len(names) != 1 || names[0] != h.Name {
			t.Errorf("List %v = %v, want [%s]", h.Type, names, h.Name)
		}
	}
}

func TestSameNameDifferentTypes(t *testing.T) {
	ctx := context.Background()
	b := New()

	cfg := ark.Handle{Type: ark.ConfigFile, Name: "same"}
	tree := ark.Handle{Type: ark.TreeFile, Name: "same"}
	if err := b.Put(ctx, cfg, strings.NewReader("config")); err != nil {
		t.Fatalf("Put config: %v", err)
	}
	if err := b.Put(ctx, tree, strings.NewReader("tree")); err != nil {
		t.Fatalf("Put tree: %v", err)
	}

	for h, want := range map[ark.Handle]string{cfg: "config", tree: "tree"} {
		rc, err := b.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get %v: %v", h, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != want {
			t.Errorf("Get %v = %q, want %q", h, got, want)
		}
	}
}

func TestPutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.SnapshotFile, Name: "01hxyz"}

	for _, payload := range []string{"first", "second and longer"} {
		if err := b.Put(ctx, h, strings.NewReader(payload)); err != nil {
			t.Fatalf("Put %q: %v", payload, err)
		}
		rc, err := b.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != payload {
			t.Errorf("Get = %q, want %q", got, payload)
		}
	}

	count := 0
	if err := b.List(ctx, ark.SnapshotFile, func(ark.BlobInfo) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != 1 {
		t.Errorf("List returned %d objects, want 1", count)
	}
}

func TestPutFailureKeepsPrevious(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.ConfigFile, Name: "config"}

	if err := b.Put(ctx, h, strings.NewReader("good")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := errors.New("boom")
	if err := b.Put(ctx, h, iotest.ErrReader(want)); !errors.Is(err, want) {
		t.Fatalf("Put with failing reader = %v, want %v", err, want)
	}

	rc, err := b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "good" {
		t.Errorf("Get = %q, want %q", got, "good")
	}
}

func TestGetReturnsACopy(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.BlobFile, Name: "abcdef0123456789"}
	if err := b.Put(ctx, h, strings.NewReader("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	copy(got, "XXXXXXX")

	rc, err = b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	again, _ := io.ReadAll(rc)
	rc.Close()
	if string(again) != "payload" {
		t.Errorf("stored object mutated through the reader: %q", again)
	}
}

func TestMissingObject(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.TreeFile, Name: "absent"}

	if _, err := b.Get(ctx, h); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, ark.ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
	if ok, err := b.Exists(ctx, h); ok || err != nil {
		t.Errorf("Exists = %v, %v; want false, nil", ok, err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Errorf("Delete of a missing object = %v, want nil", err)
	}
	if err := b.List(ctx, ark.TreeFile, func(ark.BlobInfo) error {
		t.Error("List called fn on an empty namespace")
		return nil
	}); err != nil {
		t.Errorf("List on an empty namespace = %v, want nil", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.LockFile, Name: "lock-1"}
	if err := b.Put(ctx, h, strings.NewReader("held")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, err := b.Exists(ctx, h)
	if ok || err != nil {
		t.Errorf("Exists after Delete = %v, %v; want false, nil", ok, err)
	}
}

func TestRejectsUnknownTypeAndEmptyName(t *testing.T) {
	ctx := context.Background()
	b := New()

	unknown := ark.Handle{Type: ark.HandleType(200), Name: "x"}
	if err := b.Put(ctx, unknown, strings.NewReader("x")); err == nil {
		t.Error("Put with an unknown handle type = nil, want an error")
	}
	if err := b.List(ctx, ark.HandleType(200), func(ark.BlobInfo) error { return nil }); err == nil {
		t.Error("List with an unknown handle type = nil, want an error")
	}
	if err := b.Put(ctx, ark.Handle{Type: ark.ConfigFile}, strings.NewReader("x")); err == nil {
		t.Error("Put with an empty name = nil, want an error")
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := New()
	h := ark.Handle{Type: ark.ConfigFile, Name: "config"}

	if err := b.Put(ctx, h, strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
	if _, err := b.Get(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if _, err := b.Exists(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Exists = %v, want context.Canceled", err)
	}
	if err := b.Delete(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v, want context.Canceled", err)
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Stat = %v, want context.Canceled", err)
	}
	if err := b.List(ctx, ark.ConfigFile, func(ark.BlobInfo) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v, want context.Canceled", err)
	}
}

// TestConcurrentAccess is the race detector's target: run with -race.
func TestConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	b := New()
	const workers = 16

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			h := ark.Handle{Type: ark.BlobFile, Name: fmt.Sprintf("chunk-%02d", i)}
			payload := strings.Repeat("x", i+1)
			if err := b.Put(ctx, h, strings.NewReader(payload)); err != nil {
				t.Errorf("Put %v: %v", h, err)
				return
			}
			if _, err := b.Get(ctx, h); err != nil {
				t.Errorf("Get %v: %v", h, err)
			}
			if _, err := b.Exists(ctx, h); err != nil {
				t.Errorf("Exists %v: %v", h, err)
			}
			if err := b.List(ctx, ark.BlobFile, func(ark.BlobInfo) error { return nil }); err != nil {
				t.Errorf("List: %v", err)
			}
		})
	}
	wg.Wait()

	count := 0
	if err := b.List(ctx, ark.BlobFile, func(ark.BlobInfo) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != workers {
		t.Errorf("List returned %d objects, want %d", count, workers)
	}
}

// TestConcurrentPutSameHandle checks that racing writers leave one whole
// object behind, never a mix of both payloads.
func TestConcurrentPutSameHandle(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.ConfigFile, Name: "config"}
	a := strings.Repeat("a", 4096)
	z := strings.Repeat("z", 4096)

	var wg sync.WaitGroup
	for range 8 {
		for _, payload := range []string{a, z} {
			wg.Go(func() {
				if err := b.Put(ctx, h, strings.NewReader(payload)); err != nil {
					t.Errorf("Put: %v", err)
				}
			})
		}
	}
	wg.Wait()

	rc, err := b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != a && string(got) != z {
		t.Errorf("Get returned a torn object of len %d", len(got))
	}
}

func TestCloseEmptiesTheBackend(t *testing.T) {
	ctx := context.Background()
	b := New()
	h := ark.Handle{Type: ark.ConfigFile, Name: "config"}
	if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ok, err := b.Exists(ctx, h)
	if ok || err != nil {
		t.Errorf("Exists after Close = %v, %v; want false, nil", ok, err)
	}
}
