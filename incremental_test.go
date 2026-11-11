package ark_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// statFile is one file of a statSource: content plus the metadata a backup
// compares against the parent snapshot.
type statFile struct {
	content string
	mtime   time.Time
	ctime   time.Time
	inode   uint64
}

// statSource yields its files in path order and counts every open. walking,
// when set, is closed after the first file and the walk then waits for
// resume before the rest.
type statSource struct {
	name    string
	files   map[string]statFile
	opens   map[string]int
	walking chan struct{}
	resume  chan struct{}
}

func newStatSource(name string, files map[string]statFile) *statSource {
	return &statSource{name: name, files: files, opens: map[string]int{}}
}

func (s *statSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: ark.SourceTypeFile}
}

func (s *statSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	for i, p := range slices.Sorted(maps.Keys(s.files)) {
		f := s.files[p]
		info := ark.FileInfo{
			Path:       p,
			Size:       int64(len(f.content)),
			Mode:       0o644,
			ModTime:    f.mtime,
			ChangeTime: f.ctime,
			Inode:      f.inode,
			Type:       ark.FileTypeRegular,
		}
		err := fn(info, func() (io.ReadCloser, error) {
			s.opens[p]++
			return io.NopCloser(strings.NewReader(f.content)), nil
		})
		if err != nil {
			return err
		}
		if i == 0 && s.walking != nil {
			close(s.walking)
			<-s.resume
		}
	}
	return nil
}

func (s *statSource) opened() []string {
	return slices.Sorted(maps.Keys(s.opens))
}

func baseFiles() map[string]statFile {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	return map[string]statFile{
		"a.txt": {content: "alpha", mtime: t0, ctime: t0, inode: 1},
		"b.txt": {content: "bravo", mtime: t0, ctime: t0, inode: 2},
		"c.bin": {content: payload(3), mtime: t0, ctime: t0, inode: 3},
		"d.txt": {content: "delta", mtime: t0},
	}
}

// restoreAll restores snapshot id into a recording sink and returns the bytes
// it received per path.
func restoreAll(t *testing.T, repo *ark.Repository, id string) map[string]string {
	t.Helper()
	sink := &recordingSink{}
	if _, err := repo.Restore(context.Background(), id, sink); err != nil {
		t.Fatalf("Restore %s: %v", id, err)
	}
	return sink.written
}

func checkRestore(t *testing.T, repo *ark.Repository, id string, files map[string]statFile) {
	t.Helper()
	got := restoreAll(t, repo, id)
	for p, f := range files {
		if got[p] != f.content {
			t.Errorf("%s restored %d bytes that differ from the source's %d", p, len(got[p]), len(f.content))
		}
	}
}

