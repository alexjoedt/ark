package ark_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arkmem"
)

// stubSource yields one regular file under a fixed source name.
type stubSource struct {
	name    string
	content string
}

func (s stubSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: ark.SourceTypeFile}
}

func (s stubSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	info := ark.FileInfo{
		Path: "data.txt",
		Size: int64(len(s.content)),
		Mode: 0o644,
		Type: ark.FileTypeRegular,
	}
	return fn(info, strings.NewReader(s.content))
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
	for _, want := range []string{"config", "chunks", "trees", "snapshots"} {
		if !slices.Contains(namespaces, want) {
			t.Errorf("repository has no %s namespace: %v", want, namespaces)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "latest")); !os.IsNotExist(err) {
		t.Errorf("stat latest/: %v, want not exist", err)
	}
}
