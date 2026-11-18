package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForgetWarnsAboutAFutureSnapshot(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	repo := initRepo(t)
	if code, out := runCapture(
		t,
		"-r",
		repo,
		"import",
		archiveDatedAt(t, time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC)),
	); code != 0 {
		t.Fatalf("import exit %d: %s", code, out)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, out := runCapture(t, "-r", repo, "backup", src); code != 0 {
			t.Fatalf("backup exit %d: %s", code, out)
		}
	}

	for _, args := range [][]string{
		{"forget", "--keep-last", "1", "--dry-run"},
		{"forget", "--keep-last", "1", "--yes"},
	} {
		code, _, logs := runCaptureAll(t, append([]string{"-r", repo}, args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, logs)
		}
		if n := strings.Count(logs, "warning: snapshot "); n != 1 ||
			!strings.Contains(logs, "(2099-01-01 ") ||
			!strings.Contains(logs, "is dated in the future; kept and ignored by the policy") {
			t.Fatalf("%v: stderr %q", args, logs)
		}
	}

	code, out, logs := runCaptureAll(t, "--json", "-r", repo, "forget", "--keep-last", "1", "--dry-run")
	if code != 0 || !strings.Contains(logs, "dated in the future") {
		t.Fatalf("--json: exit %d, stderr %q", code, logs)
	}
	var res struct {
		Keep []struct {
			Reasons []string `json:"reasons"`
		} `json:"keep"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not one JSON document: %q", out)
	}
	var found bool
	for _, k := range res.Keep {
		found = found || strings.Join(k.Reasons, ",") == "in the future"
	}
	if !found {
		t.Fatalf("no keep entry with reason in the future: %q", out)
	}

	code, _, logs = runCaptureAll(t, "--quiet", "-r", repo, "forget", "--keep-last", "1", "--dry-run")
	if code != 0 || strings.Contains(logs, "dated in the future") {
		t.Fatalf("--quiet: exit %d, stderr %q", code, logs)
	}
}

func TestForgetByIDOfAFutureSnapshotDoesNotWarn(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	repo := initRepo(t)
	code, out := runCapture(
		t,
		"--json",
		"-r",
		repo,
		"import",
		archiveDatedAt(t, time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC)),
	)
	if code != 0 {
		t.Fatalf("import exit %d: %s", code, out)
	}
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	code, _, logs := runCaptureAll(t, "-r", repo, "forget", "--yes", snap.ID)
	if code != 0 || strings.Contains(logs, "warning") {
		t.Fatalf("exit %d, stderr %q", code, logs)
	}
	if _, out := runCapture(t, "--json", "-r", repo, "list"); strings.Contains(out, snap.ID) {
		t.Fatalf("snapshot still listed: %q", out)
	}
}