func TestBackupReusesUnchangedFilesWithoutOpening(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	first := newStatSource("app", baseFiles())
	parent, err := repo.Backup(ctx, first)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if len(first.opened()) != len(first.files) {
		t.Fatalf("first backup opened %v, want every file", first.opened())
	}

	src := newStatSource("app", baseFiles())
	child, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if len(src.opens) != 0 {
		t.Errorf("unchanged files were opened: %v", src.opened())
	}
	st := child.Stats
	if st.FilesUnchanged != len(src.files) || st.ChunksNew != 0 || st.ChunksReused != parent.Stats.ChunksTotal {
		t.Errorf("stats: unchanged %d, new %d, reused %d; want %d, 0, %d",
			st.FilesUnchanged, st.ChunksNew, st.ChunksReused, len(src.files), parent.Stats.ChunksTotal)
	}
	if st.TotalSize != parent.Stats.TotalSize || st.FileCount != parent.Stats.FileCount {
		t.Errorf("size %d files %d, want the parent's %d and %d",
			st.TotalSize, st.FileCount, parent.Stats.TotalSize, parent.Stats.FileCount)
	}
	checkRestore(t, repo, child.ID, src.files)

	// Under another name there is no parent, so every file is read again.
	reread := newStatSource("copy", baseFiles())
	full, err := repo.Backup(ctx, reread)
	if err != nil {
		t.Fatalf("backup without parent: %v", err)
	}
	if len(reread.opens) != len(reread.files) {
		t.Fatalf("backup without parent opened %v, want every file", reread.opened())
	}
	skipped, err := ark.RootTree(ctx, repo, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ark.RootTree(ctx, repo, full.ID)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != read {
		t.Errorf("root tree with skipping %s, without %s", skipped, read)
	}
}

func TestBackupRereadsChangedFiles(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	if _, err := repo.Backup(ctx, newStatSource("app", baseFiles())); err != nil {
		t.Fatalf("first backup: %v", err)
	}

	files := baseFiles()
	later := files["a.txt"].mtime.Add(time.Second)
	grown := files["a.txt"]
	grown.content = "alpha, longer"
	files["a.txt"] = grown
	touched := files["b.txt"]
	touched.content, touched.mtime = "BRAVO", later // same size
	files["b.txt"] = touched
	rewritten := files["c.bin"]
	rewritten.content = strings.ToUpper(rewritten.content[:1]) + rewritten.content[1:]
	rewritten.ctime = later // mtime kept, only ctime tells
	files["c.bin"] = rewritten

	src := newStatSource("app", files)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if got, want := src.opened(), []string{"a.txt", "b.txt", "c.bin"}; !slices.Equal(got, want) {
		t.Errorf("opened %v, want %v", got, want)
	}
	if snap.Stats.FilesUnchanged != 1 {
		t.Errorf("FilesUnchanged = %d, want 1 (d.txt)", snap.Stats.FilesUnchanged)
	}
	checkRestore(t, repo, snap.ID, files)
}

func TestBackupComparesChangeTimeOnlyWhenBothSidesHaveIt(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	if _, err := repo.Backup(ctx, newStatSource("app", baseFiles())); err != nil {
		t.Fatalf("first backup: %v", err)
	}

	// A tar-like source reports no ctime or inode: Size and ModTime decide.
	files := baseFiles()
	for p, f := range files {
		f.ctime, f.inode = time.Time{}, 0
		files[p] = f
	}
	// d.txt had none in the parent, and now gains both.
	d := files["d.txt"]
	d.ctime, d.inode = d.mtime.Add(time.Hour), 9
	files["d.txt"] = d

	src := newStatSource("app", files)
	if _, err := repo.Backup(ctx, src); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if len(src.opens) != 0 {
		t.Errorf("opened %v, want none", src.opened())
	}

	// A zero mtime is no evidence at all.
	noTime := map[string]statFile{"x": {content: "one"}}
	if _, err := repo.Backup(ctx, newStatSource("bare", noTime)); err != nil {
		t.Fatal(err)
	}
	again := newStatSource("bare", map[string]statFile{"x": {content: "two"}})
	snap, err := repo.Backup(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if again.opens["x"] != 1 {
		t.Error("a file without mtime was taken from the parent")
	}
	if got := restoreAll(t, repo, snap.ID)["x"]; got != "two" {
		t.Errorf("restored %q, want %q", got, "two")
	}
}

// A file whose mtime falls in the tick the parent read it in may have been
// rewritten after that read without a visible stat change, so it is read
// again; a parent of another source lends no files at all.
func TestBackupRereadsRacyAndForeignParentFiles(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	// Whole seconds and no ctime or inode, as a tar header reports them.
	now := time.Now().Truncate(time.Second)
	if _, err := repo.Backup(ctx, newStatSource("vol", map[string]statFile{
		"db": {content: "page-1", mtime: now},
	})); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	files := map[string]statFile{"db": {content: "page-2", mtime: now}}
	src := newStatSource("vol", files)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if src.opens["db"] != 1 {
		t.Error("a file modified in the parent's tick was taken from the parent")
	}
	checkRestore(t, repo, snap.ID, files)

	other, err := repo.Backup(ctx, newStatSource("other", baseFiles()))
	if err != nil {
		t.Fatalf("backup of the other source: %v", err)
	}
	foreign := newStatSource("app", baseFiles())
	if _, err := repo.Backup(ctx, foreign, ark.WithParent(other.ID)); err != nil {
		t.Fatalf("backup with a foreign parent: %v", err)
	}
	if len(foreign.opens) != len(foreign.files) {
		t.Errorf("with a foreign parent opened %v, want every file", foreign.opened())
	}
}

// A forget and prune of the parent while the child still reuses its chunks:
// forget only removes the manifest, and prune needs the exclusive lock the
// running backup's shared lock blocks.
func TestForgetAndPruneOfParentDuringChildBackup(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	parent, err := repo.Backup(ctx, newStatSource("app", baseFiles()))
	if err != nil {
		t.Fatalf("parent backup: %v", err)
	}

	src := newStatSource("app", baseFiles())
	src.walking, src.resume = make(chan struct{}), make(chan struct{})
	var (
		wg    sync.WaitGroup
		child *ark.Snapshot
		cerr  error
	)
	wg.Go(func() { child, cerr = repo.Backup(ctx, src) })
	<-src.walking

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{parent.ID}}); err != nil {
		t.Fatalf("Forget of the parent: %v", err)
	}
	if _, err := repo.Prune(ctx); !errors.Is(err, ark.ErrRepoLocked) {
		t.Fatalf("Prune during the backup: %v, want ErrRepoLocked", err)
	}
	close(src.resume)
	wg.Wait()
	if cerr != nil {
		t.Fatalf("child backup: %v", cerr)
	}
	if child.Stats.FilesUnchanged != len(src.files) {
		t.Errorf("FilesUnchanged = %d, want %d", child.Stats.FilesUnchanged, len(src.files))
	}

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune after the backup: %v", err)
	}
	checkRestore(t, repo, child.ID, src.files)
	res, err := repo.Check(ctx)
	if err != nil || len(res.Errors) != 0 {
		t.Errorf("Check: %v %v", err, res)
	}
}

