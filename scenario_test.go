package ark_test

// The scenario list gates a release. Every entry runs against
// arkmem, in this package, on the standard library alone, on a password
// protected repository. Where an entry
// already had a home the list points at it instead of repeating it:
//
//	round trip per entry type            TestScenarioRoundTripCoversEveryEntryType
//	second backup reuses every chunk     TestScenarioSecondBackupReusesEveryChunk
//	each forget policy field             TestForgetPolicyFieldsSelectTheirPeriod (forget_internal_test.go)
//	KeepWithin, IDs, ErrEmptyPolicy      TestKeepWithinKeepsRecentSnapshots, TestForgetByIDsRemovesExactlyThose,
//	                                     TestForgetRefusesAZeroPolicyWithoutIDs (api_test.go)
//	prune removes orphans, keeps live    TestPruneSweepsBothNamespaces (prune_test.go)
//	prune refused under a shared lock    TestScenarioPruneIsRefusedWhileASharedLockExists
//	verify finds corrupt and missing     TestScenarioVerifyFindsACorruptChunk, TestScenarioVerifyFindsAMissingChunk
//	check over several snapshots         TestScenarioCheckCoversEverySnapshot
//	export and import round trip         TestExportWritesPlainTarWithMetadataFirst, TestImportRechunksIntoANewSnapshot (export_test.go)
//	id prefixes and ErrAmbiguousID       TestUniquePrefixResolvesEverywhere, TestAmbiguousPrefixIsRejected (api_test.go)
//	WithRestorePaths                     TestRestorePathsPruneTheWalk (restore_test.go)
//	cancelled backup leaves no manifest  TestScenarioCancelledBackupLeavesNoManifest
//	stale lock rules with a fake clock   TestLockIsStaleAfterThirtyMinutes and friends (lock_internal_test.go)
//	Init twice fails                     TestInitTwiceIsRefused (config_test.go)
//	Open on an empty backend fails       TestOpenRequiresAConfig (config_test.go)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
	"golang.org/x/sys/unix"
)

// testPassword protects the repositories of the scenario list and the helpers
// it points at. TestMain lowers argon2id, so it costs nothing.
var testPassword = ark.WithPassword("ark-test")

// scenarioMaxChunk is the maximum chunk size the scenario repositories use.
// It is small so "larger than the max chunk" costs kilobytes, not megabytes.
const scenarioMaxChunk = 16 << 10

