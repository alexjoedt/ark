package ark_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// TestTamperedObjectsAreRejected is the integrity claim of the repository
// format as one matrix: every sealed namespace times every way an attacker
// with write access to the backend can change an object. Each cell mutates
// the victim object of its namespace and asserts what every consumer of that
// namespace sees. The config object is plaintext and has its own row,
// TestTamperedConfigIsRejected.
//
// A chunk or tree node is sealed on its own and stored inside a pack. Its
// cells change its sealed bytes where the pack holds them; the cells that
// replace a whole object (truncated, swapped, moved) do that to its pack.
//
// Consumers per namespace:
//
//	keys       Open fails with ErrWrongPassword or ErrUnsupportedFormat
//	chunks     Restore fails with ErrCorruptChunk, Verify lists the chunk in
//	           CorruptChunks with OK false, Check lists it under Errors
//	trees      Restore and Verify fail with ErrCorruptTree, Check lists the
//	           snapshot under Errors
//	dataindex  the object is skipped, so its chunks are missing: Restore
//	           fails with ErrNotFound, Verify lists the chunk in MissingChunks
//	           with OK false, Check lists it under Errors
//	treeindex  the object is skipped, so its tree nodes are missing: Restore
//	           and Verify fail with ErrNotFound, Check lists the snapshot
//	           under Errors
//	snapshots  Snapshots, Restore and Verify fail with ErrCorruptManifest,
//	           Check lists the snapshot under Errors
//	locks      the lock is absent for Locks and Backup, Check lists it under
//	           Warnings
//
// The tag binds key, namespace and name (objectAAD), so it alone rejects
// every cell: Restore reads through a non-verifying store and still fails on
// a moved or swapped chunk or tree. No cell restores other bytes than the
// backup held.
func TestTamperedObjectsAreRejected(t *testing.T) {
	for _, ns := range tamperNamespaces {
		for _, mut := range tamperMutations {
			t.Run(ns.typ.String()+"/"+mut.name, func(t *testing.T) {
				f := newTamperFixture(t)
				victim, other := ns.setup(t, f)
				if mut.apply == nil {
					ns.accepted(t, f, victim)
					return
				}
				mut.apply(t, f, ns.typ, victim, other)
				ns.rejected(t, f, victim)
			})
		}
	}
}

// TestTamperedConfigIsRejected is the config row of the integrity matrix.
// The master key carries the SHA-256 of the config bytes Init stored, so
// every change to them fails Open with ErrCorruptConfig. The password is
// changed first: the binding survives ChangePassword. A changed version is
// a tamper too: the version is only trusted once the hash matches.
func TestTamperedConfigIsRejected(t *testing.T) {
	ctx := context.Background()
	pw := ark.WithPassword("changed password")
	cells := []struct {
		name   string
		mutate func(t *testing.T, cfg []byte) []byte
		want   error
	}{
		{"unchanged", nil, nil},
		{"flipped id byte", func(_ *testing.T, cfg []byte) []byte {
			cfg[bytes.Index(cfg, []byte(`"id":"`))+len(`"id":"`)] ^= 0x01
			return cfg
		}, ark.ErrCorruptConfig},
		{"flipped chunker byte", func(_ *testing.T, cfg []byte) []byte {
			cfg[bytes.Index(cfg, []byte(`"min":`))+len(`"min":`)] ^= 0x01
			return cfg
		}, ark.ErrCorruptConfig},
		{"truncated", func(_ *testing.T, cfg []byte) []byte { return cfg[:len(cfg)/2] }, ark.ErrCorruptConfig},
		{"whitespace", func(_ *testing.T, cfg []byte) []byte { return append(cfg, '\n') }, ark.ErrCorruptConfig},
		{"other repository", func(t *testing.T, _ []byte) []byte {
			b2 := initRepo(t, pw)
			return readObject(t, b2, ark.ConfigFile, "config")
		}, ark.ErrCorruptConfig},
		{"flipped version byte", func(_ *testing.T, cfg []byte) []byte {
			cfg[bytes.Index(cfg, []byte(`"version":`))+len(`"version":`)] ^= 0x01
			return cfg
		}, ark.ErrCorruptConfig},
	}
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			b := arkmem.New()
			repo := openRepo(t, b)
			if err := repo.ChangePassword(ctx, pw); err != nil {
				t.Fatalf("ChangePassword: %v", err)
			}
			if c.mutate != nil {
				cfg := c.mutate(t, readObject(t, b, ark.ConfigFile, "config"))
				h := ark.Handle{Type: ark.ConfigFile, Name: "config"}
				if err := b.Put(ctx, h, bytes.NewReader(cfg)); err != nil {
					t.Fatalf("put config: %v", err)
				}
			}
			got, err := ark.Open(ctx, b, pw)
			if err == nil {
				got.Close()
			}
			if !errors.Is(err, c.want) {
				t.Errorf("Open = %v, want %v", err, c.want)
			}
		})
	}
}

