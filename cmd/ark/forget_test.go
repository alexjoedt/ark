package main

import (
	"archive/tar"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	_ "time/tzdata" // the child resolves TZ on a machine without a zone database too
)

// Half an hour either side of midnight in Berlin is two days there and one
// day in UTC. The CLI names no zone, so TZ decides what --keep-daily keeps.
func TestForgetCutsPeriodsInTheZoneOfTZ(t *testing.T) {
	repo := initRepo(t)
	for _, at := range []string{"2026-01-14T22:30:00Z", "2026-01-14T23:30:00Z"} {
		importEmptySnapshot(t, repo, at)
	}

	kept := func(tz string) int {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), os.Args[0],
			"--json", "-r", repo, "forget", "--keep-daily", "2", "--dry-run")
		cmd.Env = append(os.Environ(), envCLIChild+"=1", "TZ="+tz)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("TZ=%s forget: %v: %s%s", tz, err, out, &stderr)
		}
		var res struct {
			Keep   []struct{ ID string } `json:"keep"`
			Remove []struct{ ID string } `json:"remove"`
		}
		if err := json.Unmarshal(out, &res); err != nil || len(res.Keep)+len(res.Remove) != 2 {
			t.Fatalf("TZ=%s forget: not two snapshots: %v: %q", tz, err, out)
		}
		return len(res.Keep)
	}
	if berlin, utc := kept("Europe/Berlin"), kept("UTC"); berlin != 2 || utc != 1 {
		t.Errorf("--keep-daily 2 keeps %d under TZ=Europe/Berlin and %d under TZ=UTC, want 2 and 1", berlin, utc)
	}
}

// importEmptySnapshot imports an archive of no files whose metadata carries
// the creation time, an RFC 3339 string.
func importEmptySnapshot(t *testing.T, repo, createdAt string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"version": 3, "created_at": createdAt, "source_name": "app", "source_type": "file",
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "in.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "metadata.json", Mode: 0o644, Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(meta); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := runCapture(t, "-r", repo, "import", archive); code != 0 {
		t.Fatalf("import exit %d: %s", code, out)
	}
}