// scenarioRepo opens a repository whose chunker splits at scenarioMaxChunk.
func scenarioRepo(t *testing.T, b ark.Backend) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: scenarioMaxChunk}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// backupDir backs up root through arkfs.
func backupDir(t *testing.T, repo *ark.Repository, root string) *ark.Snapshot {
	t.Helper()
	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	snap, err := repo.Backup(context.Background(), src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return snap
}

// restoreInto restores a snapshot into a fresh directory and returns it.
func restoreInto(t *testing.T, repo *ark.Repository, id string) string {
	t.Helper()
	dst := t.TempDir()
	sink, err := arkfs.NewSink(dst)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if _, err := repo.Restore(context.Background(), id, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return dst
}

// TestScenarioRoundTripCoversEveryEntryType is the first line of the scenario list:
// one backup and one restore over every entry type the v1 format models,
// including the two sizes that have their own code path, empty and larger
// than a chunk.
func TestScenarioRoundTripCoversEveryEntryType(t *testing.T) {
	root := t.TempDir()

	big := payload(3) // 512 KiB, tens of chunks at scenarioMaxChunk
	files := map[string]string{
		"a-text.txt":    "hello",
		"b-empty.txt":   "",
		"c-big.bin":     big,
		"dir/inner.txt": "inner",
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatalf("mkdir empty-dir: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Symlink("a-text.txt", filepath.Join(root, "d-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Sorts after its target, so the walk stores the content first.
	hardlinked := os.Link(filepath.Join(root, "a-text.txt"), filepath.Join(root, "z-hardlink.txt")) == nil
	xattred := unix.Setxattr(filepath.Join(root, "a-text.txt"), "user.ark", []byte("value"), 0) == nil

	repo := scenarioRepo(t, arkmem.New())
	snap := backupDir(t, repo, root)
	dst := restoreInto(t, repo, snap.ID)

	entries := entriesOf(t, repo, snap.ID)
	// Directories reach a caller with a trailing slash, the way tar names
	// them; regular files, symlinks and hardlinks carry the plain path.
	wantType := map[string]ark.FileType{
		"a-text.txt":    ark.FileTypeRegular,
		"b-empty.txt":   ark.FileTypeRegular,
		"c-big.bin":     ark.FileTypeRegular,
		"d-link":        ark.FileTypeSymlink,
		"dir/":          ark.FileTypeDirectory,
		"dir/inner.txt": ark.FileTypeRegular,
		"empty-dir/":    ark.FileTypeDirectory,
	}
	if hardlinked {
		wantType["z-hardlink.txt"] = ark.FileTypeHardlink
	}
	for path, want := range wantType {
		got, ok := entries[path]
		if !ok {
			t.Errorf("snapshot has no entry %s", path)
			continue
		}
		if got.Type != want {
			t.Errorf("%s has type %q, want %q", path, got.Type, want)
		}
	}

	// A file past the maximum chunk size is stored in several chunks.
	if len(big) <= scenarioMaxChunk {
		t.Fatalf("the big file is %d bytes, not more than the %d byte maximum", len(big), scenarioMaxChunk)
	}
	if snap.Stats.ChunksTotal < len(big)/scenarioMaxChunk {
		t.Errorf("the snapshot holds %d chunks, too few for %d bytes at a %d byte maximum",
			snap.Stats.ChunksTotal, len(big), scenarioMaxChunk)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("read restored %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %s holds %d bytes, want %d", name, len(got), len(want))
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "empty-dir")); err != nil || !info.IsDir() {
		t.Errorf("empty-dir did not survive the round trip: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "d-link"))
	if err != nil {
		t.Errorf("readlink: %v", err)
	} else if target != "a-text.txt" {
		t.Errorf("restored symlink points at %q, want %q", target, "a-text.txt")
	}
	if hardlinked && !sameInode(t, filepath.Join(dst, "a-text.txt"), filepath.Join(dst, "z-hardlink.txt")) {
		t.Error("the restored hardlink is a separate inode")
	}
	if xattred {
		var buf [64]byte
		n, err := unix.Getxattr(filepath.Join(dst, "a-text.txt"), "user.ark", buf[:])
		if err != nil {
			t.Errorf("getxattr on the restored file: %v", err)
		} else if got := string(buf[:n]); got != "value" {
			t.Errorf("restored xattr user.ark is %q, want %q", got, "value")
		}
	}
}

// TestScenarioSecondBackupReusesEveryChunk pins the point of content
// addressing: unchanged data costs no new object.
func TestScenarioSecondBackupReusesEveryChunk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), []byte(payload(4)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := arkmem.New()
	repo := scenarioRepo(t, b)

	first := backupDir(t, repo, root)
	if first.Stats.ChunksNew == 0 {
		t.Fatalf("the first backup stored no new chunks")
	}
	before := len(objectNames(t, b, ark.BlobFile))

	second := backupDir(t, repo, root)
	if second.Stats.ChunksNew != 0 {
		t.Errorf("the second backup stored %d new chunks, want 0", second.Stats.ChunksNew)
	}
	if second.Stats.ChunksReused != second.Stats.ChunksTotal {
		t.Errorf("the second backup reused %d of %d chunks, want all",
			second.Stats.ChunksReused, second.Stats.ChunksTotal)
	}
	if second.Stats.ChunksTotal != first.Stats.ChunksTotal {
		t.Errorf("the second backup counts %d chunks, the first counted %d",
			second.Stats.ChunksTotal, first.Stats.ChunksTotal)
	}
	if after := len(objectNames(t, b, ark.BlobFile)); after != before {
		t.Errorf("the chunk namespace grew from %d to %d objects", before, after)
	}
}

// TestScenarioVerifyFindsACorruptChunk breaks a chunk in place and by
// copying another chunk's object over it. The first fails the tag, the second
// is a valid object sealed for another name, which the name in the tag's
// associated data rejects.
func TestScenarioVerifyFindsACorruptChunk(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(obj, other []byte) []byte
	}{
		{"flipped byte", func(obj, _ []byte) []byte {
			obj[len(obj)/2] ^= 0x01
			return obj
		}},
		{"copied from another name", func(_, other []byte) []byte { return other }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b := arkmem.New()
			repo := scenarioRepo(t, b)

			snap, err := repo.Backup(ctx, stubSource{name: "app", content: "the original contents"})
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if _, err := repo.Backup(ctx, stubSource{name: "other", content: "other contents"}); err != nil {
				t.Fatalf("Backup: %v", err)
			}

			corrupted := ark.ObjectName(repo, []byte("the original contents"))
			obj := readObject(t, b, ark.BlobFile, corrupted)
			other := readObject(t, b, ark.BlobFile, ark.ObjectName(repo, []byte("other contents")))
			h := ark.Handle{Type: ark.BlobFile, Name: corrupted}
			if err := b.Put(ctx, h, bytes.NewReader(tt.tamper(obj, other))); err != nil {
				t.Fatalf("overwrite chunk: %v", err)
			}

			result, err := repo.Verify(ctx, snap.ID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if result.OK {
				t.Error("Verify passed over a corrupted chunk")
			}
			if len(result.CorruptChunks) != 1 || result.CorruptChunks[0] != corrupted {
				t.Errorf("Verify reports corrupt %v, want [%s]", result.CorruptChunks, corrupted)
			}
			if len(result.MissingChunks) != 0 {
				t.Errorf("Verify reports missing %v, the chunk is there", result.MissingChunks)
			}
		})
	}
}

// TestScenarioVerifyFindsACorruptTree flips a byte in a tree node. Verify
// cannot walk past it, so it fails, and Check names the snapshot.
func TestScenarioVerifyFindsACorruptTree(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "contents"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	trees := objectNames(t, b, ark.TreeFile)
	if len(trees) != 1 {
		t.Fatalf("the snapshot holds %d trees, want 1", len(trees))
	}
	obj := readObject(t, b, ark.TreeFile, trees[0])
	obj[len(obj)-1] ^= 0x01
	if err := b.Put(ctx, ark.Handle{Type: ark.TreeFile, Name: trees[0]}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("overwrite tree: %v", err)
	}

	_, err = repo.Verify(ctx, snap.ID)
	if !errors.Is(err, ark.ErrCorruptTree) || !strings.Contains(err.Error(), trees[0]) {
		t.Errorf("Verify over a corrupt tree returned %v, want ErrCorruptTree naming tree %s", err, trees[0])
	}
	result, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], snap.ID) {
		t.Errorf("Check reports %v, want one error naming snapshot %s", result.Errors, snap.ID)
	}
}

// TestScenarioTamperedManifestIsRefused: the tag covers the whole manifest,
// so a changed byte surfaces as ErrCorruptManifest everywhere it is read.
func TestScenarioTamperedManifestIsRefused(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "contents"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	obj := readObject(t, b, ark.SnapshotFile, snap.ID)
	obj[len(obj)-1] ^= 0x01
	if err := b.Put(ctx, ark.Handle{Type: ark.SnapshotFile, Name: snap.ID}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("overwrite manifest: %v", err)
	}

	assertCorruptManifest(t, repo, snap.ID)
}

// TestScenarioMovedManifestIsRefused: a manifest copied under another
// snapshot id does not open, its name is bound to the tag.
func TestScenarioMovedManifestIsRefused(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "contents"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	moved := snap.ID[:len(snap.ID)-1] + "x"
	if moved == snap.ID {
		moved = snap.ID[:len(snap.ID)-1] + "y"
	}
	obj := readObject(t, b, ark.SnapshotFile, snap.ID)
	if err := b.Put(ctx, ark.Handle{Type: ark.SnapshotFile, Name: moved}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("copy manifest: %v", err)
	}
	if err := b.Delete(ctx, ark.Handle{Type: ark.SnapshotFile, Name: snap.ID}); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}

	assertCorruptManifest(t, repo, moved)
}