const (
	victimContent = "the victim's contents"
	otherContent  = "some other contents"
)

type tamperFixture struct {
	b             *arkmem.Backend
	repo          *ark.Repository
	victim, other *ark.Snapshot
	// index names the index object each backup wrote, the victim's first.
	index map[ark.HandleType][2]string
}

func newTamperFixture(t *testing.T) *tamperFixture {
	t.Helper()
	ctx := context.Background()
	f := &tamperFixture{b: arkmem.New()}
	f.repo = openRepo(t, f.b)
	var err error
	if f.victim, err = f.repo.Backup(ctx, stubSource{name: "victim", content: victimContent}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	first := map[ark.HandleType][]string{}
	for _, typ := range []ark.HandleType{ark.DataIndexFile, ark.TreeIndexFile} {
		first[typ] = objectNames(t, f.b, typ)
	}
	if f.other, err = f.repo.Backup(ctx, stubSource{name: "other", content: otherContent}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	f.index = make(map[ark.HandleType][2]string)
	for typ, victim := range first {
		other := slices.DeleteFunc(objectNames(t, f.b, typ), func(n string) bool { return slices.Contains(victim, n) })
		if len(victim) != 1 || len(other) != 1 {
			t.Fatalf("%s objects per backup: %v and %v, want one each", typ, victim, other)
		}
		f.index[typ] = [2]string{victim[0], other[0]}
	}
	return f
}

// stored returns the object that stores name of namespace typ, and where in
// it the sealed bytes are: the pack of a chunk or tree node, else the object
// itself.
func (f *tamperFixture) stored(t *testing.T, typ ark.HandleType, name string) (ark.Handle, int64, int64) {
	t.Helper()
	if typ != ark.BlobFile && typ != ark.TreeFile {
		return ark.Handle{Type: typ, Name: name}, 0, int64(len(readObject(t, f.b, typ, name)))
	}
	pack, offset, length, err := ark.BlobPlace(context.Background(), f.repo, typ, name)
	if err != nil {
		t.Fatalf("place of %s/%s: %v", typ, name, err)
	}
	return pack, offset, length
}

// whole returns the stored object of name and its handle.
func (f *tamperFixture) whole(t *testing.T, typ ark.HandleType, name string) (ark.Handle, []byte) {
	t.Helper()
	h, _, _ := f.stored(t, typ, name)
	return h, readObject(t, f.b, h.Type, h.Name)
}

func (f *tamperFixture) put(t *testing.T, typ ark.HandleType, name string, obj []byte) {
	t.Helper()
	if err := f.b.Put(context.Background(), ark.Handle{Type: typ, Name: name}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("put %s/%s: %v", typ, name, err)
	}
}

func (f *tamperFixture) del(t *testing.T, typ ark.HandleType, name string) {
	t.Helper()
	if err := f.b.Delete(context.Background(), ark.Handle{Type: typ, Name: name}); err != nil {
		t.Fatalf("delete %s/%s: %v", typ, name, err)
	}
}

// plaintext returns what the object under name holds once opened. A key file
// is sealed under the passphrase, not the data key, so its body stands in.
func (f *tamperFixture) plaintext(t *testing.T, typ ark.HandleType, name string) []byte {
	t.Helper()
	if typ == ark.KeyFile {
		return readObject(t, f.b, typ, name)
	}
	data, err := ark.OpenObject(context.Background(), f.repo, typ, name)
	if err != nil {
		t.Fatalf("open %s/%s: %v", typ, name, err)
	}
	return data
}

// sealedAs returns data sealed by repo the way it would store it under h.
func sealedAs(t *testing.T, repo *ark.Repository, b ark.Backend, h ark.Handle, data []byte) []byte {
	t.Helper()
	if err := ark.PutObject(context.Background(), repo, h, data); err != nil {
		t.Fatalf("seal %s/%s: %v", h.Type, h.Name, err)
	}
	obj := readObject(t, b, h.Type, h.Name)
	if err := b.Delete(context.Background(), h); err != nil {
		t.Fatalf("delete %s/%s: %v", h.Type, h.Name, err)
	}
	return obj
}

// editSealed rewrites the sealed bytes of the object under name: the whole
// object, the bytes of a chunk or tree node in its pack, or the hex sealed
// key inside a key file. A pack is cut off behind bytes that edit shortened.
func (f *tamperFixture) editSealed(t *testing.T, typ ark.HandleType, name string, edit func([]byte) []byte) {
	t.Helper()
	if typ != ark.KeyFile {
		h, offset, length := f.stored(t, typ, name)
		whole := readObject(t, f.b, h.Type, h.Name)
		edited := edit(bytes.Clone(whole[offset : offset+length]))
		out := append(bytes.Clone(whole[:offset]), edited...)
		if int64(len(edited)) == length {
			out = append(out, whole[offset+length:]...)
		}
		f.put(t, h.Type, h.Name, out)
		return
	}
	obj := readObject(t, f.b, typ, name)
	var kf map[string]any
	if err := json.Unmarshal(obj, &kf); err != nil {
		t.Fatalf("parse key file: %v", err)
	}
	sealed, err := hex.DecodeString(kf["key"].(string))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	kf["key"] = hex.EncodeToString(edit(sealed))
	body, err := json.Marshal(kf)
	if err != nil {
		t.Fatalf("marshal key file: %v", err)
	}
	f.put(t, typ, name, body)
}

// flipAt returns a mutation that flips one byte at the offset off picks.
func flipAt(name string, off func(n int) int) tamperMutation {
	return tamperMutation{name, func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, _ string) {
		f.editSealed(t, typ, victim, func(obj []byte) []byte {
			obj[off(len(obj))] ^= 0x01
			return obj
		})
	}}
}

// Offsets of a sealed object: format byte, 24-byte nonce, ciphertext of the
// zstd frame, 16-byte tag (objectstore.go seal).
const (
	nonceStart = 1
	frameStart = nonceStart + 24
	tagSize    = 16
)

// foreignNamespace is where the cross-namespace cell seals the victim's
// content before copying it into the victim's namespace. The config object is
// plaintext; TestTamperedConfigIsRejected covers it.
//
//nolint:exhaustive // see above
var foreignNamespace = map[ark.HandleType]ark.HandleType{
	ark.KeyFile:       ark.SnapshotFile,
	ark.BlobFile:      ark.TreeFile,
	ark.TreeFile:      ark.BlobFile,
	ark.DataIndexFile: ark.TreeIndexFile,
	ark.TreeIndexFile: ark.DataIndexFile,
	ark.SnapshotFile:  ark.LockFile,
	ark.LockFile:      ark.SnapshotFile,
}

type tamperMutation struct {
	name string
	// apply changes the victim object; nil is the unchanged control cell.
	apply func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, other string)
}

