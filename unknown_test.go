package ark_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// corruptSentinels are the errors a damaged object is reported under. What a
// build does not know is none of them: it is ErrUnsupportedFormat alone.
var corruptSentinels = []error{
	ark.ErrCorruptChunk, ark.ErrCorruptTree, ark.ErrCorruptIndex, ark.ErrCorruptManifest, ark.ErrCorruptConfig,
}

// wantUnsupported fails the test unless err is ErrUnsupportedFormat, names
// what the build did not know and wraps no corrupt sentinel.
func wantUnsupported(t *testing.T, op string, err error, names string) {
	t.Helper()
	if !errors.Is(err, ark.ErrUnsupportedFormat) {
		t.Errorf("%s = %v, want ErrUnsupportedFormat", op, err)
		return
	}
	if !strings.Contains(err.Error(), names) {
		t.Errorf("%s = %q, want it to name %s", op, err, names)
	}
	for _, corrupt := range corruptSentinels {
		if errors.Is(err, corrupt) {
			t.Errorf("%s = %v, which also reports corruption (%v)", op, err, corrupt)
		}
	}
}

// readers are the operations that read a snapshot. None of them may pass over
// something in it that the build does not know.
func readers(repo *ark.Repository, id ark.SnapshotID) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"Restore": func(ctx context.Context) error {
			_, err := repo.Restore(ctx, id, discardSink{})
			return err
		},
		"Verify": func(ctx context.Context) error {
			_, err := repo.Verify(ctx, id)
			return err
		},
		"Check": func(ctx context.Context) error {
			_, err := repo.Check(ctx)
			return err
		},
		"Export": func(ctx context.Context) error { return repo.Export(ctx, id, io.Discard) },
		"Walk": func(ctx context.Context) error {
			return repo.Walk(ctx, id, func(ark.FileInfo) error { return nil })
		},
		"Reader": func(ctx context.Context) error {
			rd, err := repo.OpenReader(ctx)
			if err != nil {
				return err
			}
			defer rd.Close()
			fsys, err := rd.FS(id)
			if err != nil {
				return err
			}
			return fs.WalkDir(fsys, ".", func(_ string, _ fs.DirEntry, err error) error { return err })
		},
		"Prune": func(ctx context.Context) error {
			_, err := repo.Prune(ctx)
			return err
		},
	}
}

// TestUnknownFileTypeFailsEveryReader: a tree entry of a type a later build
// added is not an entry to leave out. A restore without it would be a
// snapshot with a file missing and no error, and a prune that took it for an
// entry holding nothing would delete what it references.
func TestUnknownFileTypeFailsEveryReader(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo, id := backupFixture(t, b)
	if err := ark.RetypeEntry(ctx, repo, id, "zeta.txt", "door"); err != nil {
		t.Fatalf("RetypeEntry: %v", err)
	}
	packs := objectNames(t, b, ark.DataPackFile)

	for name, read := range readers(repo, id) {
		wantUnsupported(t, name, read(ctx), `file type "door"`)
	}
	if got := objectNames(t, b, ark.DataPackFile); len(got) != len(packs) {
		t.Errorf("the prune left %d data packs of %d", len(got), len(packs))
	}
}

// linkIntoDirSource yields a file in a directory and a hardlink to it from
// the root, so the link's target is in another tree node than the link.
type linkIntoDirSource struct{}

func (linkIntoDirSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "linked", Type: ark.SourceTypeFile}
}

func (linkIntoDirSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	if err := fn(ark.FileInfo{Path: "dir", Mode: 0o755, Type: ark.FileTypeDirectory}, nil); err != nil {
		return err
	}
	file := ark.FileInfo{Path: "dir/file", Size: 2, Mode: 0o644, Type: ark.FileTypeRegular}
	if err := fn(file, openReader(strings.NewReader("ok"))); err != nil {
		return err
	}
	return fn(ark.FileInfo{Path: "link", Mode: 0o644, Type: ark.FileTypeHardlink, LinkDest: "dir/file"}, nil)
}