// putCountingBackend counts the Puts per object type.
type putCountingBackend struct {
	ark.Backend

	mu   sync.Mutex
	puts map[ark.HandleType]int
}

func (b *putCountingBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	b.mu.Lock()
	b.puts[h.Type]++
	b.mu.Unlock()
	return b.Backend.Put(ctx, h, r)
}

func rootTree(t *testing.T, repo *ark.Repository, id string) string {
	t.Helper()
	root, err := ark.RootTree(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("RootTree %s: %v", id, err)
	}
	return root
}

// take returns the counts so far and starts over.
func (b *putCountingBackend) take() map[ark.HandleType]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	puts := b.puts
	b.puts = map[ark.HandleType]int{}
	return puts
}

func TestBackupStoresOnlyChangedTreeNodes(t *testing.T) {
	ctx := context.Background()
	b := &putCountingBackend{Backend: arkmem.New(), puts: map[ark.HandleType]int{}}
	repo := openRepo(t, b)

	t0 := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	files := map[string]statFile{
		"top.txt":          {content: "top", mtime: t0, ctime: t0, inode: 1},
		"a/one.txt":        {content: "one", mtime: t0, ctime: t0, inode: 2},
		"a/deep/two.txt":   {content: "two", mtime: t0, ctime: t0, inode: 3},
		"b/three.txt":      {content: "three", mtime: t0, ctime: t0, inode: 4},
		"b/other/four.txt": {content: payload(4), mtime: t0, ctime: t0, inode: 5},
		"b/other/five.txt": {content: payload(5), mtime: t0, ctime: t0, inode: 6},
	}
	parent, err := repo.Backup(ctx, newStatSource("app", files))
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if got := b.take(); got[ark.TreeFile] != 5 {
		t.Fatalf("first backup stored %d tree nodes, want 5 (%v)", got[ark.TreeFile], got)
	}

	// Nothing changed: the nodes are in the repository under the same names.
	same, err := repo.Backup(ctx, newStatSource("app", maps.Clone(files)))
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	got := b.take()
	if got[ark.TreeFile] != 0 || got[ark.BlobFile] != 0 || got[ark.SnapshotFile] != 1 {
		t.Errorf("unchanged backup stored %v, want only the manifest", got)
	}
	if sameRoot, parentRoot := rootTree(t, repo, same.ID), rootTree(t, repo, parent.ID); sameRoot != parentRoot {
		t.Errorf("root tree %s differs from the parent's %s", sameRoot, parentRoot)
	}

	// One file changed: its directory, the parents up to the root and the
	// root itself are rewritten, the other subtrees are not.
	t1 := t0.Add(time.Hour)
	files["a/deep/two.txt"] = statFile{content: "two!", mtime: t1, ctime: t1, inode: 3}
	child, err := repo.Backup(ctx, newStatSource("app", files))
	if err != nil {
		t.Fatalf("third backup: %v", err)
	}
	got = b.take()
	if got[ark.TreeFile] != 3 || got[ark.BlobFile] != 1 {
		t.Errorf("changed backup stored %v, want 3 tree nodes and 1 blob", got)
	}
	st := child.Stats
	if st.ChunksNew != 1 || st.ChunksReused != parent.Stats.ChunksTotal-1 || st.StoredSize == 0 ||
		st.NewChunksRawSize != 4 {
		t.Errorf("stats: new %d, reused %d, stored %d, raw %d; want 1, %d, >0, 4",
			st.ChunksNew, st.ChunksReused, st.StoredSize, st.NewChunksRawSize, parent.Stats.ChunksTotal-1)
	}
	checkRestore(t, repo, child.ID, files)
}

