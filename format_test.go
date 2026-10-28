package ark_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// formatSealed is the format byte of a sealed object.
const formatSealed = 0x02

// backupFixture initialises a repository in b and backs up a small tree of two
// directories, three files and a symlink into it. It returns the open
// repository and the snapshot id.
func backupFixture(t *testing.T, b ark.Backend) (*ark.Repository, string) {
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
		names := objectNames(t, b, typ)
		if len(names) == 0 {
			t.Fatalf("namespace %s is empty", typ)
		}
		for _, name := range names {
			obj := readObject(t, b, typ, name)
			if obj[0] != formatSealed {
				t.Errorf("%s/%s starts with 0x%02x, want 0x%02x", typ, name, obj[0], formatSealed)
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
}

// TestSameFilesInTwoRepositoriesShareNoNames is the point of the keyed names:
// nobody holding one repository's names can test whether a known file sits
// in another.
func TestSameFilesInTwoRepositoriesShareNoNames(t *testing.T) {
	one, two := arkmem.New(), arkmem.New()
	backupFixture(t, one)
	backupFixture(t, two)

	for _, typ := range []ark.HandleType{ark.BlobFile, ark.TreeFile} {
		names := objectNames(t, one, typ)
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

	if stored := readObject(t, b, ark.SnapshotFile, id); stored[0] != formatSealed {
		t.Errorf("manifest starts with 0x%02x, want 0x%02x", stored[0], formatSealed)
	}
	manifest := openObject(t, repo, ark.SnapshotFile, id)
	var m struct {
		RootTree string `json:"root_tree"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.RootTree == "" {
		t.Fatal("manifest has no root_tree")
	}

	if !slices.Contains(objectNames(t, b, ark.TreeFile), m.RootTree) {
		t.Errorf("root tree %s is not in the trees namespace", m.RootTree)
	}
	if slices.Contains(objectNames(t, b, ark.BlobFile), m.RootTree) {
		t.Errorf("root tree %s is still in the chunks namespace", m.RootTree)
	}
	// Two directories back up to two tree nodes, chunks stay out of them.
	if got := len(objectNames(t, b, ark.TreeFile)); got != 2 {
		t.Errorf("trees namespace holds %d objects, want 2", got)
	}
}

func TestTreeNodeJSONIsCanonical(t *testing.T) {
	b := arkmem.New()
	repo, _ := backupFixture(t, b)

	for _, name := range objectNames(t, b, ark.TreeFile) {
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
	chunk := objectNames(t, b, ark.BlobFile)[0]
	obj := readObject(t, b, ark.BlobFile, chunk)
	obj[0] = 0x01
	if putErr := b.Put(ctx, ark.Handle{Type: ark.BlobFile, Name: chunk}, bytes.NewReader(obj)); putErr != nil {
		t.Fatalf("rewrite chunk: %v", putErr)
	}

	_, err := repo.Restore(ctx, id, discardSink{})
	if err == nil {
		t.Fatal("restore of a chunk with an unknown format byte succeeded")
	}
	if !strings.Contains(err.Error(), "0x01") {
		t.Errorf("error does not name the format byte: %v", err)
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
