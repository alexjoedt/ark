package ark_test

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arkmem"
)

// stubSource yields one regular file under a fixed source name. typ defaults
// to ark.SourceTypeFile.
type stubSource struct {
	name    string
	content string
	typ     ark.SourceType
	modTime time.Time
}

func (s stubSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: cmp.Or(s.typ, ark.SourceTypeFile)}
}

func (s stubSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	info := ark.FileInfo{
		Path: "data.txt",
		Size: int64(len(s.content)),
		Mode: 0o644,
		Type: ark.FileTypeRegular,
	}
	info.ModTime = s.modTime
	return fn(info, openReader(strings.NewReader(s.content)))
}

// openReader wraps r as the open func a Source hands out for a regular file.
func openReader(r io.Reader) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(r), nil }
}

// openRepo initialises a repository in b and opens it.
func openRepo(t *testing.T, b ark.Backend) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestBackupParentIsTheNewestSnapshotOfTheSource(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	first, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if first.ParentID != "" {
		t.Errorf("first snapshot of a source has parent %q, want none", first.ParentID)
	}

	// A snapshot of another source in between must not become the parent.
	other, err := repo.Backup(ctx, stubSource{name: "database", content: "other"})
	if err != nil {
		t.Fatalf("other backup: %v", err)
	}

	second, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if second.ParentID != first.ID {
		t.Errorf("parent is %q, want the previous snapshot of the same source %q", second.ParentID, first.ID)
	}
	if second.ParentID == other.ID {
		t.Error("parent is the newest snapshot of a different source")
	}
}

// Two machines backing up one source name into a shared repository must not
// take each other's files unread: a file can match by path, size and mtime on
// both and still differ.
func TestBackupParentNeverCrossesHosts(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	a, err := repo.Backup(ctx, stubSource{name: "docs", content: "one"}, ark.WithHost("a"))
	if err != nil {
		t.Fatalf("backup on a: %v", err)
	}
	b, err := repo.Backup(ctx, stubSource{name: "docs", content: "two"}, ark.WithHost("b"))
	if err != nil {
		t.Fatalf("backup on b: %v", err)
	}
	if b.ParentID != "" {
		t.Errorf("host b took parent %q from host a", b.ParentID)
	}
	none, err := repo.Backup(ctx, stubSource{name: "docs", content: "three"})
	if err != nil {
		t.Fatalf("backup without a host: %v", err)
	}
	if none.ParentID != "" {
		t.Errorf("backup without a host took parent %q", none.ParentID)
	}
	again, err := repo.Backup(ctx, stubSource{name: "docs", content: "four"}, ark.WithHost("a"))
	if err != nil {
		t.Fatalf("second backup on a: %v", err)
	}
	if again.ParentID != a.ID {
		t.Errorf("parent is %q, want host a's snapshot %q", again.ParentID, a.ID)
	}
}

// A file source and a docker volume can share a name and a host; they are two
// series, and a tar entry must never be taken unread from a file's snapshot.
func TestBackupParentNeverCrossesSourceTypes(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	host := ark.WithHost("h")

	file, err := repo.Backup(ctx, stubSource{name: "db", content: "one"}, host)
	if err != nil {
		t.Fatalf("file backup: %v", err)
	}
	volume, err := repo.Backup(ctx, stubSource{name: "db", content: "two", typ: "docker-volume"}, host)
	if err != nil {
		t.Fatalf("volume backup: %v", err)
	}
	if volume.ParentID != "" {
		t.Errorf("the volume took parent %q from the file source", volume.ParentID)
	}
	again, err := repo.Backup(ctx, stubSource{name: "db", content: "three"}, host)
	if err != nil {
		t.Fatalf("second file backup: %v", err)
	}
	if again.ParentID != file.ID {
		t.Errorf("parent is %q, want the file source's snapshot %q", again.ParentID, file.ID)
	}
}

// TestSourceNamesAreComparedVerbatim guards the removal of normalizeSourceName:
// it replaced path separators with underscores, so these two names shared one
// latest pointer.
func TestSourceNamesAreComparedVerbatim(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	slashed, err := repo.Backup(ctx, stubSource{name: "/srv/app", content: "one"})
	if err != nil {
		t.Fatalf("backup /srv/app: %v", err)
	}
	underscored, err := repo.Backup(ctx, stubSource{name: "_srv_app", content: "two"})
	if err != nil {
		t.Fatalf("backup _srv_app: %v", err)
	}
	if underscored.ParentID != "" {
		t.Errorf("_srv_app inherited parent %q from a differently named source", underscored.ParentID)
	}

	again, err := repo.Backup(ctx, stubSource{name: "/srv/app", content: "three"})
	if err != nil {
		t.Fatalf("second backup /srv/app: %v", err)
	}
	if again.ParentID != slashed.ID {
		t.Errorf("parent is %q, want %q", again.ParentID, slashed.ID)
	}
}