// TestScenarioForgetRemovesACorruptManifest: naming the id without a policy
// is the way out. A policy still refuses, and prune works again afterwards.
func TestScenarioForgetRemovesACorruptManifest(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	healthy, err := repo.Backup(ctx, stubSource{name: "app", content: "healthy"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	broken, err := repo.Backup(ctx, stubSource{name: "app", content: "broken"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	obj := readObject(t, b, ark.SnapshotFile, broken.ID)
	obj[len(obj)-1] ^= 0x01
	if err := b.Put(ctx, ark.Handle{Type: ark.SnapshotFile, Name: broken.ID}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("overwrite manifest: %v", err)
	}

	if _, err := repo.Forget(
		ctx,
		ark.ForgetPolicy{KeepLast: 1},
		ark.SnapshotFilter{},
	); !errors.Is(
		err,
		ark.ErrCorruptManifest,
	) {
		t.Errorf("Forget with a policy returned %v, want ErrCorruptManifest", err)
	}
	res, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{broken.ID, broken.ID}})
	if err != nil {
		t.Fatalf("Forget by id: %v", err)
	}
	if len(res.Remove) != 1 || res.Remove[0].ID != broken.ID {
		t.Fatalf("Forget removed %v, want only %s", res.Remove, broken.ID)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].ID != healthy.ID {
		t.Fatalf("Snapshots = %v, want only %s", snaps, healthy.ID)
	}
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if vr, err := repo.Verify(ctx, healthy.ID); err != nil || !vr.OK {
		t.Fatalf("Verify after prune: %+v, %v", vr, err)
	}
}

func assertCorruptManifest(t *testing.T, repo *ark.Repository, id string) {
	t.Helper()
	ctx := context.Background()

	if _, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); !errors.Is(err, ark.ErrCorruptManifest) {
		t.Errorf("Snapshots returned %v, want ErrCorruptManifest", err)
	}
	if _, err := repo.Snapshot(ctx, id); !errors.Is(err, ark.ErrCorruptManifest) {
		t.Errorf("Snapshot returned %v, want ErrCorruptManifest", err)
	}
	if _, err := repo.Verify(ctx, id); !errors.Is(err, ark.ErrCorruptManifest) {
		t.Errorf("Verify returned %v, want ErrCorruptManifest", err)
	}
	result, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SnapshotsChecked != 1 || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], id) {
		t.Errorf("Check reports %d snapshots, errors %v, want one error naming snapshot %s",
			result.SnapshotsChecked, result.Errors, id)
	}
	if _, err := repo.Prune(ctx); !errors.Is(err, ark.ErrCorruptManifest) {
		t.Errorf("Prune returned %v, want ErrCorruptManifest", err)
	}
}

