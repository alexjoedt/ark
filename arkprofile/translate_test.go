package arkprofile_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/arkuri"
)

func TestBackupAndForgetThroughAProfile(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.NoPassword()); err != nil {
		t.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.NoPassword())
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
			Labels:   map[string]string{"app": "docs"},
		},
		Forget: arkprofile.Forget{KeepLast: 1},
	}
	// other backs up the same directory under a source of its own
	other := &arkprofile.Profile{Name: "other", Backup: arkprofile.Backup{Path: dir, Source: "other"}}

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

	if first.SourceName != dir || first.Host != "laptop" || first.Comment != "nightly" ||
		!reflect.DeepEqual(first.Tags, []string{"daily"}) || !reflect.DeepEqual(first.Excludes, []string{"*.tmp"}) ||
		first.Labels["app"] != "docs" {
		t.Fatalf("snapshot = %+v", first)
	}
	if first.Stats.FileCount != 1 {
		t.Fatalf("FileCount = %d, want 1 (skip.tmp excluded)", first.Stats.FileCount)
	}

	policy := docs.ForgetPolicy()
	filter, err := docs.Filter()
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.Forget(ctx, policy, filter)
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
	got, want := ids(left), []ark.SnapshotID{otherOld.ID, otherNew.ID, last.ID}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("left %v, want %v", got, want)
	}
}

func TestSourceNames(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		p    arkprofile.Profile
		want string
	}{
		{"path, not the profile name", arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: dir}}, dir},
		{"source wins", arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: dir, Source: "d"}}, "d"},
		{"no name keeps the path", arkprofile.Profile{Backup: arkprofile.Backup{Path: dir}}, dir},
		{"relative path made absolute", arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: "."}}, wd},
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
			// without backup.source the name is the one a plain backup of the path stores
			if tt.p.Backup.Source == "" {
				plain, err := arkfs.NewSource(tt.p.Backup.Path)
				if err != nil {
					t.Fatal(err)
				}
				if plain.Meta().Name != tt.want {
					t.Errorf("plain backup name = %q, want %q", plain.Meta().Name, tt.want)
				}
			}
			f, err := tt.p.Filter()
			if err != nil {
				t.Fatal(err)
			}
			if f.Source != tt.want {
				t.Errorf("filter source = %q, want %q", f.Source, tt.want)
			}
		})
	}
}

// The profile's name never stands in for a missing backup.source and
// backup.path.
func TestFilterWithoutASourceFails(t *testing.T) {
	p := &arkprofile.Profile{Name: "docs", Forget: arkprofile.Forget{KeepLast: 1}}
	_, err := p.Filter()
	if !errors.Is(err, arkprofile.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	for _, key := range []string{"backup.path", "backup.source", "backup.volume"} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("err = %v, want %s named", err, key)
		}
	}
}

// A volume profile selects the volume's docker-volume series, never a
// directory backed up under the same source name, and a path profile only
// file snapshots.
func TestFilterSelectsTheSourceType(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.NoPassword()); err != nil {
		t.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a", 0o600)
	files := &arkprofile.Profile{Name: "files", Backup: arkprofile.Backup{Path: dir, Source: "pgdata", Host: "db1"}}
	src, err := files.Source()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Backup(ctx, src, files.BackupOptions()...); err != nil {
		t.Fatal(err)
	}

	vol := &arkprofile.Profile{Name: "db", Backup: arkprofile.Backup{Volume: "pgdata", Host: "db1"}}
	f, err := vol.Filter()
	if err != nil {
		t.Fatal(err)
	}
	if f.Source != "pgdata" || f.Type != "docker-volume" || f.Host != "db1" {
		t.Fatalf("volume filter = %+v", f)
	}
	snaps, err := repo.Snapshots(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("volume filter selects %d file snapshots", len(snaps))
	}

	ff, err := files.Filter()
	if err != nil {
		t.Fatal(err)
	}
	if ff.Type != ark.SourceTypeFile {
		t.Fatalf("path filter type = %q, want %q", ff.Type, ark.SourceTypeFile)
	}
	if snaps, err := repo.Snapshots(ctx, ff); err != nil || len(snaps) != 1 {
		t.Fatalf("path filter selects %d snapshots, err %v; want 1", len(snaps), err)
	}
}

// The volume source needs the Docker SDK, which arkprofile does not import.
func TestVolumeProfileHasNoSource(t *testing.T) {
	p := &arkprofile.Profile{Name: "db", Backup: arkprofile.Backup{Volume: "pgdata"}}
	if _, err := p.Source(); !errors.Is(err, arkprofile.ErrInvalid) || !strings.Contains(err.Error(), "arkdocker") {
		t.Fatalf("Source = %v, want ErrInvalid pointing to arkdocker", err)
	}
}