// TestHardlinkIntoAnUnknownNodeIsUnsupported: a Reader resolves a hardlink
// through the node of its target. When a later build wrote that node, the
// link is unsupported like the node and is not a corrupt tree.
func TestHardlinkIntoAnUnknownNodeIsUnsupported(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	snap, err := repo.Backup(ctx, linkIntoDirSource{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	nodes := blobNames(t, repo, ark.TreeFile)
	if len(nodes) != 2 {
		t.Fatalf("the snapshot has %d tree nodes, want the root and dir", len(nodes))
	}

	// One of the two fails on the way to the link, the other at its target.
	var atTarget int
	for _, name := range nodes {
		h := ark.Handle{Type: ark.TreeFile, Name: name}
		stored := readBlob(t, b, repo, h.Type, h.Name)
		later, err := ark.Retag(repo, h, stored, 0x04)
		if err != nil {
			t.Fatalf("retag: %v", err)
		}
		writeBlob(t, b, repo, h.Type, h.Name, later)

		rd, err := repo.OpenReader(ctx)
		if err != nil {
			t.Fatalf("OpenReader: %v", err)
		}
		fsys, err := rd.FS(snap.ID)
		if err != nil {
			t.Fatalf("FS: %v", err)
		}
		_, err = fs.Stat(fsys, "link")
		wantUnsupported(t, "Stat(link)", err, "0x04")
		if err != nil && strings.Contains(err.Error(), "hardlink link to dir/file") {
			atTarget++
		}
		_ = rd.Close()
		writeBlob(t, b, repo, h.Type, h.Name, stored)
	}
	if atTarget != 1 {
		t.Errorf("%d of the two reads failed at the link's target, want 1", atTarget)
	}
}

// TestUnknownFormatByteIsRejected stores one object per namespace as a later
// build with another codec would: sealed, under a format byte this build
// does not know. The operation fails as unsupported and names the byte: no
// verify lists the object as corrupt, no index load or rebuild skips it and
// no lock holder is ignored. The byte merely changed on a stored object is
// something else, damage, since the tag covers the byte.
func TestUnknownFormatByteIsRejected(t *testing.T) {
	const unknown = 0x04
	type step = func(*testing.T, ark.Backend, *ark.Repository)
	retag := func(t *testing.T, repo *ark.Repository, h ark.Handle, obj []byte) []byte {
		t.Helper()
		out, err := ark.Retag(repo, h, obj, unknown)
		if err != nil {
			t.Fatalf("retag %s/%s: %v", h.Type, h.Name, err)
		}
		return out
	}
	inPack := func(typ ark.HandleType) step {
		return func(t *testing.T, b ark.Backend, repo *ark.Repository) {
			name := blobNames(t, repo, typ)[0]
			obj := readBlob(t, b, repo, typ, name)
			writeBlob(t, b, repo, typ, name, retag(t, repo, ark.Handle{Type: typ, Name: name}, obj))
		}
	}
	whole := func(typ ark.HandleType) step {
		return func(t *testing.T, b ark.Backend, repo *ark.Repository) {
			h := ark.Handle{Type: typ, Name: objectNames(t, b, typ)[0]}
			putObject(t, b, h.Type, h.Name, retag(t, repo, h, readObject(t, b, h.Type, h.Name)))
		}
	}
	table := func(t *testing.T, b ark.Backend, repo *ark.Repository) {
		retagTable(t, b, func(obj []byte) []byte {
			return retag(t, repo, ark.Handle{Type: ark.DataPackFile, Name: ark.PackTableName}, obj)
		})
	}
	lock := func(t *testing.T, b ark.Backend, repo *ark.Repository) {
		h := ark.Handle{Type: ark.LockFile, Name: "held-by-a-later-build"}
		putObject(t, b, h.Type, h.Name, retag(t, repo, h, sealedAs(t, repo, b, h, []byte(`{"exclusive":false}`))))
	}
	for name, tc := range map[string]struct {
		store step
		ops   []string
	}{
		"chunk":       {inPack(ark.BlobFile), []string{"Restore", "Verify", "Check", "Export"}},
		"tree node":   {inPack(ark.TreeFile), []string{"Restore", "Verify", "Check", "Prune"}},
		"data index":  {whole(ark.DataIndexFile), []string{"Restore", "Verify", "Check", "Prune"}},
		"tree index":  {whole(ark.TreeIndexFile), []string{"Restore", "Verify", "Check", "Prune"}},
		"manifest":    {whole(ark.SnapshotFile), []string{"Restore", "Verify", "Check", "Prune"}},
		"pack table":  {table, []string{"RebuildIndex"}},
		"lock object": {lock, []string{"Restore", "Check", "Prune"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := arkmem.New()
			repo, id := backupFixture(t, b)
			tc.store(t, b, repo)

			all := readers(repo, id)
			all["RebuildIndex"] = func(ctx context.Context) error {
				_, err := repo.RebuildIndex(ctx)
				return err
			}
			for _, op := range tc.ops {
				wantUnsupported(t, op, all[op](ctx), "0x04")
			}
		})
	}

	t.Run("byte changed on a stored chunk", func(t *testing.T) {
		b := arkmem.New()
		repo, id := backupFixture(t, b)
		chunk := blobNames(t, repo, ark.BlobFile)[0]
		obj := readBlob(t, b, repo, ark.BlobFile, chunk)
		obj[0] = unknown
		writeBlob(t, b, repo, ark.BlobFile, chunk, obj)

		_, err := repo.Restore(context.Background(), id, discardSink{})
		if !errors.Is(err, ark.ErrCorruptChunk) || errors.Is(err, ark.ErrUnsupportedFormat) {
			t.Errorf("Restore = %v, want ErrCorruptChunk alone", err)
		}
	})
}

// retagTable rewrites the blob table of the one data pack with edit and
// stores the pack under the name its bytes then hash to, so only the table
// itself differs.
func retagTable(t *testing.T, b ark.Backend, edit func([]byte) []byte) {
	t.Helper()
	old := objectNames(t, b, ark.DataPackFile)[0]
	section, table := splitPack(t, readObject(t, b, ark.DataPackFile, old))
	table = edit(bytes.Clone(table))
	pack := binary.LittleEndian.AppendUint32(append(bytes.Clone(section), table...), uint32(len(table)))
	sum := sha256.Sum256(pack)
	putObject(t, b, ark.DataPackFile, hex.EncodeToString(sum[:]), pack)
	if err := b.Delete(context.Background(), ark.Handle{Type: ark.DataPackFile, Name: old}); err != nil {
		t.Fatalf("delete the old pack: %v", err)
	}
}

// withKey returns the JSON object data with the key "later" added, in the
// object itself or in the object under each name of path.
func withKey(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", data, err)
	}
	if len(path) == 0 {
		obj["later"] = json.RawMessage(`true`)
	} else {
		obj[path[0]] = withKey(t, obj[path[0]], path[1:]...)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// TestManifestIsDecodedStrictly: a manifest with a key this build does not
// know, in the object or in its stats, or of another version, is one a later
// build wrote. Every reader refuses it, and none calls it corrupt, which a
// forget by id would take as leave to remove it.
func TestManifestIsDecodedStrictly(t *testing.T) {
	for name, tc := range map[string]struct {
		edit  func(t *testing.T, m []byte) []byte
		names string
	}{
		"unknown key":          {func(t *testing.T, m []byte) []byte { return withKey(t, m) }, `key "later"`},
		"unknown key in stats": {func(t *testing.T, m []byte) []byte { return withKey(t, m, "stats") }, `key "later"`},
		"later version": {func(_ *testing.T, m []byte) []byte {
			return bytes.Replace(m, []byte(`"version":1`), []byte(`"version":2`), 1)
		}, "version 2"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := arkmem.New()
			repo, id := backupFixture(t, b)
			h := ark.Handle{Type: ark.SnapshotFile, Name: string(id)}
			m, err := ark.OpenObject(ctx, repo, h.Type, h.Name)
			if err != nil {
				t.Fatalf("open the manifest: %v", err)
			}
			if err := ark.PutObject(ctx, repo, h, tc.edit(t, m)); err != nil {
				t.Fatalf("store the manifest: %v", err)
			}

			for op, read := range readers(repo, id) {
				wantUnsupported(t, op, read(ctx), tc.names)
			}
			_, err = repo.Snapshots(ctx, ark.SnapshotFilter{})
			wantUnsupported(t, "Snapshots", err, tc.names)
			_, err = repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{id}})
			wantUnsupported(t, "Forget", err, tc.names)
		})
	}
}