// TestScenarioVerifyFindsAMissingChunk is the other half: the object is gone,
// which is a different result field from a corrupt one.
func TestScenarioVerifyFindsAMissingChunk(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "the original contents"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	names := objectNames(t, b, ark.BlobFile)
	if len(names) != 1 {
		t.Fatalf("the snapshot holds %d chunks, want 1", len(names))
	}
	gone := names[0]
	if err := b.Delete(ctx, ark.Handle{Type: ark.BlobFile, Name: gone}); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	result, err := repo.Verify(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.OK {
		t.Error("Verify passed over a missing chunk")
	}
	if len(result.MissingChunks) != 1 || result.MissingChunks[0] != gone {
		t.Errorf("Verify reports missing %v, want [%s]", result.MissingChunks, gone)
	}
}

// TestScenarioCheckCoversEverySnapshot: check is verify over the whole
// repository, so it counts every snapshot and names the one that broke.
func TestScenarioCheckCoversEverySnapshot(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	var snaps []*ark.Snapshot
	for _, content := range []string{"one", "two", "three"} {
		snap, err := repo.Backup(ctx, stubSource{name: "app", content: content})
		if err != nil {
			t.Fatalf("Backup %s: %v", content, err)
		}
		snaps = append(snaps, snap)
	}

	result, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SnapshotsChecked != 3 {
		t.Errorf("Check looked at %d snapshots, want 3", result.SnapshotsChecked)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Check on a healthy repository reports %v", result.Errors)
	}

	// Break the chunk of the middle snapshot. Every snapshot has its own
	// content, so exactly one of them loses it.
	broken := snaps[1]
	chunk := ark.ObjectName(repo, []byte("two"))
	if err := b.Delete(ctx, ark.Handle{Type: ark.BlobFile, Name: chunk}); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	result, err = repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SnapshotsChecked != 3 {
		t.Errorf("Check looked at %d snapshots, want 3", result.SnapshotsChecked)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Check reports %d errors, want 1: %v", len(result.Errors), result.Errors)
	}
	if !strings.Contains(result.Errors[0], broken.ID) || !strings.Contains(result.Errors[0], chunk) {
		t.Errorf("Check reports %q, want it to name snapshot %s and chunk %s",
			result.Errors[0], broken.ID, chunk)
	}
}

// TestScenarioPruneIsRefusedWhileASharedLockExists is the lock table seen
// from the outside: prune deletes objects other operations may be reading, so
// it needs the repository to itself.
func TestScenarioPruneIsRefusedWhileASharedLockExists(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := scenarioRepo(t, b)

	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	putForeignLock(t, repo, false)

	_, err := repo.Prune(ctx)
	if !errors.Is(err, ark.ErrRepoLocked) {
		t.Fatalf("Prune under a shared lock returned %v, want ErrRepoLocked", err)
	}

	// A shared lock is exactly what the other operations take, so they run.
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "two"}); err != nil {
		t.Errorf("Backup under a foreign shared lock failed: %v", err)
	}
	if _, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil {
		t.Errorf("Snapshots under a foreign shared lock failed: %v", err)
	}
}

