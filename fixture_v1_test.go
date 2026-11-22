package ark_test

import (
	"bytes"
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/internal/v1fixture"
)

// The stored repository of format version 1 (scripts/v1fixture). A build that
// fails these tests can no longer read what version 1 wrote, or names other
// chunks or tree nodes for the same input; the fixture is never written again
// to make them pass. Pack, index and snapshot bytes are not compared.

// copyFixture copies the stored repository into memory, only the namespaces
// in types when any are given, so no test writes into testdata.
func copyFixture(t *testing.T, types ...ark.HandleType) *arkmem.Backend {
	t.Helper()
	ctx := context.Background()
	src, err := arklocal.New(v1fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if len(types) == 0 {
		for typ := ark.ConfigFile; typ <= ark.LostPackFile; typ++ {
			types = append(types, typ)
		}
	}
	dst := arkmem.New()
	for _, typ := range types {
		var names []string
		if err := src.List(ctx, typ, func(bi ark.BlobInfo) error {
			names = append(names, bi.Name)
			return nil
		}); err != nil {
			t.Fatalf("list %s: %v", typ, err)
		}
		for _, name := range names {
			h := ark.Handle{Type: typ, Name: name}
			if err := dst.Put(ctx, h, bytes.NewReader(readObject(t, src, typ, name))); err != nil {
				t.Fatalf("put %s/%s: %v", typ, name, err)
			}
		}
	}
	return dst
}

func openFixture(t *testing.T, b ark.Backend) *ark.Repository {
	t.Helper()
	repo, err := ark.Open(context.Background(), b, ark.NewPassword(v1fixture.Password))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// fixtureSnapshots returns the stored snapshots, oldest first.
func fixtureSnapshots(t *testing.T, repo *ark.Repository) []*ark.Snapshot {
	t.Helper()
	snaps, err := repo.Snapshots(context.Background(), ark.SnapshotFilter{})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(snaps, func(a, b *ark.Snapshot) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(snaps) != len(v1fixture.Snapshots()) {
		t.Fatalf("the fixture holds %d snapshots, want %d", len(snaps), len(v1fixture.Snapshots()))
	}
	return snaps
}

// entrySink keeps every entry a restore hands it, with its content.
type entrySink struct {
	meta    ark.SourceMeta
	entries []v1fixture.File
}

func (s *entrySink) Open(_ context.Context, meta ark.SourceMeta) error {
	s.meta = meta
	return nil
}

func (s *entrySink) Receive(_ context.Context, fi ark.FileInfo, r io.Reader) error {
	f := v1fixture.File{FileInfo: fi}
	if r != nil {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		f.Content = data
	}
	s.entries = append(s.entries, f)
	return nil
}

func (*entrySink) Close() error { return nil }

func TestFixtureV1RestoresWhatItsSourceYielded(t *testing.T) {
	ctx := context.Background()
	b := copyFixture(t)
	repo := openFixture(t, b)
	snaps := fixtureSnapshots(t, repo)

	for i, want := range v1fixture.Snapshots() {
		snap := snaps[i]
		if !snap.CreatedAt.Equal(want.Time) || snap.Host != v1fixture.Host ||
			snap.SourceName != "/srv/fixture" || snap.SourceType != ark.SourceTypeFile ||
			!slices.Equal(snap.Tags, want.Tags) || !maps.Equal(snap.Labels, v1fixture.Labels()) {
			t.Errorf("snapshot %d: %+v", i, snap)
		}

		sink := &entrySink{}
		res, err := repo.Restore(ctx, snap.ID, sink)
		if err != nil {
			t.Fatalf("restore snapshot %d: %v", i, err)
		}
		if len(res.Skipped) != 0 {
			t.Errorf("restore snapshot %d skipped %v", i, res.Skipped)
		}
		got := make(map[string]v1fixture.File, len(sink.entries))
		for _, f := range sink.entries {
			f.Path = strings.TrimSuffix(f.Path, "/") // a restore names a directory with a trailing slash
			got[f.Path] = f
		}
		sizes := make(map[string]int64)
		for _, w := range want.Files {
			sizes[w.Path] = w.Size
		}
		if len(got) != len(want.Files) {
			t.Errorf("snapshot %d restored %d entries, want %d", i, len(got), len(want.Files))
		}
		for _, w := range want.Files {
			g, ok := got[w.Path]
			if !ok {
				t.Errorf("snapshot %d: %q not restored", i, w.Path)
				continue
			}
			if !sameEntry(g, w, sizes) {
				t.Errorf("snapshot %d: %q restored as\n%+v\nwant\n%+v", i, w.Path, g.FileInfo, w.FileInfo)
			}
		}
	}
	if snaps[1].ParentID != snaps[0].ID {
		t.Errorf("second snapshot has parent %q, want %q", snaps[1].ParentID, snaps[0].ID)
	}

	formats := map[byte]int{}
	for _, name := range blobNames(t, repo, ark.BlobFile) {
		formats[readBlob(t, b, repo, ark.BlobFile, name)[0]]++
	}
	if formats[0x02] == 0 || formats[0x03] == 0 || len(formats) != 2 {
		t.Errorf("format bytes of the chunks: %v, want zstd (0x02) and raw (0x03) both", formats)
	}

	if res := mustCheck(t, repo); len(res.Errors)+len(res.Warnings) != 0 {
		t.Errorf("check: %+v", res)
	}
}

// sameEntry compares what a restore handed out with what the source yielded.
// A hardlink comes back with the size of its target.
func sameEntry(got, want v1fixture.File, size map[string]int64) bool {
	if want.Type == ark.FileTypeHardlink {
		want.Size = size[want.LinkDest]
	}
	return got.Path == want.Path && got.Type == want.Type && got.Mode == want.Mode &&
		got.UID == want.UID && got.GID == want.GID && got.Size == want.Size &&
		got.ModTime.Equal(want.ModTime) && got.ChangeTime.Equal(want.ChangeTime) &&
		got.Inode == want.Inode && got.LinkDest == want.LinkDest &&
		got.DevMajor == want.DevMajor && got.DevMinor == want.DevMinor &&
		maps.EqualFunc(got.Xattrs, want.Xattrs, bytes.Equal) && bytes.Equal(got.Content, want.Content)
}

func TestFixtureV1BackupStoresTheSameNames(t *testing.T) {
	ctx := context.Background()
	stored := openFixture(t, copyFixture(t))
	again := openFixture(t, copyFixture(t, ark.ConfigFile, ark.KeyFile))

	storedSnaps := fixtureSnapshots(t, stored)
	for i, s := range v1fixture.Snapshots() {
		snap, err := again.Backup(ctx, s.Source(), s.Options()...)
		if err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		want, err := ark.RootTree(ctx, stored, storedSnaps[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ark.RootTree(ctx, again, snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("snapshot %d: root tree %s, the fixture stores %s", i, got, want)
		}
	}
	for _, typ := range []ark.HandleType{ark.BlobFile, ark.TreeFile} {
		if got, want := blobNames(t, again, typ), blobNames(t, stored, typ); !slices.Equal(got, want) {
			t.Errorf("%s names:\n%v\nthe fixture stores\n%v", typ, got, want)
		}
	}
}
