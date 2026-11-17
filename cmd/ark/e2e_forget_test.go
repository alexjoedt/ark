package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkuri"
)

// forgetHistory is the history every forget case starts from, in backup
// order: two hosts backing up one tree that changes between any two of them.
// The times are Europe/Berlin clock times. They are picked so a rule that
// counted in UTC, by calendar year instead of ISO week year, or over both
// hosts at once would keep something else:
//
//   - a7 and a8 lie either side of midnight in Berlin and on one day in UTC
//   - a1 and a2 share ISO week 2025-W01 across the turn of the year
//   - a5 and a6 share an hour
//   - b2 is among the three newest of the repository, not of its host alone
var forgetHistory = []struct {
	name, host, at string
	tags           []string
}{
	{"a0", "alpha", "2024-03-10 12:00:00", []string{"archive"}},
	{"a1", "alpha", "2024-12-30 12:00:00", nil},
	{"a2", "alpha", "2025-01-02 12:00:00", nil},
	{"a3", "alpha", "2025-05-20 12:00:00", []string{"pinned"}},
	{"b0", "beta", "2025-06-01 12:00:00", []string{"archive"}},
	{"a4", "alpha", "2025-06-09 08:00:00", nil},
	{"b1", "beta", "2025-06-10 12:00:00", []string{"pinned"}},
	{"a5", "alpha", "2025-06-11 10:15:00", nil},
	{"a6", "alpha", "2025-06-11 10:45:00", []string{"pinned", "db"}},
	{"a7", "alpha", "2025-06-11 23:30:00", nil},
	{"a8", "alpha", "2025-06-12 00:30:00", []string{"db"}},
	{"b2", "beta", "2025-06-12 06:00:00", nil},
	{"a9", "alpha", "2025-06-12 09:00:00", nil},
}

// forgetCases has one entry per rule family and one combination. keep is
// worked out by hand from the rules, per host, never by asking the library.
var forgetCases = []struct {
	name string
	args []string
	keep string
}{
	{"last", []string{"--keep-last", "3"}, "a7 a8 a9 b0 b1 b2"},
	// in UTC the day of 11 June would be a8's
	{"daily", []string{"--keep-daily", "3"}, "a4 a7 a9 b0 b1 b2"},
	// a1 is in the week a2 took; the fourth week is a0's
	{"weekly", []string{"--keep-weekly", "4"}, "a0 a2 a3 a9 b0 b2"},
	// b0 shares b2's month and stays as the oldest of its series
	{"unlimited", []string{"--keep-monthly", "unlimited"}, "a0 a1 a2 a3 a9 b0 b2"},
	// two days back from the newest of each host: a9 and b2
	{"within", []string{"--keep-within", "2d"}, "a5 a6 a7 a8 a9 b1 b2"},
	// a5 loses its hour to a6; a0 is the oldest but outside the year
	{
		"within periods",
		[]string{"--keep-within-hourly", "2d", "--keep-within-weekly", "1y"},
		"a2 a3 a6 a7 a8 a9 b0 b1 b2",
	},
	// pinned alone (a3, b1) or db alone (a8) is not a list
	{"tag", []string{"--keep-tag", "pinned,db", "--keep-tag", "archive"}, "a0 a6 b0"},
	{
		"combination",
		[]string{"--keep-within", "12h", "--keep-daily", "2", "--keep-yearly", "2", "--keep-tag", "pinned"},
		"a1 a3 a6 a7 a8 a9 b0 b1 b2",
	},
}

// TestE2EForget backs the history up once per backend, copies it into a
// fresh repository per case, runs forget with a prune, and holds what is left
// against the case: the kept ids, a clean check, and every kept snapshot
// restoring to the tree it was taken from.
func TestE2EForget(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("over a dozen backups and a hundred CLI runs, skipped with -short")
	}
	cases := func(t *testing.T, newRepo func(t *testing.T) string) {
		h := backUpForgetHistory(t, newRepo(t))
		for _, tc := range forgetCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel() // a repository per case; most of a run is waiting on the disk
				repo := newRepo(t)
				copyRepo(t, h.repo, repo)
				forgetRun(t, h, repo, tc.args, strings.Fields(tc.keep))
			})
		}
	}
	t.Run("local", func(t *testing.T) {
		t.Parallel() // the two serial backups of the history overlap
		cases(t, func(t *testing.T) string { return filepath.Join(t.TempDir(), "repo") })
	})
	t.Run("sftp", func(t *testing.T) {
		t.Parallel()
		cases(t, func(t *testing.T) string { return sftpTestRepo(t, "e2e-forget-") })
	})
}

// forgetCLI runs the CLI against repo and returns its stdout. The CLI is this
// test binary run again: it carries the zone database, so TZ decides the
// periods on a machine without one too.
func forgetCLI(t *testing.T, repo string) func(args ...string) []byte {
	return func(args ...string) []byte {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append([]string{"--json", "-q", "-r", repo}, args...)
		cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
		cmd.Env = append(os.Environ(), envCLIChild+"=1", "TZ=Europe/Berlin",
			"ARK_REPOSITORY=", "ARK_PASSWORD=", "ARK_PASSWORD_FILE=", "ARK_PASSWORD_COMMAND=")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ark %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
		}
		return stdout.Bytes()
	}
}

type forgetSnapshot struct {
	ID string `json:"id"`
}

// forgetBackups is the history backed up into repo, which the cases copy
// and only read.
type forgetBackups struct {
	repo string
	id   map[string]string            // name -> snapshot id
	name map[string]string            // snapshot id -> name
	tree map[string]map[string]string // name -> the tree it was taken from
}