type excludingSource struct {
	*statSource
	excludes []string
}

func (s excludingSource) Meta() ark.SourceMeta {
	m := s.statSource.Meta()
	m.Excludes = s.excludes
	return m
}

func TestSnapshotCarriesTheSourceExcludes(t *testing.T) {
	ctx := context.Background()
	src := excludingSource{
		statSource: newStatSource("app", map[string]statFile{"a.txt": {content: "a"}}),
		excludes:   []string{"*.tmp", "/cache"},
	}
	repo := openRepo(t, arkmem.New())
	snap, err := repo.Backup(ctx, src, ark.WithTags("t"))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	want := []string{"*.tmp", "/cache"}
	if !slices.Equal(snap.Excludes, want) {
		t.Fatalf("Backup returned excludes %q, want %q", snap.Excludes, want)
	}
	snap.Excludes[0] = "changed"
	snap.Tags[0] = "changed"

	got, err := repo.Snapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !slices.Equal(got.Excludes, want) || !slices.Equal(got.Tags, []string{"t"}) {
		t.Fatalf("Snapshot excludes %q tags %q, want %q and [t]", got.Excludes, got.Tags, want)
	}
	got.Excludes[0] = "changed"
	all, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(all) != 1 || !slices.Equal(all[0].Excludes, want) {
		t.Fatalf("Snapshots %v, want one with excludes %q", all, want)
	}

	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !slices.Equal(sink.meta.Excludes, want) {
		t.Fatalf("sink opened with excludes %q, want %q", sink.meta.Excludes, want)
	}

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}
	imported, err := openRepo(t, arkmem.New()).Import(ctx, &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !slices.Equal(imported.Excludes, want) {
		t.Fatalf("imported excludes %q, want %q", imported.Excludes, want)
	}
}

func TestSnapshotWithoutExcludesHasNone(t *testing.T) {
	ctx := context.Background()
	for name, excludes := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			repo := openRepo(t, arkmem.New())
			src := excludingSource{
				statSource: newStatSource("app", map[string]statFile{"a": {content: "a"}}),
				excludes:   excludes,
			}
			snap, err := repo.Backup(ctx, src)
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if snap.Excludes != nil {
				t.Fatalf("Backup excludes %q, want nil", snap.Excludes)
			}
			got, err := repo.Snapshot(ctx, snap.ID)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if got.Excludes != nil {
				t.Fatalf("Snapshot excludes %q, want nil", got.Excludes)
			}
		})
	}
}
