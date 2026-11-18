package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// futureArchive writes an export archive of a fresh one-file snapshot whose
// created_at is replaced by at, and returns its path. ark backup --time
// refuses a future time, so this is the way to get such a snapshot into a
// repository.
func futureArchive(t *testing.T, at time.Time) string {
	t.Helper()
	repo := initRepo(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, "--json", "-r", repo, "backup", src)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	code, archive := runCapture(t, "-r", repo, "export", snap.ID)
	if code != 0 {
		t.Fatalf("export exit %d", code)
	}

	var rewritten bytes.Buffer
	tr := tar.NewReader(strings.NewReader(archive))
	tw := tar.NewWriter(&rewritten)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "metadata.json" {
			var meta map[string]any
			if err := json.Unmarshal(body, &meta); err != nil {
				t.Fatal(err)
			}
			meta["created_at"] = at.UTC()
			if body, err = json.Marshal(meta); err != nil {
				t.Fatal(err)
			}
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "future.tar")
	if err := os.WriteFile(path, rewritten.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportWarnsAboutAFutureSnapshot(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	target := initRepo(t)

	code, out, logs := runCaptureAll(t, "--json", "-r", target, "import", futureArchive(t, time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC)))
	if code != 0 {
		t.Fatalf("import exit %d, stderr %q", code, logs)
	}
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil || snap.ID == "" {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	want := "warning: snapshot " + snap.ID[:8] + " (2099-"
	if !strings.Contains(logs, want) || !strings.Contains(logs, "is dated in the future") {
		t.Fatalf("stderr %q lacks %q", logs, want)
	}

	code, _, logs = runCaptureAll(t, "--quiet", "-r", initRepo(t), "import", futureArchive(t, time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC)))
	if code != 0 || strings.Contains(logs, "dated in the future") {
		t.Fatalf("--quiet: exit %d, stderr %q", code, logs)
	}
}

func TestImportOfAPastSnapshotDoesNotWarn(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	code, _, logs := runCaptureAll(t, "-r", initRepo(t), "import", futureArchive(t, time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)))
	if code != 0 || strings.Contains(logs, "dated in the future") {
		t.Fatalf("exit %d, stderr %q", code, logs)
	}
}
