package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	r, err := ark.Open(context.Background(), b, ark.WithoutPassword())
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