// putForeignLock writes a lock object of another host, which cannot be
// inspected from here and is therefore never stale while it is young.
func putForeignLock(t *testing.T, repo *ark.Repository, exclusive bool) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"time":      time.Now().UTC(),
		"exclusive": exclusive,
		"hostname":  "elsewhere.example",
		"username":  "operator",
		"pid":       4242,
		"uid":       1000,
		"gid":       1000,
	})
	if err != nil {
		t.Fatalf("marshal lock: %v", err)
	}
	h := ark.Handle{Type: ark.LockFile, Name: "00000000000000000000000000"}
	if err := ark.PutObject(context.Background(), repo, h, data); err != nil {
		t.Fatalf("put lock: %v", err)
	}
}

// TestScenarioCancelledBackupLeavesNoManifest: the manifest is written last,
// so an aborted backup leaves unreferenced objects for prune and no snapshot
// anybody can see.
func TestScenarioCancelledBackupLeavesNoManifest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := arkmem.New()
	repo := scenarioRepo(t, b)

	_, err := repo.Backup(ctx, cancellingSource{cancel: cancel})
	if err == nil {
		t.Fatal("the cancelled backup returned no error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled backup returned %v, want context.Canceled", err)
	}

	if names := objectNames(t, b, ark.SnapshotFile); len(names) != 0 {
		t.Errorf("the cancelled backup left the manifests %v", names)
	}
	snaps, err := repo.Snapshots(context.Background(), ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("the repository holds %d snapshots, want none", len(snaps))
	}
}

// cancellingSource yields one file and then cancels the backup, the way a
// user hitting ctrl-c does.
type cancellingSource struct{ cancel context.CancelFunc }

func (s cancellingSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "app", Type: ark.SourceTypeFile}
}

func (s cancellingSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	info := ark.FileInfo{Path: "data.txt", Size: 5, Mode: 0o644, Type: ark.FileTypeRegular}
	if err := fn(info, strings.NewReader("first")); err != nil {
		return err
	}
	s.cancel()
	return ctx.Err()
}
