package arksftp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
)

// TestRemoveTempKeepsAsideFiles plants what killed Puts, cut-off mkdirs and
// failed renames leave in the root, namespaces and shards, a shard behind a
// link included. RemoveTemp deletes the temporary files and dirs but keeps
// one in locks, ListAside names only the aside files.
func TestRemoveTempKeepsAsideFiles(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	root := b.basePath
	putAll(t, b,
		ark.Handle{Type: ark.DataPackFile, Name: "aa01"},
		ark.Handle{Type: ark.LockFile, Name: "l1"},
	)
	for _, d := range []string{".ark-tmp-root", "datapacks/.ark-tmp-ns", "treepacks/bb/.ark-tmp-dir"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(root, "datapacks", "cc")); err != nil {
		t.Fatal(err)
	}
	temps := []string{
		"config/.ark-tmp-1",
		"datapacks/.ark-tmp-3",
		"datapacks/aa/.ark-tmp-4",
		"datapacks/cc/.ark-tmp-5",
		"treepacks/bb/.ark-tmp-6",
	}
	asides := []string{
		"datapacks/aa/.ark-old-aa02",
		"datapacks/cc/.ark-old-cc01",
		"keys/.ark-old-k1",
	}
	lockTemp := "locks/.ark-tmp-2"
	for _, f := range append(slices.Clone(temps), append(asides, lockTemp)...) {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := b.RemoveTemp(ctx); err != nil {
		t.Fatalf("RemoveTemp: %v", err)
	}
	for _, f := range temps {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(f))); !os.IsNotExist(err) {
			t.Errorf("%s after RemoveTemp: %v, want gone", f, err)
		}
	}
	for _, d := range []string{".ark-tmp-root", "datapacks/.ark-tmp-ns", "treepacks/bb/.ark-tmp-dir"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); !os.IsNotExist(err) {
			t.Errorf("temporary dir %s after RemoveTemp: %v, want gone", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(lockTemp))); err != nil {
		t.Errorf("%s after RemoveTemp: %v, want kept, a lock write may be in flight", lockTemp, err)
	}
	var got []string
	if err := b.ListAside(ctx, func(p string) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("ListAside: %v", err)
	}
	slices.Sort(got)
	if !slices.Equal(got, asides) {
		t.Errorf("ListAside = %v, want %v", got, asides)
	}
	if got, want := listNames(t, b, ark.DataPackFile), []string{"aa01"}; !slices.Equal(got, want) {
		t.Errorf("List datapacks = %v, want %v", got, want)
	}
	if got, want := listNames(t, b, ark.LockFile), []string{"l1"}; !slices.Equal(got, want) {
		t.Errorf("List locks = %v, want %v", got, want)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.RemoveTemp(cancelled); err == nil {
		t.Error("RemoveTemp on a cancelled context: want error")
	}
}

// TestPruneRemovesTempFiles runs a repository prune over sftp: the temporary
// files killed Puts left are gone, the aside file stays.
func TestPruneRemovesTempFiles(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	pw := ark.NewPassword("ark-test")
	if err := ark.Init(ctx, b, pw); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, pw)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// No repo.Close: it closes b, and pipeBackend closes its client itself.
	temps := []string{
		filepath.Join(b.basePath, "snapshots", ".ark-tmp-1"),
		filepath.Join(b.basePath, "datapacks", "aa", ".ark-tmp-2"),
	}
	aside := filepath.Join(b.basePath, "keys", ".ark-old-k1")
	for _, p := range append(slices.Clone(temps), aside) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, p := range temps {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s after prune: %v, want gone", p, err)
		}
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("aside file after prune: %v, want kept", err)
	}
}

// TestRemoveTempKeepsWhatItMayNotRemove runs RemoveTemp over a temporary file
// in a dir it may not write: the file stays and RemoveTemp succeeds, so a
// prune does not fail on another user's leftover.
func TestRemoveTempKeepsWhatItMayNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes any file")
	}
	b := pipeBackend(t)
	shard := filepath.Join(b.basePath, "datapacks", "aa")
	tmp := filepath.Join(shard, ".ark-tmp-1")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shard, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shard, 0o700) })
	if err := b.RemoveTemp(context.Background()); err != nil {
		t.Fatalf("RemoveTemp: %v, want nil", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("temporary file after RemoveTemp: %v, want kept", err)
	}
}

// TestRemoveTempFailsOnADanglingNamespace runs RemoveTemp with a namespace
// behind a link whose target is gone, as List does: the disk it leads to may
// not be mounted.
func TestRemoveTempFailsOnADanglingNamespace(t *testing.T) {
	b := pipeBackend(t)
	if err := os.MkdirAll(b.basePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(b.basePath, "snapshots")); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveTemp(context.Background()); err == nil {
		t.Error("RemoveTemp: want error")
	}
}
