package arkprofile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/arkuri"
)

func TestBackupAndForgetThroughAProfile(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep", 0o600)
	writeFile(t, filepath.Join(dir, "skip.tmp"), "skip", 0o600)

	docs := &arkprofile.Profile{
		Name: "docs",
		Backup: arkprofile.Backup{
			Path:     dir,
			Tags:     []string{"daily"},
			Excludes: []string{"*.tmp"},
			Host:     "laptop",
			Comment:  "nightly",
		},
		Forget: arkprofile.Forget{KeepLast: 1},
	}
	other := &arkprofile.Profile{Name: "other", Backup: arkprofile.Backup{Path: dir}}

	backup := func(p *arkprofile.Profile) *ark.Snapshot {
		t.Helper()
		src, err := p.Source()
		if err != nil {
			t.Fatal(err)
		}
		snap, err := repo.Backup(ctx, src, p.BackupOptions()...)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	// Two snapshots of other: a forget with keep-last 1 over every source
	// would remove one of them.
	first := backup(docs)
	otherOld := backup(other)
	otherNew := backup(other)
	last := backup(docs)

	if first.SourceName != "docs" || first.Host != "laptop" || first.Comment != "nightly" ||
		!reflect.DeepEqual(first.Tags, []string{"daily"}) || !reflect.DeepEqual(first.Excludes, []string{"*.tmp"}) {
		t.Fatalf("snapshot = %+v", first)
	}
	if first.Stats.FileCount != 1 {
		t.Fatalf("FileCount = %d, want 1 (skip.tmp excluded)", first.Stats.FileCount)
	}

	policy, err := docs.ForgetPolicy()
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.Forget(ctx, policy, docs.Filter())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Remove) != 1 || res.Remove[0].ID != first.ID {
		t.Fatalf("removed %v, want only %s", ids(res.Remove), first.ID)
	}

	left, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got, want := ids(left), []string{otherOld.ID, otherNew.ID, last.ID}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("left %v, want %v", got, want)
	}
}

func TestSourceNames(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		p    arkprofile.Profile
		want string
	}{
		{"profile name", arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: dir}}, "docs"},
		{"backup.name wins", arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: dir, Name: "d"}}, "d"},
		{"no name keeps the path", arkprofile.Profile{Backup: arkprofile.Backup{Path: dir}}, dir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := tt.p.Source()
			if err != nil {
				t.Fatal(err)
			}
			if got := src.Meta().Name; got != tt.want {
				t.Errorf("source name = %q, want %q", got, tt.want)
			}
			if got := tt.p.Filter().Name; got != tt.want {
				t.Errorf("filter name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSourceWithoutAPath(t *testing.T) {
	_, err := (&arkprofile.Profile{Name: "docs"}).Source()
	if !errors.Is(err, arkprofile.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestForgetPolicy(t *testing.T) {
	p := arkprofile.Profile{Forget: arkprofile.Forget{
		KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: 6, KeepWithin: "2d",
	}}
	got, err := p.ForgetPolicy()
	if err != nil {
		t.Fatal(err)
	}
	want := ark.ForgetPolicy{
		KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: 6,
		KeepWithin: 48 * time.Hour,
	}
	if got != want {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}

	p.Forget.KeepWithin = "soon"
	if _, err := p.ForgetPolicy(); !errors.Is(err, arkprofile.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()

	_, err := (&arkprofile.Profile{Name: "docs"}).Open(ctx, ark.WithoutPassword())
	if !errors.Is(err, arkprofile.ErrInvalid) {
		t.Fatalf("empty repo: err = %v, want ErrInvalid", err)
	}

	p := &arkprofile.Profile{Repo: filepath.Join(t.TempDir(), "repo")}
	if err := os.Mkdir(p.Repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(ctx, ark.WithoutPassword()); !errors.Is(err, ark.ErrNotInitialized) {
		t.Fatalf("uninitialized: err = %v, want ErrNotInitialized", err)
	}

	p.Repo = "local://" + p.Repo
	if err := initAt(ctx, p.Repo); err != nil {
		t.Fatal(err)
	}
	repo, err := p.Open(ctx, ark.WithoutPassword())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func initAt(ctx context.Context, uri string) error {
	b, err := arkuri.Open(ctx, uri)
	if err != nil {
		return err
	}
	defer b.Close()
	return ark.Init(ctx, b, ark.WithoutPassword())
}

func ids(snaps []*ark.Snapshot) []string {
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}
