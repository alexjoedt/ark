package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
)

func storedLabels(t *testing.T, repo, id string) map[string]string {
	t.Helper()
	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(context.Background(), b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	snap, err := r.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Labels
}

func TestBackupRecordsThePathLabelUnderASource(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "docs", "repo: "+repo+"\nbackup:\n  path: "+src+"\n  source: notes\n")

	for name, snap := range map[string]profileSnapshot{
		"--source": backupSource(t, repo, src),
		"profile":  profileBackup(t, "-P", "docs"),
	} {
		if got := storedLabels(t, repo, snap.ID)["ark/path"]; got != src {
			t.Errorf("%s: ark/path = %q, want %q", name, got, src)
		}
	}
}

func backupSource(t *testing.T, repo, src string) profileSnapshot {
	t.Helper()
	code, out := runCapture(t, "--json", "-r", repo, "backup", "--source", "x", src)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	var s profileSnapshot
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	return s
}

type labeledSnapshot struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
}

func labeledIDs(t *testing.T, repo string, labels ...string) []string {
	t.Helper()
	args := []string{"--json", "-r", repo, "snapshots"}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	code, out := runCapture(t, args...)
	if code != 0 {
		t.Fatalf("snapshots %v exit %d: %s", labels, code, out)
	}
	var snaps []labeledSnapshot
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("stdout is not a list: %q", out)
	}
	ids := make([]string, 0, len(snaps))
	for _, s := range snaps {
		if s.Labels == nil {
			t.Fatalf("labels null in %s", out)
		}
		ids = append(ids, s.ID)
	}
	return ids
}

func TestLabelFlagsSetAndFilter(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, "--json", "-r", repo, "backup", "--source", "x",
		"--label", "app=my-app", "--label", "url=a=b,c", src)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	var snap labeledSnapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	if snap.Labels["app"] != "my-app" || snap.Labels["url"] != "a=b,c" || snap.Labels["ark/path"] != src {
		t.Fatalf("labels = %v", snap.Labels)
	}
	other := backupSource(t, repo, src)

	for _, tt := range []struct {
		labels []string
		want   []string
	}{
		{[]string{"app=my-app"}, []string{snap.ID}},
		{[]string{"app=other"}, nil},
		{[]string{"app=my-app", "url=a=b,c"}, []string{snap.ID}},
		{[]string{"app=my-app", "url=other"}, nil},
		{[]string{"ark/path=" + src}, []string{other.ID, snap.ID}},
	} {
		got := labeledIDs(t, repo, tt.labels...)
		if !slices.Equal(got, tt.want) {
			t.Errorf("--label %v = %v, want %v", tt.labels, got, tt.want)
		}
	}

	code, out = runCapture(t, "--json", "-r", repo, "find", "--label", "app=my-app", "a.txt")
	if code != 0 {
		t.Fatalf("find exit %d: %s", code, out)
	}
	var hit struct {
		SnapshotID string            `json:"snapshot_id"`
		Labels     map[string]string `json:"labels"`
	}
	err := json.Unmarshal([]byte(out), &hit)
	if err != nil || hit.SnapshotID != snap.ID || hit.Labels["app"] != "my-app" {
		t.Fatalf("find stdout %q, %v", out, err)
	}
	code, out = runCapture(t, "--json", "-r", repo, "find", "--label", "app=other", "a.txt")
	if code != 0 || out != "" {
		t.Fatalf("find app=other: exit %d, stdout %q", code, out)
	}

	if code, out = runCapture(t, "--json", "-r", repo, "forget", "--all", "--yes", "--label", "app=my-app"); code != 0 {
		t.Fatalf("forget exit %d: %s", code, out)
	}
	if got := labeledIDs(t, repo); !slices.Equal(got, []string{other.ID}) {
		t.Fatalf("left %v, want only %s", got, other.ID)
	}
}

func TestBadLabelsExitTwoBeforeOpen(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	repo := filepath.Join(t.TempDir(), "missing")
	src := t.TempDir()
	long := "app=" + strings.Repeat("x", 1025)
	// a filter checks the shape only, a backup the key and value as well
	malformed := []string{"app", "app=a --label app=b"}
	bad := append([]string{"-app=x", "my app=x", "Example.com/app=x", long, "app=a\tb"}, malformed...)
	for _, l := range bad {
		args := strings.Split(l, " --label ")
		cmds := [][]string{{"backup", "--source", "x"}}
		if slices.Contains(malformed, l) {
			cmds = append(cmds, []string{"snapshots"}, []string{"forget", "--all", "--yes"})
		}
		for _, cmd := range cmds {
			full := append([]string{"-r", repo}, cmd...)
			for _, a := range args {
				full = append(full, "--label", a)
			}
			if cmd[0] == "backup" {
				full = append(full, src)
			}
			if code, _ := runCapture(t, full...); code != 2 {
				t.Errorf("%v: exit %d, want 2", full[2:], code)
			}
		}
	}
	for _, args := range [][]string{
		{"backup", "--source", "x", "--label", "ark/path=/x", src},
		{"find", "--label", "app", "a.txt"},
	} {
		if code, _ := runCapture(t, append([]string{"-r", repo}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// a filter may name ark's own keys and keys a third-party source records
	for _, l := range []string{"ark/path=/x", "Example.com/app=x", "my app=x"} {
		if code, _ := runCapture(t, "-r", repo, "snapshots", "--label", l); code == 2 {
			t.Errorf("snapshots --label %s: exit 2, want a repository error", l)
		}
	}
}

func TestProfileLabelsMergeUnderTheCLI(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	writeProfile(t, dir, "docs", "repo: "+repo+"\nbackup:\n  path: "+src+"\n  label:\n    app: docs\n    env: prod\n")

	for _, tt := range []struct {
		args []string
		want map[string]string
	}{
		{nil, map[string]string{"app": "docs", "env": "prod"}},
		{
			[]string{"--label", "app=cli", "--label", "team=ops"},
			map[string]string{"app": "cli", "env": "prod", "team": "ops"},
		},
	} {
		code, out := runCapture(t, append([]string{"--json", "backup", "-P", "docs"}, tt.args...)...)
		if code != 0 {
			t.Fatalf("backup %v exit %d: %s", tt.args, code, out)
		}
		var snap labeledSnapshot
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatalf("stdout is not a snapshot: %q", out)
		}
		delete(snap.Labels, "ark/path")
		if !maps.Equal(snap.Labels, tt.want) {
			t.Errorf("--label %v: labels %v, want %v", tt.args, snap.Labels, tt.want)
		}
	}

	code, out := runCapture(t, "--json", "profile", "show", "docs")
	if code != 0 || !strings.Contains(out, `"app=\"docs\", env=\"prod\""`) {
		t.Fatalf("profile show exit %d: %s", code, out)
	}

	writeProfile(t, dir, "bad", "repo: "+repo+"\nbackup:\n  path: "+src+"\n  label:\n    ark/path: /x\n")
	if code, _ := runCapture(t, "backup", "-P", "bad"); code != 2 {
		t.Errorf("profile with a reserved label: exit %d, want 2", code)
	}
}