var tamperMutations = []tamperMutation{
	{"unchanged", nil},
	flipAt("format byte", func(int) int { return 0 }),
	flipAt("nonce", func(int) int { return nonceStart + 12 }),
	flipAt("ciphertext", func(int) int { return frameStart }),
	flipAt("tag", func(n int) int { return n - tagSize/2 }),
	{"truncated", func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, _ string) {
		f.editSealed(t, typ, victim, func(obj []byte) []byte { return obj[:len(obj)/2] })
	}},
	{"swapped", func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, other string) {
		hv, v := f.whole(t, typ, victim)
		ho, o := f.whole(t, typ, other)
		f.put(t, hv.Type, hv.Name, o)
		f.put(t, ho.Type, ho.Name, v)
	}},
	{"moved", func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, other string) {
		hv, _ := f.whole(t, typ, victim)
		ho, o := f.whole(t, typ, other)
		f.put(t, hv.Type, hv.Name, o)
		f.del(t, ho.Type, ho.Name)
	}},
	{"other namespace", func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, _ string) {
		h := ark.Handle{Type: foreignNamespace[typ], Name: victim}
		obj := sealedAs(t, f.repo, f.b, h, f.plaintext(t, typ, victim))
		f.editSealed(t, typ, victim, func([]byte) []byte { return obj })
	}},
	{"other repository", func(t *testing.T, f *tamperFixture, typ ark.HandleType, victim, _ string) {
		b2 := arkmem.New()
		repo2 := openRepo(t, b2)
		if typ == ark.KeyFile {
			_, body := onlyKeyFile(t, b2)
			f.put(t, typ, victim, body)
			return
		}
		h := ark.Handle{Type: typ, Name: victim}
		obj := sealedAs(t, repo2, b2, h, f.plaintext(t, typ, victim))
		f.editSealed(t, typ, victim, func([]byte) []byte { return obj })
	}},
}

