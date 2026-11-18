package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func forgetListRepo(t *testing.T, n int) (repo string, ids []string) {
	t.Helper()
	t.Setenv("ARK_REPOSITORY", "")
	repo = initRepo(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range n {
		code, out := runCapture(t, "--json", "-r", repo, "backup", "--source", "docs", src)
		if code != 0 {
			t.Fatalf("backup exit %d: %s", code, out)
		}
		var snap struct{ ID string }
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, snap.ID)
	}
	return repo, ids
}

func countLines(out, prefix string) int {
	n := 0
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestForgetListsBeforeThePrompt(t *testing.T) {
	repo, _ := forgetListRepo(t, 3)

	// a non-tty run without --yes lists, names one question and removes nothing
	code, out, logs := runCaptureAll(t, "-r", repo, "forget", "--keep-last", "1", "--prune")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %q", code, logs)
	}
	if !strings.Contains(logs, "remove 2 snapshots and prune unreferenced data: confirmation needed") {
		t.Fatalf("stderr %q", logs)
	}
	if n := countLines(out, "remove "); n != 2 || !strings.Contains(out, "(docs)") {
		t.Fatalf("%d remove lines: %q", n, out)
	}
	if _, out := runCapture(t, "--json", "-r", repo, "snapshots"); strings.Count(out, `"id"`) != 3 {
		t.Fatalf("snapshots removed before confirmation: %q", out)
	}

	// --yes: the list once, then the summary, then the prune result
	code, out = runCapture(t, "-r", repo, "forget", "--keep-last", "1", "--prune", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out)
	}
	if countLines(out, "remove ") != 2 || countLines(out, "removed ") != 2 ||
		!strings.Contains(out, "removed 2 snapshots, kept 1") ||
		strings.Contains(out, "would") {
		t.Fatalf("output %q", out)
	}
	if !strings.Contains(out, "unused") {
		t.Fatalf("no prune result: %q", out)
	}
}

func TestForgetByIDListsBeforeThePrompt(t *testing.T) {
	repo, ids := forgetListRepo(t, 2)
	code, out, logs := runCaptureAll(t, "-r", repo, "forget", ids[0])
	if code != 2 || !strings.Contains(logs, "remove 1 snapshots: confirmation needed") ||
		!strings.Contains(out, "remove "+ids[0]) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, logs)
	}
	code, out = runCapture(t, "-r", repo, "forget", "--yes", ids[0])
	if code != 0 || strings.Count(out, ids[0]) != 1 || !strings.Contains(out, "removed 1 snapshots, kept 0") {
		t.Fatalf("exit %d: %q", code, out)
	}
}

func TestForgetNothingToRemoveDoesNotPrompt(t *testing.T) {
	repo, _ := forgetListRepo(t, 1)
	code, out := runCapture(t, "-r", repo, "forget", "--keep-last", "1", "--prune")
	if code != 0 || !strings.Contains(out, "remove 0 snapshots, kept 1") {
		t.Fatalf("exit %d: %q", code, out)
	}
}

func TestForgetJSONUnchanged(t *testing.T) {
	repo, _ := forgetListRepo(t, 2)
	code, out := runCapture(t, "--json", "-r", repo, "forget", "--keep-last", "1", "--yes")
	var res struct {
		Keep, Remove []json.RawMessage
		DryRun       bool `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 ||
		len(res.Remove) != 1 || len(res.Keep) != 1 || res.DryRun {
		t.Fatalf("exit %d, err %v: %q", code, err, out)
	}
}