func backUpForgetHistory(t *testing.T, repo string) *forgetBackups {
	ark := forgetCLI(t, repo)
	ark("init")
	src := t.TempDir()
	writeTree(t, src, map[string]string{"keep.txt": "never changes", "sub/keep.md": "# kept"})
	if err := os.Symlink("sub/keep.md", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	h := &forgetBackups{
		repo: repo,
		id:   map[string]string{},
		name: map[string]string{},
		tree: map[string]map[string]string{},
	}
	for step, s := range forgetHistory {
		// every snapshot has a file of its own, shares shared.bin with up to
		// three neighbours, and sees a.txt change length
		if err := os.RemoveAll(filepath.Join(src, "only")); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"a.txt":                   strings.Repeat("s", step+1),
			"only/" + s.name + ".bin": randomBytes(t, 64<<10),
		}
		if step%4 == 0 {
			files["shared.bin"] = randomBytes(t, 256<<10)
		}
		writeTree(t, src, files)

		args := []string{"backup", "--time", s.at, "--host", s.host}
		for _, tag := range s.tags {
			args = append(args, "--tag", tag)
		}
		var snap forgetSnapshot
		if err := json.Unmarshal(ark(append(args, src)...), &snap); err != nil || snap.ID == "" {
			t.Fatalf("backup %s: no snapshot id: %v", s.name, err)
		}
		h.id[s.name], h.name[snap.ID], h.tree[s.name] = snap.ID, s.name, readTree(t, src)
	}
	return h
}

// copyRepo copies every object of the repository at from into the one at to.
func copyRepo(t *testing.T, from, to string) {
	t.Helper()
	ctx := t.Context()
	open := func(uri string) ark.Backend {
		b, err := arkuri.Open(ctx, uri)
		if err != nil {
			t.Fatalf("open repository: %v", err)
		}
		t.Cleanup(func() { b.Close() })
		return b
	}
	src, dst := open(from), open(to)
	for _, typ := range repoTypes {
		var names []string
		if err := src.List(ctx, typ, func(info ark.BlobInfo) error {
			names = append(names, info.Name)
			return nil
		}); err != nil {
			t.Fatalf("list %s: %v", typ, err)
		}
		for _, name := range names {
			h := ark.Handle{Type: typ, Name: name}
			r, err := src.Get(ctx, h)
			if err != nil {
				t.Fatalf("get %s %s: %v", typ, name, err)
			}
			err = dst.Put(ctx, h, r)
			r.Close()
			if err != nil {
				t.Fatalf("put %s %s: %v", typ, name, err)
			}
		}
	}
	if err := dst.Sync(ctx); err != nil {
		t.Fatalf("sync copy: %v", err)
	}
}

func forgetRun(t *testing.T, h *forgetBackups, repo string, rule, keep []string) {
	ark := forgetCLI(t, repo)

	var wantKeep, wantRemove []string
	for _, s := range forgetHistory {
		if slices.Contains(keep, s.name) {
			wantKeep = append(wantKeep, s.name)
		} else {
			wantRemove = append(wantRemove, s.name)
		}
	}
	slices.Sort(wantKeep)
	slices.Sort(wantRemove)
	if len(wantKeep) != len(keep) || len(wantRemove) == 0 {
		t.Fatalf("case keeps %v of the history, want some removed and every name known", keep)
	}
	names := func(snaps []forgetSnapshot) []string {
		var out []string
		for _, s := range snaps {
			out = append(out, cmp.Or(h.name[s.ID], s.ID))
		}
		slices.Sort(out)
		return out
	}

	// forget --prune writes two values: the forget result, then the prune's
	var forgot struct {
		Keep, Remove []forgetSnapshot
	}
	var pruned struct {
		BlobsRemoved int `json:"blobs_removed"`
	}
	dec := json.NewDecoder(bytes.NewReader(ark(append([]string{"forget", "--prune", "--yes"}, rule...)...)))
	if err := dec.Decode(&forgot); err != nil {
		t.Fatalf("forget result: %v", err)
	}
	if err := dec.Decode(&pruned); err != nil {
		t.Fatalf("prune result after forget: %v", err)
	}
	if got := names(forgot.Keep); !slices.Equal(got, wantKeep) {
		t.Errorf("forget %v keeps %v, want %v", rule, got, wantKeep)
	}
	if got := names(forgot.Remove); !slices.Equal(got, wantRemove) {
		t.Errorf("forget %v removes %v, want %v", rule, got, wantRemove)
	}
	// every snapshot has a file no other has, so a removal frees blobs
	if pruned.BlobsRemoved == 0 {
		t.Errorf("prune after forget %v removed no blob", rule)
	}

	var left []forgetSnapshot
	if err := json.Unmarshal(ark("snapshots"), &left); err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if got := names(left); !slices.Equal(got, wantKeep) {
		t.Errorf("after forget %v the repository holds %v, want %v", rule, got, wantKeep)
	}

	// check exits non-zero on a problem, so ark has failed the test on one
	var checked struct {
		SnapshotsChecked int `json:"snapshots_checked"`
	}
	if err := json.Unmarshal(ark("check"), &checked); err != nil {
		t.Fatalf("check: %v", err)
	}
	if checked.SnapshotsChecked != len(wantKeep) {
		t.Errorf("check after forget %v checked %d snapshots, want %d", rule, checked.SnapshotsChecked, len(wantKeep))
	}
	for _, n := range wantKeep {
		dest := filepath.Join(t.TempDir(), "out")
		ark("restore", h.id[n], dest)
		compareTrees(t, fmt.Sprintf("%s after forget %v", n, rule), h.tree[n], readTree(t, dest))
	}
}
