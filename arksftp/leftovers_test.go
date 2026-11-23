package arksftp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexjoedt/ark"
)

// TestRemoveTempKeepsAsideFiles plants what killed Puts and failed renames
// leave in namespaces and shards, a shard behind a link included. RemoveTemp
// deletes only the temporary files, ListAside names only the aside files.
func TestRemoveTempKeepsAsideFiles(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	root := b.basePath
	putAll(t, b,
		ark.Handle{Type: ark.DataPackFile, Name: "aa01"},
		ark.Handle{Type: ark.LockFile, Name: "l1"},
	)
	if err := os.MkdirAll(filepath.Join(root, "treepacks", "bb", ".ark-tmp-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(root, "datapacks", "cc")); err != nil {
		t.Fatal(err)
	}
	temps := []string{
		"config/.ark-tmp-1",
		"locks/.ark-tmp-2",
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
	for _, f := range append(slices.Clone(temps), asides...) {
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
	if _, err := os.Stat(filepath.Join(root, "treepacks", "bb", ".ark-tmp-dir")); err != nil {
		t.Errorf("a dir with the temp prefix: %v, want kept", err)
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
