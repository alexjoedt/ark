package arklocal

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
)

// putAll stores every handle with its name as content.
func putAll(t *testing.T, b *Backend, hs ...ark.Handle) {
	t.Helper()
	for _, h := range hs {
		if err := b.Put(context.Background(), h, strings.NewReader(h.Name)); err != nil {
			t.Fatalf("Put %s: %v", h.Name, err)
		}
	}
}

func listNames(t *testing.T, b *Backend, typ ark.HandleType) []string {
	t.Helper()
	var names []string
	err := b.List(context.Background(), typ, func(i ark.BlobInfo) error {
		if i.Size != int64(len(i.Name)) {
			t.Errorf("List size of %s = %d, want %d", i.Name, i.Size, len(i.Name))
		}
		names = append(names, i.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("List %v: %v", typ, err)
	}
	slices.Sort(names)
	return names
}

// moveBehindLink moves the dir or file at p to another dir and leaves a
// symlink to it at p, as a repository spread over two disks has.
func moveBehindLink(t *testing.T, p string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), filepath.Base(p))
	if err := os.Rename(p, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// TestListFollowsSymlinks puts a namespace, a shard and an object behind a
// symlink. List must return every object; listing the link itself made every
// pack look lost.
func TestListFollowsSymlinks(t *testing.T) {
	packs := []ark.Handle{
		{Type: ark.DataPackFile, Name: "aa01"},
		{Type: ark.DataPackFile, Name: "aa02"},
		{Type: ark.DataPackFile, Name: "bb01"},
	}
	locks := []ark.Handle{{Type: ark.LockFile, Name: "l1"}, {Type: ark.LockFile, Name: "l2"}}
	for _, tc := range []struct {
		name string
		link string // relative to the root
		hs   []ark.Handle
	}{
		{"pack namespace", "datapacks", packs},
		{"shard", "datapacks/aa", packs},
		{"pack", "datapacks/bb/bb01", packs},
		{"flat namespace", "locks", locks},
		{"object", "locks/l2", locks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend(t)
			putAll(t, b, tc.hs...)
			moveBehindLink(t, filepath.Join(b.BasePath(), tc.link))
			var want []string
			for _, h := range tc.hs {
				want = append(want, h.Name)
			}
			if got := listNames(t, b, tc.hs[0].Type); !slices.Equal(got, want) {
				t.Errorf("List = %v, want %v", got, want)
			}
		})
	}
}

// TestListSkipsEntriesOutsideTheLayout plants what a NAS, macOS or fsck leave
// in a repository. List yields only the stored objects.
func TestListSkipsEntriesOutsideTheLayout(t *testing.T) {
	b := newBackend(t)
	putAll(t, b,
		ark.Handle{Type: ark.DataPackFile, Name: "aa01"},
		ark.Handle{Type: ark.DataPackFile, Name: "b"}, // shorter than a shard, stored flat
		ark.Handle{Type: ark.LockFile, Name: "l1"},
	)
	root := b.BasePath()
	for _, d := range []string{
		"datapacks/@eaDir/aa02",
		"datapacks/aa/@eaDir/aa03",
		"datapacks/lost+found/aa04",
		"datapacks/aa/aa05",
		"locks/@eaDir/l2",
		"locks/sub/l3",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"datapacks/.DS_Store",
		"datapacks/aa/.DS_Store",
		"datapacks/aa/.ark-tmp-1",
		"datapacks/cc01",    // a file at shard level
		"datapacks/aa/bb01", // in the wrong shard
		"locks/.DS_Store",
	} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "locks"), filepath.Join(root, "datapacks", "dd")); err != nil {
		t.Fatal(err)
	}
	if got, want := listNames(t, b, ark.DataPackFile), []string{"aa01", "b"}; !slices.Equal(got, want) {
		t.Errorf("List datapacks = %v, want %v", got, want)
	}
	if got, want := listNames(t, b, ark.LockFile), []string{"l1"}; !slices.Equal(got, want) {
		t.Errorf("List locks = %v, want %v", got, want)
	}
}

// TestListFailsWhenAnEntryCannotBeRead covers dirs List cannot read and
// entries it cannot stat. A listing that passed over them would lack objects
// that are stored, and a caller would count them as lost.
func TestListFailsWhenAnEntryCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  bool // the case needs permissions to bite, which root has not
		setup func(t *testing.T, root string)
	}{
		{"unreadable namespace", true, func(t *testing.T, root string) {
			chmod(t, filepath.Join(root, "datapacks"), 0o300)
		}},
		{"unreadable shard", true, func(t *testing.T, root string) {
			chmod(t, filepath.Join(root, "datapacks", "aa"), 0o300)
		}},
		{"symlink loop", false, func(t *testing.T, root string) {
			p := filepath.Join(root, "datapacks", "bb")
			if err := os.Symlink(p, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling shard link", false, func(t *testing.T, root string) {
			p := filepath.Join(root, "datapacks", "aa")
			if err := os.Rename(p, filepath.Join(root, "moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), p); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling namespace link", false, func(t *testing.T, root string) {
			p := filepath.Join(root, "datapacks")
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "unmounted"), p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.root && os.Geteuid() == 0 {
				t.Skip("root reads any dir")
			}
			b := newBackend(t)
			putAll(t, b, ark.Handle{Type: ark.DataPackFile, Name: "aa01"})
			tc.setup(t, b.BasePath())
			err := b.List(context.Background(), ark.DataPackFile, func(ark.BlobInfo) error { return nil })
			if err == nil {
				t.Error("List = nil, want an error")
			}
		})
	}
}

func chmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
}

// TestListSkipsAnObjectDeletedWhileListing removes an object after List read
// its dir and before it got to it: a deleted object is no error.
func TestListSkipsAnObjectDeletedWhileListing(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	putAll(t, b, ark.Handle{Type: ark.LockFile, Name: "l1"}, ark.Handle{Type: ark.LockFile, Name: "l2"})
	var names []string
	err := b.List(ctx, ark.LockFile, func(i ark.BlobInfo) error {
		names = append(names, i.Name)
		return b.Delete(ctx, ark.Handle{Type: ark.LockFile, Name: "l2"})
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(names, []string{"l1"}) {
		t.Errorf("List = %v, want [l1]", names)
	}
}