type tamperNamespace struct {
	typ ark.HandleType
	// setup returns the names of the victim object and of a second object of
	// the namespace.
	setup func(t *testing.T, f *tamperFixture) (victim, other string)
	// accepted asserts the control cell, rejected every mutation.
	accepted, rejected func(t *testing.T, f *tamperFixture, victim string)
}

var tamperNamespaces = []tamperNamespace{
	{
		typ: ark.KeyFile,
		setup: func(t *testing.T, f *tamperFixture) (string, string) {
			victim, body := onlyKeyFile(t, f.b)
			if err := f.repo.ChangePassword(context.Background(), ark.WithPassword("second password")); err != nil {
				t.Fatalf("ChangePassword: %v", err)
			}
			other, _ := onlyKeyFile(t, f.b)
			putObject(t, f.b, ark.KeyFile, victim, body)
			return victim, other
		},
		accepted: func(t *testing.T, f *tamperFixture, _ string) {
			repo, err := ark.Open(context.Background(), f.b, testPassword)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			repo.Close()
		},
		rejected: func(t *testing.T, f *tamperFixture, _ string) {
			repo, err := ark.Open(context.Background(), f.b, testPassword)
			if err == nil {
				repo.Close()
				t.Fatal("Open succeeded over a mutated key file")
			}
			if !errors.Is(err, ark.ErrWrongPassword) && !errors.Is(err, ark.ErrUnsupportedFormat) {
				t.Errorf("Open = %v, want ErrWrongPassword or ErrUnsupportedFormat", err)
			}
		},
	},
	{
		typ: ark.BlobFile,
		setup: func(_ *testing.T, f *tamperFixture) (string, string) {
			return ark.ObjectName(f.repo, []byte(victimContent)), ark.ObjectName(f.repo, []byte(otherContent))
		},
		accepted: acceptedSnapshot,
		rejected: func(t *testing.T, f *tamperFixture, victim string) {
			assertRestoreFails(t, f, ark.ErrCorruptChunk)
			res, err := f.repo.Verify(context.Background(), f.victim.ID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.OK || !slices.Contains(res.CorruptChunks, victim) {
				t.Errorf("Verify = OK %v, corrupt %v, want not OK with chunk %s", res.OK, res.CorruptChunks, victim)
			}
			assertCheckError(t, f, victim)
		},
	},
	{
		typ: ark.TreeFile,
		setup: func(t *testing.T, f *tamperFixture) (string, string) {
			return snapshotTree(t, f, f.victim.ID), snapshotTree(t, f, f.other.ID)
		},
		accepted: acceptedSnapshot,
		rejected: func(t *testing.T, f *tamperFixture, _ string) {
			assertRestoreFails(t, f, ark.ErrCorruptTree)
			assertVerifyFails(t, f, ark.ErrCorruptTree)
			assertCheckError(t, f, f.victim.ID)
		},
	},
	{
		typ: ark.SnapshotFile,
		setup: func(_ *testing.T, f *tamperFixture) (string, string) {
			return f.victim.ID, f.other.ID
		},
		accepted: acceptedSnapshot,
		rejected: func(t *testing.T, f *tamperFixture, _ string) {
			_, err := f.repo.Snapshots(context.Background(), ark.SnapshotFilter{})
			if !errors.Is(err, ark.ErrCorruptManifest) {
				t.Errorf("Snapshots = %v, want ErrCorruptManifest", err)
			}
			assertRestoreFails(t, f, ark.ErrCorruptManifest)
			assertVerifyFails(t, f, ark.ErrCorruptManifest)
			assertCheckError(t, f, f.victim.ID)
		},
	},
	{
		typ: ark.LockFile,
		setup: func(t *testing.T, f *tamperFixture) (string, string) {
			// Only the victim blocks a backup, so a cell that leaves the
			// other lock intact still tells whether the victim counts.
			putTamperLock(t, f, ark.LockFile, "victim-lock", true)
			putTamperLock(t, f, ark.LockFile, "other-lock", false)
			return "victim-lock", "other-lock"
		},
		accepted: func(t *testing.T, f *tamperFixture, victim string) {
			if !hasLock(t, f, victim) {
				t.Errorf("Locks does not list the intact lock %s", victim)
			}
			_, err := f.repo.Backup(context.Background(), stubSource{name: "new", content: "new"})
			if !errors.Is(err, ark.ErrRepoLocked) {
				t.Errorf("Backup under an intact exclusive lock = %v, want ErrRepoLocked", err)
			}
		},
		rejected: func(t *testing.T, f *tamperFixture, victim string) {
			if hasLock(t, f, victim) {
				t.Errorf("Locks lists the mutated lock %s", victim)
			}
			if _, err := f.repo.Backup(context.Background(), stubSource{name: "new", content: "new"}); err != nil {
				t.Errorf("Backup beside a mutated lock: %v", err)
			}
			assertCheckWarning(t, f, victim, "does not open")
		},
	},
	{
		typ: ark.DataIndexFile,
		setup: func(_ *testing.T, f *tamperFixture) (string, string) {
			return f.index[ark.DataIndexFile][0], f.index[ark.DataIndexFile][1]
		},
		accepted: acceptedSnapshot,
		rejected: func(t *testing.T, f *tamperFixture, _ string) {
			chunk := ark.ObjectName(f.repo, []byte(victimContent))
			assertRestoreFails(t, f, ark.ErrNotFound)
			res, err := f.repo.Verify(context.Background(), f.victim.ID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.OK || !slices.Contains(res.MissingChunks, chunk) {
				t.Errorf("Verify = OK %v, missing %v, want not OK with chunk %s", res.OK, res.MissingChunks, chunk)
			}
			assertCheckError(t, f, chunk)
		},
	},
	{
		typ: ark.TreeIndexFile,
		setup: func(_ *testing.T, f *tamperFixture) (string, string) {
			return f.index[ark.TreeIndexFile][0], f.index[ark.TreeIndexFile][1]
		},
		accepted: acceptedSnapshot,
		rejected: func(t *testing.T, f *tamperFixture, _ string) {
			assertRestoreFails(t, f, ark.ErrNotFound)
			assertVerifyFails(t, f, ark.ErrNotFound)
			assertCheckError(t, f, f.victim.ID)
		},
	},
}

func snapshotTree(t *testing.T, f *tamperFixture, id string) string {
	t.Helper()
	tree, err := ark.RootTree(context.Background(), f.repo, id)
	if err != nil {
		t.Fatalf("RootTree: %v", err)
	}
	return tree
}

// putTamperLock stores a lock object of another host under name.
func putTamperLock(t *testing.T, f *tamperFixture, typ ark.HandleType, name string, exclusive bool) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"time":      time.Now().UTC(),
		"exclusive": exclusive,
		"hostname":  "elsewhere.example",
		"pid":       4242,
	})
	if err != nil {
		t.Fatalf("marshal lock: %v", err)
	}
	if err := ark.PutObject(context.Background(), f.repo, ark.Handle{Type: typ, Name: name}, data); err != nil {
		t.Fatalf("put %s/%s: %v", typ, name, err)
	}
}

