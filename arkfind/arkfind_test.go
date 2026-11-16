package arkfind_test

import (
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfind"
	"github.com/alexjoedt/ark/arkmem"
)

// treeSource yields its entries in order; a path ending in "/" is a directory.
type treeSource struct {
	name    string
	entries []entry
}

type entry struct {
	path    string
	content string
	mtime   time.Time
}

func (s treeSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: ark.SourceTypeFile}
}

func (s treeSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	for _, e := range s.entries {
		if dir, ok := strings.CutSuffix(e.path, "/"); ok {
			info := ark.FileInfo{Path: dir, Mode: 0o755, Type: ark.FileTypeDirectory, ModTime: e.mtime}
			if err := fn(info, nil); err != nil {
				return err
			}
			continue
		}
		info := ark.FileInfo{
			Path:    e.path,
			Size:    int64(len(e.content)),
			Mode:    0o644,
			Type:    ark.FileTypeRegular,
			ModTime: e.mtime,
		}
		content := e.content
		open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }
		if err := fn(info, open); err != nil {
			return err
		}
	}
	return nil
}

func newRepo(t *testing.T) (*ark.Repository, *arkmem.Backend) {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo, b
}

func backup(t *testing.T, repo *ark.Repository, src ark.Source, opts ...ark.BackupOption) *ark.Snapshot {
	t.Helper()
	snap, err := repo.Backup(context.Background(), src, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// find collects every match as "<snapshot id> <path>".
func find(t *testing.T, repo *ark.Repository, filter ark.SnapshotFilter, patterns ...string) []string {
	t.Helper()
	var got []string
	err := arkfind.Find(context.Background(), repo, filter, patterns, func(m arkfind.Match) error {
		got = append(got, m.Snapshot.ID+" "+m.File.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFindNewestFirstGroupedInTreeOrder(t *testing.T) {
	repo, _ := newRepo(t)
	mtime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	report := entry{path: "a/report.txt", content: "quarterly numbers", mtime: mtime}
	s1 := backup(
		t,
		repo,
		treeSource{name: "docs", entries: []entry{{path: "a/"}, report, {path: "b/"}, {path: "b/y.txt", content: "y"}}},
	)
	s2 := backup(t, repo, treeSource{name: "docs", entries: []entry{{path: "a/"}, {path: "a/z.log", content: "z"}}})
	s3 := backup(t, repo, treeSource{name: "docs", entries: []entry{
		{path: "a/"}, report, {path: "b/"}, {path: "b/y.txt", content: "y"}, {path: "b/w.txt", content: "w"},
	}})

	var got []arkfind.Match
	err := arkfind.Find(
		context.Background(),
		repo,
		ark.SnapshotFilter{},
		[]string{"report.txt"},
		func(m arkfind.Match) error {
			got = append(got, m)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Snapshot.ID != s3.ID || got[1].Snapshot.ID != s1.ID {
		t.Fatalf("matches = %v, want report.txt in %s then %s", got, s3.ID, s1.ID)
	}
	for _, m := range got {
		f := m.File
		if f.Path != "a/report.txt" || f.Size != int64(len(report.content)) || !f.ModTime.Equal(mtime) {
			t.Errorf("snapshot %s: file = %q size %d mtime %v, want a/report.txt size %d mtime %v",
				m.Snapshot.ID, f.Path, f.Size, f.ModTime, len(report.content), mtime)
		}
	}

	all := find(t, repo, ark.SnapshotFilter{}, "*.txt")
	want := []string{
		s3.ID + " a/report.txt", s3.ID + " b/w.txt", s3.ID + " b/y.txt",
		s1.ID + " a/report.txt", s1.ID + " b/y.txt",
	}
	if !slices.Equal(all, want) {
		t.Errorf("*.txt = %q, want %q (s2 %s has none)", all, want, s2.ID)
	}
}

func TestFindMatchRule(t *testing.T) {
	repo, _ := newRepo(t)
	backup(t, repo, treeSource{name: "src", entries: []entry{
		{path: "report.txt", content: "r"},
		{path: "docs/"},
		{path: "docs/report.txt", content: "r"},
		{path: "docs/old/"},
		{path: "docs/old/report.txt", content: "r"},
		{path: "docs/old/notes.md", content: "n"},
		{path: "src/"},
		{path: "src/main.go", content: "m"},
	}})

	tests := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{
			"base name anywhere",
			[]string{"report.txt"},
			[]string{"docs/old/report.txt", "docs/report.txt", "report.txt"},
		},
		{"glob on base name", []string{"*.md"}, []string{"docs/old/notes.md"}},
		{"unanchored path", []string{"old/notes.md"}, []string{"docs/old/notes.md"}},
		{"leading slash anchors at root", []string{"/report.txt"}, []string{"report.txt"}},
		{"anchored path", []string{"/docs/report.txt"}, []string{"docs/report.txt"}},
		{"anchored path not below root", []string{"/old"}, nil},
		{
			"directory yields descendants",
			[]string{"old"},
			[]string{"docs/old/", "docs/old/notes.md", "docs/old/report.txt"},
		},
		{"patterns are ORed", []string{"*.go", "notes.md"}, []string{"docs/old/notes.md", "src/main.go"}},
		{"no hit", []string{"missing"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, m := range find(t, repo, ark.SnapshotFilter{}, tt.patterns...) {
				_, p, _ := strings.Cut(m, " ")
				got = append(got, p)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Find(%q) = %q, want %q", tt.patterns, got, tt.want)
			}
		})
	}
}

func TestFindFilter(t *testing.T) {
	repo, _ := newRepo(t)
	files := []entry{{path: "f.txt", content: "f"}}
	home1 := backup(t, repo, treeSource{name: "home", entries: files}, ark.WithTags("daily"), ark.WithHost("laptop"))
	work := backup(t, repo, treeSource{name: "work", entries: files}, ark.WithTags("weekly"), ark.WithHost("laptop"))
	home2 := backup(
		t,
		repo,
		treeSource{name: "home", entries: files},
		ark.WithTags("daily", "weekly"),
		ark.WithHost("server"),
	)

	tests := []struct {
		name   string
		filter ark.SnapshotFilter
		want   []*ark.Snapshot
	}{
		{"none", ark.SnapshotFilter{}, []*ark.Snapshot{home2, work, home1}},
		{"name", ark.SnapshotFilter{Source: "home"}, []*ark.Snapshot{home2, home1}},
		{"tags", ark.SnapshotFilter{Tags: []string{"weekly"}}, []*ark.Snapshot{home2, work}},
		{"host", ark.SnapshotFilter{Host: "laptop"}, []*ark.Snapshot{work, home1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []string
			for _, s := range tt.want {
				want = append(want, s.ID+" f.txt")
			}
			if got := find(t, repo, tt.filter, "f.txt"); !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestFindBadPatternsTouchNoRepository(t *testing.T) {
	for _, patterns := range [][]string{nil, {}, {"["}, {"ok", ""}} {
		// A nil repository panics on first use, so an error proves nothing was read.
		err := arkfind.Find(context.Background(), nil, ark.SnapshotFilter{}, patterns, func(arkfind.Match) error {
			t.Fatal("fn called")
			return nil
		})
		if !errors.Is(err, path.ErrBadPattern) {
			t.Errorf("Find(%q) = %v, want path.ErrBadPattern", patterns, err)
		}
	}
}

func TestFindStopsOnCallbackError(t *testing.T) {
	repo, _ := newRepo(t)
	src := treeSource{name: "src", entries: []entry{{path: "a.txt", content: "a"}, {path: "b.txt", content: "b"}}}
	backup(t, repo, src)
	backup(t, repo, src)

	stop := errors.New("stop")
	calls := 0
	err := arkfind.Find(context.Background(), repo, ark.SnapshotFilter{}, []string{"*.txt"}, func(arkfind.Match) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("err = %v after %d calls, want stop after 1", err, calls)
	}
}

func TestFindReturnsCancellationUnwrapped(t *testing.T) {
	repo, _ := newRepo(t)
	src := treeSource{name: "src", entries: []entry{{path: "a.txt", content: "a"}}}
	backup(t, repo, src)
	backup(t, repo, src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := arkfind.Find(ctx, repo, ark.SnapshotFilter{}, []string{"a.txt"}, func(arkfind.Match) error {
		calls++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() || calls != 1 {
		t.Errorf("err = %v after %d calls, want bare context.Canceled after 1", err, calls)
	}
}

func TestFindSkipsSnapshotForgottenDuringSearch(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	src := treeSource{name: "src", entries: []entry{{path: "a.txt", content: "a"}}}
	s1 := backup(t, repo, src)
	s2 := backup(t, repo, src)
	s3 := backup(t, repo, src)

	var got []string
	err := arkfind.Find(ctx, repo, ark.SnapshotFilter{}, []string{"a.txt"}, func(m arkfind.Match) error {
		got = append(got, m.Snapshot.ID)
		if m.Snapshot.ID != s3.ID {
			return nil
		}
		_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{s2.ID}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{s3.ID, s1.ID}; !slices.Equal(got, want) {
		t.Errorf("searched %q, want %q", got, want)
	}
}

func TestFindNamesSnapshotOnWalkError(t *testing.T) {
	repo, b := newRepo(t)
	ctx := context.Background()
	snap := backup(t, repo, treeSource{name: "src", entries: []entry{{path: "a.txt", content: "a"}}})

	var packs []string
	if err := b.List(ctx, ark.TreePackFile, func(bi ark.BlobInfo) error {
		packs = append(packs, bi.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range packs {
		if err := b.Delete(ctx, ark.Handle{Type: ark.TreePackFile, Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	err := arkfind.Find(ctx, repo, ark.SnapshotFilter{}, []string{"a.txt"}, func(arkfind.Match) error { return nil })
	if err == nil || !strings.Contains(err.Error(), snap.ID) {
		t.Errorf("err = %v, want an error naming snapshot %s", err, snap.ID)
	}
}