func TestBackendHoldsNoLatestNamespace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b, err := arklocal.New(dir)
	if err != nil {
		t.Fatalf("arklocal.New: %v", err)
	}
	repo := openRepo(t, b)
	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read repository dir: %v", err)
	}
	var namespaces []string
	for _, e := range entries {
		namespaces = append(namespaces, e.Name())
	}
	if slices.Contains(namespaces, "latest") {
		t.Errorf("repository still has a latest namespace: %v", namespaces)
	}
	for _, want := range []string{"config", "datapacks", "treepacks", "dataindex", "treeindex", "snapshots"} {
		if !slices.Contains(namespaces, want) {
			t.Errorf("repository has no %s namespace: %v", want, namespaces)
		}
	}
	// Chunks and tree nodes are inside the packs: the directories that held
	// one object each, two levels deep for chunks, are not created any more.
	for _, gone := range []string{"latest", "chunks", "trees", "markers"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("stat %s/: %v, want not exist", gone, err)
		}
	}
}

// WithTime dates the snapshot and nothing else: the parent of the next
// backup stays the newest snapshot of the series by time, not the one
// written last.
func TestBackupWithTimeSetsCreatedAt(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	src := stubSource{name: "app", content: "one"}

	newest, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	at := time.Date(2015, 6, 1, 12, 30, 15, 0, time.FixedZone("east", 2*60*60))
	dated, err := repo.Backup(ctx, src, ark.WithTime(at))
	if err != nil {
		t.Fatalf("Backup WithTime: %v", err)
	}
	if !dated.CreatedAt.Equal(at) || dated.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %s, want %s in UTC", dated.CreatedAt, at.UTC())
	}
	if dated.ParentID != newest.ID {
		t.Errorf("a backup dated before its parent has parent %q, want %q", dated.ParentID, newest.ID)
	}
	stored, err := repo.Snapshot(ctx, dated.ID)
	if err != nil || !stored.CreatedAt.Equal(at) {
		t.Errorf("stored CreatedAt = %v, %v, want %s", stored, err, at.UTC())
	}

	next, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if next.ParentID != newest.ID {
		t.Errorf("parent = %q, want the newest by time %q, not the one written last", next.ParentID, newest.ID)
	}
}

func TestBackupRefusesAZeroOrFutureTime(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	for name, at := range map[string]time.Time{"zero": {}, "future": time.Now().Add(time.Hour)} {
		if err := ark.ValidateBackupOptions(ark.WithTime(at)); !errors.Is(err, ark.ErrInvalidTime) {
			t.Errorf("%s time: ValidateBackupOptions = %v, want ErrInvalidTime", name, err)
		}
		_, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}, ark.WithTime(at))
		if !errors.Is(err, ark.ErrInvalidTime) {
			t.Errorf("%s time: Backup = %v, want ErrInvalidTime", name, err)
		}
	}
	if snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(snaps) != 0 {
		t.Errorf("a refused backup left %d snapshots, %v", len(snaps), err)
	}
}

// A snapshot names a parent only when the backup took files from it: a parent
// of another source or one that cannot be loaded is read around and left out.
func TestBackupRecordsOnlyAParentItUsed(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	other, err := repo.Backup(ctx, stubSource{name: "other", content: "one"})
	if err != nil {
		t.Fatalf("backup of the other source: %v", err)
	}
	foreign, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}, ark.WithParent(other.ID))
	if err != nil {
		t.Fatalf("backup with a foreign parent: %v", err)
	}
	if foreign.ParentID != "" {
		t.Errorf("a foreign parent was recorded: %q", foreign.ParentID)
	}

	used, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
	if err != nil {
		t.Fatalf("backup with a usable parent: %v", err)
	}
	if used.ParentID != foreign.ID {
		t.Errorf("parent is %q, want the one the backup used, %q", used.ParentID, foreign.ID)
	}

	for _, name := range blobNames(t, repo, ark.TreeFile) {
		deletePackOf(t, b, repo, ark.TreeFile, name)
	}
	broken, err := repo.Backup(ctx, stubSource{name: "app", content: "three"})
	if err != nil {
		t.Fatalf("backup with an unloadable parent: %v", err)
	}
	if broken.ParentID != "" {
		t.Errorf("an unloadable parent was recorded: %q", broken.ParentID)
	}
}

// A parent whose files the backup cannot take, here because their mtime is too
// close to the parent's start, is not recorded either.
func TestBackupRecordsNoParentItTookNothingFrom(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	first, err := repo.Backup(ctx, stubSource{name: "app", content: "one", modTime: time.Now()})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	second, err := repo.Backup(ctx, stubSource{name: "app", content: "one", modTime: time.Now()})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if second.ParentID != "" {
		t.Errorf("parent %q was recorded though every file of %q was racy", second.ParentID, first.ID)
	}
}