func hasLock(t *testing.T, f *tamperFixture, name string) bool {
	t.Helper()
	locks, err := f.repo.Locks(context.Background())
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	return slices.ContainsFunc(locks, func(l ark.LockInfo) bool { return l.ID == name })
}

// acceptedSnapshot is the control cell of the namespaces a snapshot reads.
func acceptedSnapshot(t *testing.T, f *tamperFixture, _ string) {
	t.Helper()
	ctx := context.Background()
	sink := &recordingSink{}
	if _, err := f.repo.Restore(ctx, f.victim.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := sink.written["data.txt"]; got != victimContent {
		t.Errorf("Restore wrote %q, want %q", got, victimContent)
	}
	if res, err := f.repo.Verify(ctx, f.victim.ID); err != nil || !res.OK {
		t.Errorf("Verify = %+v, %v, want OK", res, err)
	}
	res, err := f.repo.Check(ctx)
	if err != nil || len(res.Errors) != 0 || len(res.Warnings) != 0 {
		t.Errorf("Check = %+v, %v, want no findings", res, err)
	}
}

// assertRestoreFails restores the victim snapshot. A restore that succeeds
// fails the cell, naming whether it wrote other bytes than the backup held.
func assertRestoreFails(t *testing.T, f *tamperFixture, want error) {
	t.Helper()
	sink := &recordingSink{}
	_, err := f.repo.Restore(context.Background(), f.victim.ID, sink)
	switch {
	case err == nil && sink.written["data.txt"] != victimContent:
		t.Errorf("Restore succeeded with %q, the backup held %q", sink.written["data.txt"], victimContent)
	case err == nil:
		t.Error("Restore succeeded over a mutated object")
	case !errors.Is(err, want):
		t.Errorf("Restore = %v, want %v", err, want)
	}
}

func assertVerifyFails(t *testing.T, f *tamperFixture, want error) {
	t.Helper()
	res, err := f.repo.Verify(context.Background(), f.victim.ID)
	if res != nil && res.OK {
		t.Error("Verify returned OK over a mutated object")
	}
	if !errors.Is(err, want) {
		t.Errorf("Verify = %v, want %v", err, want)
	}
}

func assertCheckError(t *testing.T, f *tamperFixture, name string) {
	t.Helper()
	res, err := f.repo.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !slices.ContainsFunc(res.Errors, func(e string) bool { return strings.Contains(e, name) }) {
		t.Errorf("Check errors %q name no %s", res.Errors, name)
	}
}

func assertCheckWarning(t *testing.T, f *tamperFixture, name, detail string) {
	t.Helper()
	res, err := f.repo.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool {
		return strings.Contains(w, name) && strings.Contains(w, detail)
	}) {
		t.Errorf("Check warnings %q have none naming %s with %q", res.Warnings, name, detail)
	}
}

