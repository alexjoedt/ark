package main

import (
	"archive/tar"
	"encoding/json"
	"maps"
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
// the creation time, an RFC 3339 string, and the tags.
func importEmptySnapshot(t *testing.T, repo, createdAt string, tags ...string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"version": 3, "created_at": createdAt, "source_name": "app", "source_type": "file", "tags": tags,
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

// --keep-tag keeps by tag, from flags and from a profile: a comma list asks
// for all of its tags, a second list widens the rule, an empty value is no
// tag. Alone it is a policy, and one that keeps nothing of the series fails
// and removes nothing.
func TestForgetKeepTag(t *testing.T) {
	repo := initRepo(t)
	importEmptySnapshot(t, repo, "2026-01-01T12:00:00Z", "a", "b")
	importEmptySnapshot(t, repo, "2026-01-02T12:00:00Z")
	importEmptySnapshot(t, repo, "2026-01-03T12:00:00Z", "b")
	importEmptySnapshot(t, repo, "2026-01-04T12:00:00Z", "a")
	profile := writeProfile(t, t.TempDir(), "app", "repo: "+repo+"\nforget:\n  keep-tag: [\"a, b\", \"\"]\n")

	const a, b, ab, none, last = "has tags [a]", "has tags [b]", "has tags [a, b]", "has tags []", "last snapshot"
	tests := []struct {
		args []string
		want map[string][]string // day of January 2026 -> reasons
	}{
		{[]string{"-r", repo, "forget", "--keep-tag", "a"}, map[string][]string{"04": {a}, "01": {a}}},
		{[]string{"-r", repo, "forget", "--keep-tag", "a,b"}, map[string][]string{"01": {ab}}},
		{[]string{"-r", repo, "forget", "--keep-tag", " a , b "}, map[string][]string{"01": {ab}}},
		{
			[]string{"-r", repo, "forget", "--keep-tag", "a", "--keep-tag", "b"},
			map[string][]string{"04": {a}, "03": {b}, "01": {a, b}},
		},
		{[]string{"-r", repo, "forget", "--keep-tag", ""}, map[string][]string{"02": {none}}},
		{
			[]string{"-r", repo, "forget", "--keep-tag", "b", "--keep-last", "1"},
			map[string][]string{"04": {last}, "03": {b}, "01": {b}},
		},
		// the imported snapshots carry no host, the profile would ask for this machine's
		{[]string{"forget", "-f", profile, "--host", ""}, map[string][]string{"02": {none}, "01": {ab}}},
		{[]string{"forget", "-f", profile, "--host", "", "--keep-tag", "b"}, map[string][]string{"03": {b}, "01": {b}}},
	}
	for _, tc := range tests {
		code, out := runCapture(t, append([]string{"--json"}, append(tc.args, "--dry-run")...)...)
		var res struct {
			Keep []struct {
				CreatedAt string `json:"created_at"`
				Reasons   []string
			}
			Remove []struct{ ID string }
		}
		if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil {
			t.Fatalf("%v: exit %d, %v: %s", tc.args, code, err, out)
		}
		got := map[string][]string{}
		for _, k := range res.Keep {
			got[k.CreatedAt[len("2026-01-"):len("2026-01-01")]] = k.Reasons
		}
		if !maps.EqualFunc(got, tc.want, slices.Equal) || len(res.Keep)+len(res.Remove) != 4 {
			t.Errorf("%v keeps %v and removes %d, want %v", tc.args, got, len(res.Remove), tc.want)
		}
	}

	if code, out := runCapture(t, "profile", "show", "-f", profile); code != 0 ||
		!regexp.MustCompile(`forget\.keep-tag\s+"a, b", ""\s+profile`).MatchString(out) {
		t.Errorf("profile show: exit %d, no keep-tag lists: %s", code, out)
	}

	for _, args := range [][]string{
		{"-r", repo, "forget", "--keep-tag", "zz", "--dry-run"},
		{"-r", repo, "forget", "--keep-tag", "zz", "--yes"},
		{"-r", repo, "forget", "--keep-tag", "a,zz", "--yes"},
	} {
		code, out, stderr := runCaptureAll(t, args...)
		if code != 1 || !strings.Contains(stderr, "keeps no snapshot of a series") || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q, want exit 1 for a series kept empty", args, code, out, stderr)
		}
	}
	if list := snapshotsJSON(t, "-r", repo); len(list) != 4 {
		t.Errorf("%d snapshots after the refused runs, want 4", len(list))
	}

	// a stray comma would make a list no snapshot matches: refused, not run
	for _, args := range [][]string{
		{"-r", repo, "forget", "--keep-last", "1", "--keep-tag", "a,", "--yes"},
		{"-r", repo, "forget", "--keep-last", "1", "--keep-tag", "a,,b", "--dry-run"},
	} {
		if code, _, stderr := runCaptureAll(t, args...); code != 2 || !strings.Contains(stderr, "stray comma") {
			t.Errorf("%v: exit %d, stderr %q, want exit 2 naming the stray comma", args, code, stderr)
		}
	}
	if list := snapshotsJSON(t, "-r", repo); len(list) != 4 {
		t.Errorf("%d snapshots after the refused lists, want 4", len(list))
	}

	// a tag list is a policy, so it does not go with ids
	if code, _ := runCapture(t, "-r", repo, "forget", "--keep-tag", "a", "0123abcd"); code != 2 {
		t.Errorf("forget --keep-tag with an id: exit %d, want 2", code)
	}
}
