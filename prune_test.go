package ark_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arkmem"
)

// TestPruneSweepsBothNamespaces guards the split of trees out of the chunk
// namespace: prune has to match each namespace against its own live set, or it
// deletes the tree nodes of the snapshots it is meant to keep.
func TestPruneSweepsBothNamespaces(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	// arkmem leaves no files behind, so Prune has nothing to remove first.
	if _, ok := ark.Backend(b).(ark.Leftovers); ok {
		t.Fatal("arkmem implements ark.Leftovers")
	}

	root := t.TempDir()
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("first revision")

	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	first, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	write("second revision")
	second, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{first.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	result, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// One orphaned chunk and one orphaned tree node.
	if result.BlobsRemoved != 2 {
		t.Errorf("prune removed %d objects, want 2", result.BlobsRemoved)
	}

	vr, err := repo.Verify(ctx, second.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK {
		t.Errorf("kept snapshot broke: missing %v, corrupt %v", vr.MissingChunks, vr.CorruptChunks)
	}

	dest := t.TempDir()
	sink, err := arkfs.NewSink(dest)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(ctx, second.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "data.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "second revision" {
		t.Errorf("restored %q, want %q", got, "second revision")
	}
}

// TestPruneRemovesTempFiles plants the temporary files killed Puts leave and
// an aside file. A dry run keeps them all, a prune removes the temporary ones
// and keeps the aside file, also through an instrumented backend.
func TestPruneRemovesTempFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(ark.Backend) ark.Backend
	}{
		{"arklocal", func(b ark.Backend) ark.Backend { return b }},
		{"instrumented", func(b ark.Backend) ark.Backend { return ark.Instrument(b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			local, err := arklocal.New(dir)
			if err != nil {
				t.Fatalf("arklocal.New: %v", err)
			}
			repo := openRepo(t, tc.wrap(local))
			src := t.TempDir()
			writeTestFile(t, filepath.Join(src, "a.txt"), []byte("kept"))
			backupDir(t, repo, src)

			shards, err := filepath.Glob(filepath.Join(dir, "datapacks", "??"))
			if err != nil || len(shards) == 0 {
				t.Fatalf("no data pack shard: %v", err)
			}
			temps := []string{
				filepath.Join(dir, "snapshots", ".ark-tmp-1"),
				filepath.Join(shards[0], ".ark-tmp-2"),
			}
			aside := filepath.Join(dir, "keys", ".ark-old-k1")
			for _, p := range append(slices.Clone(temps), aside) {
				writeTestFile(t, p, []byte("junk"))
			}

			if _, err := repo.Prune(ctx, ark.WithPruneDryRun()); err != nil {
				t.Fatalf("Prune dry run: %v", err)
			}
			for _, p := range temps {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("%s after a dry run: %v, want kept", p, err)
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
			res, err := repo.Check(ctx)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			want := "mv keys/.ark-old-k1 keys/k1"
			if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.HasSuffix(w, want) }) {
				t.Errorf("Check warnings %q, want one ending in %q", res.Warnings, want)
			}
		})
	}
}