func TestNoDockerSDK(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	out, err := exec.CommandContext(t.Context(), gobin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for dep := range strings.FieldsSeq(string(out)) {
		if strings.HasPrefix(dep, "github.com/docker/") || strings.HasPrefix(dep, "github.com/moby/") ||
			strings.HasSuffix(dep, "/arkdocker") {
			t.Errorf("arkprofile depends on %s", dep)
		}
	}
}

// A relative backup.path that cannot be made absolute fails the filter rather
// than select a source name no backup stores.
func TestFilterReportsAnUnresolvablePath(t *testing.T) {
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	p := &arkprofile.Profile{Backup: arkprofile.Backup{Path: "docs"}}
	if f, err := p.Filter(); err == nil {
		t.Fatalf("filter = %+v, want an error", f)
	}
}

// A profile without backup.host records the machine's hostname and filters on
// it, so another machine's snapshots of the same name are neither its parent
// nor forgotten by it.
func TestHostDefaultsToTheHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skip("no hostname")
	}
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.NoPassword()); err != nil {
		t.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a", 0o600)
	mine := &arkprofile.Profile{
		Name: "docs", Backup: arkprofile.Backup{Path: dir}, Forget: arkprofile.Forget{KeepLast: 1},
	}
	theirs := &arkprofile.Profile{Name: "docs", Backup: arkprofile.Backup{Path: dir, Host: hostname + "-other"}}

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
	other := backup(theirs)
	first := backup(mine)
	if first.Host != hostname {
		t.Fatalf("host = %q, want %q", first.Host, hostname)
	}
	if first.ParentID != "" {
		t.Fatalf("parent = %q, another host's snapshot", first.ParentID)
	}
	backup(mine)

	f, err := mine.Filter()
	if err != nil {
		t.Fatal(err)
	}
	if f.Host != hostname {
		t.Fatalf("filter host = %q, want %q", f.Host, hostname)
	}
	policy := mine.ForgetPolicy()
	res, err := repo.Forget(ctx, policy, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Remove) != 1 || res.Remove[0].ID != first.ID {
		t.Fatalf("removed %v, want only %s", ids(res.Remove), first.ID)
	}
	if _, err := repo.Snapshot(ctx, other.ID); err != nil {
		t.Fatalf("other host's snapshot: %v", err)
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
		KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: ark.Unlimited,
		KeepWithin: ark.Within{Days: 2},

		KeepWithinHourly: ark.Within{Hours: 3}, KeepWithinDaily: ark.Within{Days: 4},
		KeepWithinWeekly: ark.Within{Days: 35}, KeepWithinMonthly: ark.Within{Months: 6},
		KeepWithinYearly: ark.Within{Years: 7},

		KeepTags: []string{"pinned", "db, prod", ""},
	}}
	got := p.ForgetPolicy()
	want := ark.ForgetPolicy{
		KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: ark.Unlimited,
		KeepWithin: ark.Within{Days: 2},

		KeepWithinHourly: ark.Within{Hours: 3}, KeepWithinDaily: ark.Within{Days: 4},
		KeepWithinWeekly: ark.Within{Days: 35}, KeepWithinMonthly: ark.Within{Months: 6},
		KeepWithinYearly: ark.Within{Years: 7},

		KeepTags: [][]string{{"pinned"}, {"db", "prod"}, {""}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()

	_, err := (&arkprofile.Profile{Name: "docs"}).Open(ctx, ark.NoPassword())
	if !errors.Is(err, arkprofile.ErrInvalid) {
		t.Fatalf("empty repo: err = %v, want ErrInvalid", err)
	}

	p := &arkprofile.Profile{Repo: filepath.Join(t.TempDir(), "repo")}
	if err := os.Mkdir(p.Repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(ctx, ark.NoPassword()); !errors.Is(err, ark.ErrNotInitialized) {
		t.Fatalf("uninitialized: err = %v, want ErrNotInitialized", err)
	}

	p.Repo = "local://" + p.Repo
	if err := initAt(ctx, p.Repo); err != nil {
		t.Fatal(err)
	}
	repo, err := p.Open(ctx, ark.NoPassword())
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
	return ark.Init(ctx, b, ark.NoPassword())
}

func ids(snaps []*ark.Snapshot) []ark.SnapshotID {
	out := make([]ark.SnapshotID, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

// TestEveryForgetRuleIsCarriedAndChecked holds each keep count and keep-within
// window of Forget, found by reflection, to the ForgetPolicy field of its name
// and to Validate, so a rule added to Forget cannot miss either.
func TestEveryForgetRuleIsCarriedAndChecked(t *testing.T) {
	samples := map[reflect.Type][2]any{
		reflect.TypeFor[ark.KeepCount](): {ark.KeepCount(3), ark.KeepCount(-2)},
		reflect.TypeFor[ark.Within]():    {ark.Within{Days: 3}, ark.Within{Days: -1}},
	}
	ft := reflect.TypeFor[arkprofile.Forget]()
	rules := 0
	for i := range ft.NumField() {
		f := ft.Field(i)
		s, ok := samples[f.Type]
		if !ok {
			continue
		}
		rules++
		key := "forget." + f.Tag.Get("yaml")
		for j, v := range s {
			p := arkprofile.Profile{Name: "docs", Repo: "/srv/ark", PasswordEnv: "DOCS_PW"}
			reflect.ValueOf(&p.Forget).Elem().Field(i).Set(reflect.ValueOf(v))
			err := p.Validate()
			if j == 0 {
				pf := reflect.ValueOf(p.ForgetPolicy()).FieldByName(f.Name)
				if err != nil || !pf.IsValid() || !pf.Equal(reflect.ValueOf(v)) {
					t.Errorf("%s = %v: Validate %v, policy field %v", key, v, err, pf)
				}
			} else if !errors.Is(err, arkprofile.ErrInvalid) || !strings.Contains(err.Error(), key) {
				t.Errorf("%s = %v: Validate %v, want ErrInvalid naming the key", key, v, err)
			}
		}
	}
	if rules != 12 {
		t.Errorf("found %d rules in Forget, want 12", rules)
	}
}