// blobGetBackend runs onGet before every read of a data pack, which is every
// chunk read; a non-nil error fails it.
type blobGetBackend struct {
	ark.Backend
	onGet func() error
}

func (b *blobGetBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type == ark.DataPackFile {
		if err := b.onGet(); err != nil {
			return nil, err
		}
	}
	return b.Backend.Get(ctx, h)
}

func (b *blobGetBackend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if h.Type == ark.DataPackFile {
		if err := b.onGet(); err != nil {
			return nil, err
		}
	}
	return b.Backend.GetRange(ctx, h, offset, length)
}

func TestVerifyReturnsAChunkReadFailure(t *testing.T) {
	ctx := context.Background()
	flaky := errors.New("flaky backend")
	b := &blobGetBackend{Backend: arkmem.New(), onGet: func() error { return nil }}
	repo := openRepo(t, b)
	snap, err := repo.Backup(ctx, stubSource{name: "victim", content: victimContent})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	b.onGet = func() error { return flaky }
	res, err := repo.Verify(ctx, snap.ID)
	if !errors.Is(err, flaky) {
		t.Errorf("Verify = %v, want %v", err, flaky)
	}
	if res != nil && len(res.CorruptChunks) != 0 {
		t.Errorf("Verify reported %q corrupt after a read failure", res.CorruptChunks)
	}
}

func TestCheckReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newTamperFixture(t)
	b := &blobGetBackend{Backend: f.b, onGet: func() error {
		cancel()
		return context.Canceled
	}}
	repo, err := ark.Open(context.Background(), b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	res, err := repo.Check(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Check = %+v, %v, want %v", res, err, context.Canceled)
	}
}
