package main

import (
	"archive/tar"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
			Keep   []struct{ Reasons []string } `json:"keep"`
			Remove []struct{ ID string }        `json:"remove"`
		}
		if err := json.Unmarshal(out, &res); err != nil || len(res.Keep)+len(res.Remove) != 2 {
			t.Fatalf("TZ=%s forget: not two snapshots: %v: %q", tz, err, out)
		}
		// the older snapshot is kept either way, as the newest of its day
		// or as the oldest of the series: the reason tells them apart
		days := 0
		for _, k := range res.Keep {
			if slices.Contains(k.Reasons, "daily snapshot") {
				days++
			}
		}
		return days
	}
	if berlin, utc := kept("Europe/Berlin"), kept("UTC"); berlin != 2 || utc != 1 {
		t.Errorf("--keep-daily 2 sees %d days under TZ=Europe/Berlin and %d under TZ=UTC, want 2 and 1", berlin, utc)
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

// A count of unlimited keeps the newest snapshot of every period and the
// oldest of the series, from a flag and from a profile; a negative count is a usage error naming the word.
func TestForgetTakesUnlimitedCounts(t *testing.T) {
	repo := initRepo(t)
	for _, at := range []string{
		"2023-03-01T12:00:00Z", "2023-06-01T12:00:00Z", "2024-03-01T12:00:00Z", "2024-06-01T12:00:00Z",
		"2025-06-01T12:00:00Z",
	} {
		importEmptySnapshot(t, repo, at)
	}
	profile := writeProfile(t, t.TempDir(), "app", "repo: "+repo+"\nforget:\n  keep-yearly: unlimited\n")

	for _, args := range [][]string{
		{"-r", repo, "forget", "--keep-yearly", "unlimited"},
		// the imported snapshots carry no host, the profile would ask for this machine's
		{"forget", "-f", profile, "--host", ""},
	} {
		code, out := runCapture(t, append([]string{"--json"}, append(args, "--dry-run")...)...)
		var res struct {
			Keep   []struct{ ID string }
			Remove []struct {
				CreatedAt string `json:"created_at"`
			}
		}
		if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil {
			t.Fatalf("%v: exit %d, %v: %s", args, code, err, out)
		}
		// the newest of every year and the oldest of the series stay
		if len(res.Keep) != 4 || len(res.Remove) != 1 || !strings.HasPrefix(res.Remove[0].CreatedAt, "2024-03-01") {
			t.Errorf("%v keeps %v and removes %v, want all but the older snapshot of 2024", args, res.Keep, res.Remove)
		}
	}

	if code, out := runCapture(t, "profile", "show", "-f", profile); code != 0 ||
		!regexp.MustCompile(`forget\.keep-yearly\s+unlimited\s`).MatchString(out) {
		t.Errorf("profile show: exit %d, no unlimited keep-yearly: %s", code, out)
	}

	negative := writeProfile(t, t.TempDir(), "neg", "repo: "+repo+"\nforget:\n  keep-yearly: -1\n")
	for want, args := range map[string][]string{
		"--keep-daily":       {"-r", repo, "forget", "--keep-daily", "-1"},
		"forget.keep-yearly": {"forget", "-f", negative, "--host", ""},
	} {
		code, _, stderr := runCaptureAll(t, args...)
		if code != 2 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "unlimited") {
			t.Errorf("%v: exit %d, stderr %q, want exit 2 naming %s and unlimited", args, code, stderr, want)
		}
	}
}
