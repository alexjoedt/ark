package ark_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

// The format bytes of a sealed object: a zstd frame, or the content as it is.
const (
	formatZstd = 0x02
	formatRaw  = 0x03
)

// sealedFormat reports whether b is a format byte of a sealed object.
func sealedFormat(b byte) bool { return b == formatZstd || b == formatRaw }

// backupFixture initialises a repository in b and backs up a small tree of two
// directories, three files and a symlink into it. It returns the open
// repository and the snapshot id.
func backupFixture(t *testing.T, b ark.Backend) (*ark.Repository, ark.SnapshotID) {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range map[string]string{
		"zeta.txt":    "zeta contents",
		"alpha.txt":   "alpha contents",
		"sub/mid.txt": strings.Repeat("mid ", 4096),
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Symlink("alpha.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return repo, snap.ID
}

// readObject returns the stored bytes of one object, header byte included.
func readObject(t *testing.T, b ark.Backend, typ ark.HandleType, name string) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), ark.Handle{Type: typ, Name: name})
	if err != nil {
		t.Fatalf("get %s/%s: %v", typ, name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s/%s: %v", typ, name, err)
	}
	return data
}

// objectNames lists every object name in a namespace.
func objectNames(t *testing.T, b ark.Backend, typ ark.HandleType) []string {
	t.Helper()
	var names []string
	err := b.List(context.Background(), typ, func(info ark.BlobInfo) error {
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("list %s: %v", typ, err)
	}
	return names
}

// blobNames lists every chunk (BlobFile) or tree node (TreeFile) the index of
// repo lists. They are stored inside packs, so no listing of the backend
// names them.
func blobNames(t *testing.T, repo *ark.Repository, typ ark.HandleType) []string {
	t.Helper()
	names, err := ark.BlobNames(context.Background(), repo, typ)
	if err != nil {
		t.Fatalf("blob names of %s: %v", typ, err)
	}
	return names
}

// readBlob returns the sealed bytes of a chunk or tree node as its pack
// stores them.
func readBlob(t *testing.T, b ark.Backend, repo *ark.Repository, typ ark.HandleType, name string) []byte {
	t.Helper()
	ctx := context.Background()
	pack, offset, length, err := ark.BlobPlace(ctx, repo, typ, name)
	if err != nil {
		t.Fatalf("place of %s/%s: %v", typ, name, err)
	}
	obj, err := b.GetRange(ctx, pack, offset, length)
	if err != nil {
		t.Fatalf("read %s/%s from %s/%s: %v", typ, name, pack.Type, pack.Name, err)
	}
	return obj
}

// writeBlob replaces the sealed bytes of a chunk or tree node inside its
// pack by obj, which has their length. The pack keeps its name, which no read
// checks against its content.
func writeBlob(t *testing.T, b ark.Backend, repo *ark.Repository, typ ark.HandleType, name string, obj []byte) {
	t.Helper()
	ctx := context.Background()
	pack, offset, length, err := ark.BlobPlace(ctx, repo, typ, name)
	if err != nil {
		t.Fatalf("place of %s/%s: %v", typ, name, err)
	}
	if int64(len(obj)) != length {
		t.Fatalf("replacement of %s/%s has %d bytes, the blob %d", typ, name, len(obj), length)
	}
	whole := readObject(t, b, pack.Type, pack.Name)
	copy(whole[offset:], obj)
	if err := b.Put(ctx, pack, bytes.NewReader(whole)); err != nil {
		t.Fatalf("put %s/%s: %v", pack.Type, pack.Name, err)
	}
}

// deletePackOf deletes the pack that holds a chunk or tree node, which is how
// one goes missing.
func deletePackOf(t *testing.T, b ark.Backend, repo *ark.Repository, typ ark.HandleType, name string) {
	t.Helper()
	ctx := context.Background()
	pack, _, _, err := ark.BlobPlace(ctx, repo, typ, name)
	if err != nil {
		t.Fatalf("place of %s/%s: %v", typ, name, err)
	}
	if err := b.Delete(ctx, pack); err != nil {
		t.Fatalf("delete %s/%s: %v", pack.Type, pack.Name, err)
	}
}

// openObject returns the raw content of an object of repo.
func openObject(t *testing.T, repo *ark.Repository, typ ark.HandleType, name string) []byte {
	t.Helper()
	data, err := ark.OpenObject(context.Background(), repo, typ, name)
	if err != nil {
		t.Fatalf("open %s/%s: %v", typ, name, err)
	}
	return data
}

func TestChunksAndTreesAreSealedUnderKeyedNames(t *testing.T) {
	b := arkmem.New()
	repo, _ := backupFixture(t, b)

	for _, typ := range []ark.HandleType{ark.BlobFile, ark.TreeFile} {
		names := blobNames(t, repo, typ)
		if len(names) == 0 {
			t.Fatalf("namespace %s is empty", typ)
		}
		for _, name := range names {
			obj := readBlob(t, b, repo, typ, name)
			if !sealedFormat(obj[0]) {
				t.Errorf("%s/%s starts with 0x%02x, want a sealed format byte", typ, name, obj[0])
			}
			// The name is the keyed hash of the raw content, never its sha256.
			data := openObject(t, repo, typ, name)
			if got := ark.ObjectName(repo, data); got != name {
				t.Errorf("%s object is named %s, want the HMAC of its raw content %s", typ, name, got)
			}
			sum := sha256.Sum256(data)
			if name == hex.EncodeToString(sum[:]) {
				t.Errorf("%s object %s is named by the plain sha256 of its content", typ, name)
			}
		}
	}
	for _, name := range objectNames(t, b, ark.SnapshotFile) {
		sum := sha256.Sum256(openObject(t, repo, ark.SnapshotFile, name))
		if name == hex.EncodeToString(sum[:]) {
			t.Errorf("snapshot %s is named by the plain sha256 of its content", name)
		}
	}
}

// TestSameFilesInTwoRepositoriesShareNoNames is the point of the keyed names:
// nobody holding one repository's names can test whether a known file sits
// in another.
// The config is the one fixed name, the same in every repository. Each
// repository holds a lost-pack marker too, stored by hand.
func TestSameFilesInTwoRepositoriesShareNoNames(t *testing.T) {
	one, two := arkmem.New(), arkmem.New()
	for _, b := range []ark.Backend{one, two} {
		repo, _ := backupFixture(t, b)
		if err := ark.PutObject(context.Background(), repo, ark.LostPackMarker(repo), nil); err != nil {
			t.Fatalf("put marker: %v", err)
		}
	}

	for _, typ := range allHandleTypes {
		if typ == ark.ConfigFile {
			continue
		}
		names := objectNames(t, one, typ)
		// A finished backup leaves no lock, and no chunk or tree node is an
		// object of its own: they are inside the packs.
		leftEmpty := []ark.HandleType{ark.LockFile, ark.BlobFile, ark.TreeFile}
		if (len(names) == 0) != slices.Contains(leftEmpty, typ) {
			t.Errorf("namespace %s holds %d objects", typ, len(names))
		}
		for _, name := range objectNames(t, two, typ) {
			if slices.Contains(names, name) {
				t.Errorf("%s object %s is in both repositories", typ, name)
			}
		}
	}
}

func TestTreeNodesLiveInTheTreesNamespace(t *testing.T) {
	b := arkmem.New()
	repo, id := backupFixture(t, b)

	if stored := readObject(t, b, ark.SnapshotFile, string(id)); !sealedFormat(stored[0]) {
		t.Errorf("manifest starts with 0x%02x, want a sealed format byte", stored[0])
	}
	manifest := openObject(t, repo, ark.SnapshotFile, string(id))
	var m struct {
		RootTree string `json:"root_tree"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.RootTree == "" {
		t.Fatal("manifest has no root_tree")
	}

	if !slices.Contains(blobNames(t, repo, ark.TreeFile), m.RootTree) {
		t.Errorf("root tree %s is not in the tree index", m.RootTree)
	}
	if slices.Contains(blobNames(t, repo, ark.BlobFile), m.RootTree) {
		t.Errorf("root tree %s is in the data index", m.RootTree)
	}
	// Two directories back up to two tree nodes, chunks stay out of them.
	if got := len(blobNames(t, repo, ark.TreeFile)); got != 2 {
		t.Errorf("the tree index lists %d nodes, want 2", got)
	}
}

func TestTreeNodeJSONIsCanonical(t *testing.T) {
	b := arkmem.New()
	repo, _ := backupFixture(t, b)

	for _, name := range blobNames(t, repo, ark.TreeFile) {
		data := openObject(t, repo, ark.TreeFile, name)
		if bytes.ContainsAny(data, "\n\t") {
			t.Errorf("tree %s is indented, want canonical JSON: %s", name, data)
		}
		var node struct {
			Entries []struct {
				Name string `json:"name"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(data, &node); err != nil {
			t.Fatalf("parse tree %s: %v", name, err)
		}
		names := make([]string, 0, len(node.Entries))
		for _, e := range node.Entries {
			names = append(names, e.Name)
		}
		if !slices.IsSorted(names) {
			t.Errorf("tree %s entries are not sorted by name: %v", name, names)
		}
	}
}

func TestUnknownFormatByteIsRejected(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo, id := backupFixture(t, b)

	// Rewrite one chunk with the plaintext format byte of the pre-encryption
	// draft, which the encrypted format no longer reads.
	chunk := blobNames(t, repo, ark.BlobFile)[0]
	obj := readBlob(t, b, repo, ark.BlobFile, chunk)
	obj[0] = 0x01
	writeBlob(t, b, repo, ark.BlobFile, chunk, obj)

	_, err := repo.Restore(ctx, id, discardSink{})
	if err == nil {
		t.Fatal("restore of a chunk with an unknown format byte succeeded")
	}
	if !strings.Contains(err.Error(), "0x01") {
		t.Errorf("error does not name the format byte: %v", err)
	}
}

// TestWriterStoresABlobRawWhenZstdIsNotSmaller backs up random and
// compressible files. Every chunk of the random ones is stored raw, its
// content plus the seal overhead, and none of the compressible ones is. A
// format byte changed to the other codec's does not open either way: the
// byte is part of what the tag covers, or a zstd frame relabelled raw would
// come back as the content.
func TestWriterStoresABlobRawWhenZstdIsNotSmaller(t *testing.T) {
	ctx := context.Background()
	random := make([]byte, 300<<10)
	_, _ = rand.Read(random)
	for name, tc := range map[string]struct {
		content []byte
		format  byte
	}{
		"random":       {random, formatRaw},
		"compressible": {bytes.Repeat([]byte("the quick brown fox "), 15<<10), formatZstd},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			b := arkmem.New()
			repo := openRepo(t, b)
			snap := backupDir(t, repo, root)

			chunks := blobNames(t, repo, ark.BlobFile)
			if len(chunks) == 0 {
				t.Fatal("the backup stored no chunk")
			}
			for _, chunk := range chunks {
				obj := readBlob(t, b, repo, ark.BlobFile, chunk)
				plain := len(openObject(t, repo, ark.BlobFile, chunk))
				if obj[0] != tc.format {
					t.Fatalf("chunk %s starts with 0x%02x, want 0x%02x", chunk, obj[0], tc.format)
				}
				if raw := plain + ark.SealOverhead; (len(obj) == raw) != (tc.format == formatRaw) || len(obj) > raw {
					t.Errorf("chunk of %d bytes sealed at %d under 0x%02x, raw is %d", plain, len(obj), obj[0], raw)
				}

				h := ark.Handle{Type: ark.BlobFile, Name: chunk}
				if _, err := ark.UnsealObject(repo, h, obj); err != nil {
					t.Fatalf("chunk %s as stored: %v", chunk, err)
				}
				obj[0] ^= formatZstd ^ formatRaw
				if data, err := ark.UnsealObject(repo, h, obj); err == nil {
					t.Errorf("chunk %s opened to %d bytes under the other format byte 0x%02x", chunk, len(data), obj[0])
				}
			}

			writeBlob(t, b, repo, ark.BlobFile, chunks[0], func() []byte {
				obj := readBlob(t, b, repo, ark.BlobFile, chunks[0])
				obj[0] ^= formatZstd ^ formatRaw
				return obj
			}())
			if _, err := repo.Restore(ctx, snap.ID, discardSink{}); !errors.Is(err, ark.ErrCorruptChunk) {
				t.Errorf("Restore with a changed format byte = %v, want ErrCorruptChunk", err)
			}
		})
	}
}

// discardSink accepts every entry and throws the bytes away.
type discardSink struct{}

func (discardSink) Open(context.Context, ark.SourceMeta) error { return nil }

func (discardSink) Receive(_ context.Context, _ ark.FileInfo, r io.Reader) error {
	if r == nil {
		return nil
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (discardSink) Close() error { return nil }