// TestLockWithAnUnknownKeyStopsTheOperation: a lock this build cannot read in
// full may guard something it does not know of, so it is not passed over like
// a lock that does not open. Unlock WithForce is the way out, as for those.
func TestLockWithAnUnknownKeyStopsTheOperation(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo, id := backupFixture(t, b)
	h := ark.Handle{Type: ark.LockFile, Name: "held-by-a-later-build"}
	if err := ark.PutObject(ctx, repo, h, []byte(`{"exclusive":false,"later":true}`)); err != nil {
		t.Fatalf("store the lock: %v", err)
	}

	for op, read := range readers(repo, id) {
		wantUnsupported(t, op, read(ctx), `key "later"`)
	}
	_, err := repo.Backup(ctx, stubSource{name: "src", content: "content"})
	wantUnsupported(t, "Backup", err, `key "later"`)
	_, err = repo.Locks(ctx)
	wantUnsupported(t, "Locks", err, `key "later"`)

	if n, err := repo.Unlock(ctx); n != 0 || err != nil {
		t.Errorf("Unlock removed %d locks and returned %v, want it to leave the lock", n, err)
	}
	if n, err := repo.Unlock(ctx, ark.WithForce()); n != 1 || err != nil {
		t.Fatalf("Unlock WithForce removed %d locks and returned %v, want 1", n, err)
	}
	if _, err := repo.Restore(ctx, id, discardSink{}); err != nil {
		t.Errorf("Restore after the unlock: %v", err)
	}
}
