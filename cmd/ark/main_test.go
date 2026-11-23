package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arklocal"
	dockerclient "github.com/moby/moby/client"
)

// envCLIChild makes the test binary run the CLI with its own arguments and
// exit, for a test that needs a process of its own, as one under another TZ.
const envCLIChild = "ARK_TEST_CLI_CHILD"

// TestMain runs every test without a password: argon2id costs a noticeable
// fraction of a second per run. Password tests clear the variable. Stdin is
// not a terminal, so no test ever prompts.
func TestMain(m *testing.M) {
	os.Setenv("ARK_INSECURE_NO_PASSWORD", "1")
	os.Unsetenv("ARK_CPUPROFILE")
	os.Unsetenv("ARK_MEMPROFILE")
	if os.Getenv(envCLIChild) != "" {
		os.Exit(run(os.Args))
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		panic(err)
	}
	os.Stdin = devnull
	os.Exit(m.Run())
}

// runCapture runs the CLI and returns the exit code and what it wrote to stdout.
func runCapture(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var code int
	out := capture(t, &os.Stdout, func() { code = run(append([]string{"ark"}, args...)) })
	return code, out
}

// capture points *f at a pipe while fn runs and returns what was written to
// it. The pipe is drained while fn runs, so output beyond the pipe buffer
// does not block the writer.
func capture(t *testing.T, f **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	old := *f
	*f = w
	fn()
	*f = old
	w.Close()
	return string(<-done)
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if code, _ := runCapture(t, "-r", dir, "init"); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	return dir
}

// initProtected creates a repository with password pw and leaves the
// environment as TestMain set it.
func initProtected(t *testing.T, pw string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")
	t.Setenv("ARK_PASSWORD", pw)
	code, _ := runCapture(t, "-r", dir, "init")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "1")
	t.Setenv("ARK_PASSWORD", "")
	if code != 0 {
		t.Fatalf("init exit %d", code)
	}
	return dir
}

func TestRunExitCodes(t *testing.T) {
	repo := initRepo(t)
	protected := initProtected(t, "right")
	t.Setenv("ARK_REPOSITORY", "")
	noInsecure := []string{"ARK_INSECURE_NO_PASSWORD", ""}

	tests := []struct {
		name string
		args []string
		code int
		kind string
		env  []string
	}{
		{"no repo", []string{"snapshots"}, 2, "usage", nil},
		{"unknown flag", []string{"--bogus", "snapshots"}, 2, "usage", nil},
		{"serve is gone", []string{"serve"}, 2, "usage", nil},
		{"unknown subcommand flag", []string{"-r", repo, "snapshots", "--bogus"}, 2, "usage", nil},
		{"backup --name is gone", []string{"-r", repo, "backup", "--name", "x", "."}, 2, "usage", nil},
		{"snapshots --name is gone", []string{"-r", repo, "snapshots", "--name", "x"}, 2, "usage", nil},
		{"find -N is gone", []string{"-r", repo, "find", "-N", "x", "*"}, 2, "usage", nil},
		{"forget -N is gone", []string{"-r", repo, "forget", "-N", "x", "--all"}, 2, "usage", nil},
		{"verbose and quiet", []string{"-v", "-q", "-r", repo, "snapshots"}, 2, "usage", nil},
		{"missing argument", []string{"-r", repo, "inspect"}, 2, "usage", nil},
		{"init twice", []string{"-r", repo, "init"}, 1, "error", nil},
		{"unknown id", []string{"-r", repo, "inspect", "nope"}, 4, "not_found", nil},
		{"ok", []string{"-r", repo, "snapshots"}, 0, "", nil},
		{
			"wrong password",
			[]string{"-r", protected, "snapshots"},
			6,
			"wrong_password",
			append(noInsecure, "ARK_PASSWORD", "wrong"),
		},
		{"password required", []string{"-r", protected, "snapshots"}, 2, "usage", nil},
		{
			"not password protected",
			[]string{"-r", repo, "snapshots"},
			2,
			"usage",
			append(noInsecure, "ARK_PASSWORD", "right"),
		},
		{"no password source", []string{"-r", protected, "snapshots"}, 2, "usage", noInsecure},
		{
			"insecure and password",
			[]string{"-r", protected, "snapshots"},
			2,
			"usage",
			[]string{"ARK_PASSWORD", "right"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < len(tt.env); i += 2 {
				t.Setenv(tt.env[i], tt.env[i+1])
			}
			code, _ := runCapture(t, tt.args...)
			if code != tt.code {
				t.Fatalf("exit %d, want %d", code, tt.code)
			}
			if tt.kind == "" {
				return
			}
			code, out := runCapture(t, append([]string{"--json"}, tt.args...)...)
			if code != tt.code {
				t.Fatalf("json exit %d, want %d", code, tt.code)
			}
			var shape struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal([]byte(out), &shape); err != nil {
				t.Fatalf("stdout is not the error shape: %q", out)
			}
			if shape.Error.Code != tt.kind || shape.Error.Message == "" {
				t.Fatalf("got %+v, want code %q", shape.Error, tt.kind)
			}
		})
	}
}

func TestEnvFileNeverOverridesEnvironment(t *testing.T) {
	repo := initRepo(t)
	envFile := filepath.Join(t.TempDir(), "ark.env")
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(envFile, []byte("ARK_REPOSITORY="+other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARK_REPOSITORY", repo)
	if code, _ := runCapture(t, "--env-file", envFile, "snapshots"); code != 0 {
		t.Fatalf("environment lost to --env-file, exit %d", code)
	}

	// Without the variable the file supplies it, pointing at an uninitialised path.
	os.Unsetenv("ARK_REPOSITORY")
	code, _ := runCapture(t, "--json", "--env-file", envFile, "snapshots")
	if code != 1 {
		t.Fatalf("expected the env-file repository to be used and fail with 1, got %d", code)
	}

	// The flag beats both.
	code, _ = runCapture(t, "-r", repo, "--env-file", envFile, "snapshots")
	if code != 0 {
		t.Fatalf("flag lost to --env-file, exit %d", code)
	}
}

func TestErrorGoesToStderrWithoutJSON(t *testing.T) {
	_, out := runCapture(t, "-r", filepath.Join(t.TempDir(), "x"), "inspect", "nope")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout should be empty without --json, got %q", out)
	}
}

func TestInitChunkerFlags(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	repo := filepath.Join(t.TempDir(), "new", "repo")
	code, out := runCapture(t, "--json", "-r", repo, "init", "--chunk-min", "128KiB", "--chunk-max", "2M")
	if code != 0 {
		t.Fatalf("init exit %d: %s", code, out)
	}
	var shape struct {
		Repository string
		Chunker    struct{ Min, Avg, Max int }
	}
	if err := json.Unmarshal([]byte(out), &shape); err != nil {
		t.Fatalf("stdout is not the init shape: %q", out)
	}
	if shape.Repository != repo || shape.Chunker.Min != 128<<10 || shape.Chunker.Avg != 1<<20 ||
		shape.Chunker.Max != 2<<20 {
		t.Fatalf("got %+v", shape)
	}

	for _, args := range [][]string{
		{"--chunk-min", "2MiB"},
		{"--chunk-avg", "8MiB"},
		{"--chunk-avg", "3MiB", "--chunk-max", "8MiB"},
		{"--chunk-max", "128MiB"},
		{"--chunk-max", "lots"},
		{filepath.Join(t.TempDir(), "positional")},
	} {
		other := filepath.Join(t.TempDir(), "repo")
		code, _ := runCapture(t, append([]string{"-r", other, "init"}, args...)...)
		if code != 2 {
			t.Errorf("init %v: exit %d, want 2", args, code)
		}
	}
}

func TestBackupDirectory(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	type snapshot struct {
		ID         string   `json:"id"`
		SourceName string   `json:"source_name"`
		SourceType string   `json:"source_type"`
		Host       string   `json:"host"`
		Tags       []string `json:"tags"`
		Comment    string   `json:"comment"`
		ParentID   string   `json:"parent_id"`
		Stats      struct {
			TotalFiles int `json:"total_files"`
		} `json:"stats"`
	}
	backup := func(args ...string) snapshot {
		t.Helper()
		code, out := runCapture(t, append([]string{"--json", "-r", repo, "backup"}, args...)...)
		if code != 0 {
			t.Fatalf("backup exit %d: %s", code, out)
		}
		var s snapshot
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatalf("stdout is not a snapshot: %q", out)
		}
		return s
	}

	first := backup("-t", "nightly", "-t", "x", "-c", "note", "--host", "web1", src)
	if first.SourceName != src || first.SourceType != "file" || first.Host != "web1" ||
		first.Comment != "note" || len(first.Tags) != 2 || first.ParentID != "" || first.Stats.TotalFiles != 1 {
		t.Fatalf("got %+v", first)
	}

	// the parent is resolved by source type, name and host, --source changes it
	second := backup("--host", "web1", src)
	if second.ParentID != first.ID {
		t.Fatalf("parent %q, want %q", second.ParentID, first.ID)
	}
	if web2 := backup("--host", "web2", src); web2.ParentID != "" {
		t.Fatalf("host web2 took parent %q from web1", web2.ParentID)
	}
	named := backup("--source", "app", src)
	if named.SourceName != "app" || named.ParentID != "" {
		t.Fatalf("got %+v", named)
	}
	explicit := backup("--parent", first.ID[:20], src)
	if explicit.ParentID != first.ID {
		t.Fatalf("parent %q, want %q", explicit.ParentID, first.ID)
	}

	missing := filepath.Join(t.TempDir(), "nope")
	code, out := runCapture(t, "--json", "-r", repo, "backup", missing)
	if code != 1 || !strings.Contains(out, missing) {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

// TestCorruptManifestVerifyExit5AndForget: verify reports a corrupt manifest
// as a finding, and forget by id removes it.
func TestCorruptManifestVerifyExit5AndForget(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("contents"), 0o644); err != nil {
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
	manifests, _ := filepath.Glob(filepath.Join(repo, "snapshots", "*"+snap.ID+"*"))
	if len(manifests) != 1 {
		t.Fatalf("found %d manifest files for %s", len(manifests), snap.ID)
	}
	if err := os.WriteFile(manifests[0], []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _ := runCapture(t, "-r", repo, "verify", snap.ID); code != 5 {
		t.Fatalf("verify over a corrupt manifest: exit %d, want 5", code)
	}
	if code, out := runCapture(
		t,
		"-r",
		repo,
		"forget",
		"--yes",
		snap.ID,
	); code != 0 ||
		!strings.Contains(out, "corrupt manifest") {
		t.Fatalf("forget: exit %d, stdout %q", code, out)
	}
	if code, out := runCapture(t, "--json", "-r", repo, "snapshots"); code != 0 {
		t.Fatalf("snapshots after forget: exit %d, stdout %q", code, out)
	}
}

func TestVerifyAndCheckExit5(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
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

	for _, args := range [][]string{{"verify", snap.ID}, {"check"}} {
		if code, _ := runCapture(t, append([]string{"-r", repo}, args...)...); code != 0 {
			t.Fatalf("%v on a clean repo: exit %d", args, code)
		}
	}
	if code, _ := runCapture(t, "-r", repo, "verify", "nope"); code != 4 {
		t.Fatalf("verify unknown id: exit %d, want 4", code)
	}

	// corrupt every chunk: a data pack cut short ends before its chunks do
	chunks, _ := filepath.Glob(filepath.Join(repo, "datapacks", "*", "*"))
	if len(chunks) == 0 {
		t.Fatal("no data packs on disk")
	}
	for _, c := range chunks {
		if err := os.WriteFile(c, []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, out = runCapture(t, "-r", repo, "verify", snap.ID)
	if code != 5 || out != "" {
		t.Fatalf("verify: exit %d, stdout %q", code, out)
	}
	code, out = runCapture(t, "--json", "-r", repo, "verify", snap.ID)
	var vr struct {
		SnapshotID    string   `json:"snapshot_id"`
		CorruptChunks []string `json:"corrupt_chunks"`
		MissingChunks []string `json:"missing_chunks"`
		OK            bool     `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &vr); err != nil || code != 5 {
		t.Fatalf("verify --json: exit %d, stdout %q", code, out)
	}
	if vr.OK || vr.SnapshotID != snap.ID || len(vr.CorruptChunks) == 0 || vr.MissingChunks == nil {
		t.Fatalf("got %+v", vr)
	}

	code, out = runCapture(t, "--json", "-r", repo, "check")
	var cr struct {
		SnapshotsChecked int      `json:"snapshots_checked"`
		Errors           []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil || code != 5 {
		t.Fatalf("check --json: exit %d, stdout %q", code, out)
	}
	if cr.SnapshotsChecked != 1 || len(cr.Errors) == 0 {
		t.Fatalf("got %+v", cr)
	}
}

// TestCheckPrintsTheMvThatRestoresAnAsideFile plants what a failed Put over
// an object leaves without posix-rename and pins the restoring mv.
func TestCheckPrintsTheMvThatRestoresAnAsideFile(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "keys", ".ark-old-k1"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, logs := runCaptureAll(t, "-r", repo, "check")
	if code != 0 || !strings.Contains(logs, "mv keys/.ark-old-k1 keys/k1") {
		t.Fatalf("check: exit %d, stdout %q, stderr %q", code, out, logs)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
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

	// export to stdout, then feed that stream to import on stdin; import keeps the id
	code, tarball := runCapture(t, "-r", repo, "export", snap.ID)
	if code != 0 || !strings.Contains(tarball, "metadata.json") {
		t.Fatalf("export: exit %d, %d bytes", code, len(tarball))
	}
	stdin := filepath.Join(t.TempDir(), "in.tar")
	if err := os.WriteFile(stdin, []byte(tarball), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	target := initRepo(t)
	old := os.Stdin
	os.Stdin = f
	code, out = runCapture(t, "--json", "-r", target, "import", "-")
	os.Stdin = old
	if code != 0 {
		t.Fatalf("import exit %d: %s", code, out)
	}
	var imported struct {
		ID         string `json:"id"`
		SourceName string `json:"source_name"`
		Stats      struct {
			TotalFiles int `json:"total_files"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(out), &imported); err != nil {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	if imported.ID != snap.ID || imported.SourceName != src || imported.Stats.TotalFiles != 1 {
		t.Fatalf("got %+v", imported)
	}
	if code, out := runCapture(t, "-r", target, "import", stdin); code == 0 {
		t.Fatalf("a second import of the snapshot succeeded: %q", out)
	}

	// export to a file keeps stdout for --json
	file := filepath.Join(t.TempDir(), "x.tar")
	code, out = runCapture(t, "--json", "-r", repo, "export", snap.ID, file)
	if code != 0 || !strings.Contains(out, `"file"`) {
		t.Fatalf("export to file: exit %d, %q", code, out)
	}
	if fi, err := os.Stat(file); err != nil || fi.Size() != int64(len(tarball)) {
		t.Fatalf("file %v, size %d, want %d", err, fi.Size(), len(tarball))
	}
	if code, _ := runCapture(t, "-r", repo, "export", "nope", file); code != 4 {
		t.Fatalf("export unknown id: exit %d, want 4", code)
	}
}

func TestUnlockShape(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	code, out := runCapture(t, "--json", "-r", repo, "unlock")
	var shape struct {
		Removed int
		Locks   []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(out), &shape); err != nil || code != 0 {
		t.Fatalf("unlock: exit %d, %q", code, out)
	}
	if shape.Removed != 0 || shape.Locks == nil {
		t.Fatalf("got %+v", shape)
	}
}

func TestSnapshotsLastAndLsPrefix(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	for _, p := range []string{"app/a.txt", "apple/b.txt", "app/sub/c.txt"} {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// two sources, three snapshots of the first, and the first once more from
	// another host: three series
	for _, args := range [][]string{{"one"}, {"one"}, {"one"}, {"two"}, {"one", "--host", "elsewhere"}} {
		args = append([]string{"-r", repo, "backup", "-t", "n", "--source"}, append(args, src)...)
		if code, out := runCapture(t, args...); code != 0 {
			t.Fatalf("backup exit %d: %s", code, out)
		}
	}

	code, out := runCapture(t, "--json", "-r", repo, "snapshots", "--last", "2")
	var list []struct {
		ID         string `json:"id"`
		SourceName string `json:"source_name"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 {
		t.Fatalf("snapshots --last: exit %d, %q", code, out)
	}
	counts := map[string]int{}
	for _, s := range list {
		counts[s.SourceName]++
	}
	if counts["one"] != 3 || counts["two"] != 1 || len(list) != 4 {
		t.Fatalf("got %v, want two of one, one of two and one of the other host", counts)
	}
	code, out = runCapture(t, "--json", "-r", repo, "snapshots", "-s", "two", "-t", "n", "-t", "missing")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("tag filter: exit %d, %q", code, out)
	}

	code, out = runCapture(t, "--json", "-r", repo, "ls", list[0].ID, "app")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, out)
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e struct {
			Path string `json:"path"`
			Mode int    `json:"mode"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not one object per line: %q", line)
		}
		if strings.HasPrefix(e.Path, "apple") {
			t.Fatalf("prefix app matched %q", e.Path)
		}
		paths = append(paths, e.Path)
	}
	if len(paths) != 4 || paths[0] != "app/" {
		t.Fatalf("got %v", paths)
	}
	if code, _ := runCapture(t, "-r", repo, "ls", "nope"); code != 4 {
		t.Fatalf("ls unknown id: exit %d, want 4", code)
	}
	if code, out := runCapture(t, "-r", repo, "inspect", list[0].ID); code != 0 || !strings.Contains(out, "Source:") {
		t.Fatalf("inspect: exit %d, %q", code, out)
	}
}

// --last counts per series: each of source type, source name and host opens a
// series of its own, the key the library's parent and forget policy use.
func TestLastPerSeries(t *testing.T) {
	snap := func(id string, typ ark.SourceType, name, host string) *ark.Snapshot {
		return &ark.Snapshot{ID: ark.SnapshotID(id), SourceType: typ, SourceName: name, Host: host}
	}
	got := lastPerSeries([]*ark.Snapshot{
		snap("1", "file", "db", "a"),
		snap("2", "docker-volume", "db", "a"),
		snap("3", "file", "db", "b"),
		snap("4", "file", "app", "a"),
		snap("5", "docker-volume", "db", ""),
		snap("6", "file", "db", "a"),
		snap("7", "docker-volume", "db", "a"),
	}, 1)
	var ids []string
	for _, s := range got {
		ids = append(ids, string(s.ID))
	}
	if want := []string{"1", "2", "3", "4", "5"}; !slices.Equal(ids, want) {
		t.Errorf("kept %v, want the newest of each series %v", ids, want)
	}
}

func TestForgetAndPrune(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("y", 8192)), 0o644); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, name := range []string{"one", "one", "one", "two"} {
		code, out := runCapture(t, "--json", "-r", repo, "backup", "--source", name, src)
		if code != 0 {
			t.Fatalf("backup exit %d: %s", code, out)
		}
		var s struct{ ID string }
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}

	type result struct {
		Keep []struct {
			ID      string
			Reasons []string
		}
		Remove []struct{ ID string }
		DryRun bool `json:"dry_run"`
	}
	forget := func(want int, args ...string) result {
		t.Helper()
		code, out := runCapture(t, append([]string{"--json", "-r", repo, "forget"}, args...)...)
		if code != want {
			t.Fatalf("forget %v: exit %d, want %d: %s", args, code, want, out)
		}
		var r result
		if want == 0 {
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatalf("not the forget shape: %q", out)
			}
		}
		return r
	}

	// an empty policy without ids or --all is exit 2, a bad duration too
	forget(2)
	forget(2, "--keep-within", "soon")
	// a Go duration is none: m counts months, and there are no minutes
	forget(2, "--keep-within", "90m")
	forget(2, "--keep-within", "1h30m")
	forget(2, ids[0], "--keep-last", "1")
	// prompt without a terminal and without --yes is exit 2 as well
	forget(2, ids[0])

	// dry run keeps everything: --keep-last per series
	r := forget(0, "--keep-last", "1", "--dry-run")
	if !r.DryRun || len(r.Remove) != 2 || len(r.Keep) != 2 {
		t.Fatalf("got %d remove, %d keep, dry %v", len(r.Remove), len(r.Keep), r.DryRun)
	}
	if code, out := runCapture(t, "--json", "-r", repo, "snapshots"); code != 0 || strings.Count(out, `"id"`) != 4 {
		t.Fatalf("dry run deleted something: %s", out)
	}

	// a window keeps fresh snapshots and the reason names it as given,
	// --all with a filter removes them
	for _, within := range []string{"30d", "72h", "1y6m"} {
		r = forget(0, "--keep-within", within, "--dry-run")
		if len(r.Remove) != 0 {
			t.Fatalf("keep-within %s removed %d", within, len(r.Remove))
		}
		for _, k := range r.Keep {
			if !slices.Equal(k.Reasons, []string{"within " + within}) {
				t.Fatalf("%s kept for %q, want within %s", k.ID, k.Reasons, within)
			}
		}
	}
	r = forget(0, "--all", "-s", "two", "--yes")
	if len(r.Remove) != 1 || r.Remove[0].ID != ids[3] || len(r.Keep) != 0 {
		t.Fatalf("got %+v", r)
	}
	if r := forget(0, "--keep-last", "1", "--yes"); len(r.Remove) != 2 {
		t.Fatalf("got %+v", r)
	}

	code, out := runCapture(t, "--json", "-r", repo, "prune", "--yes")
	var pr struct {
		BlobsRemoved int   `json:"blobs_removed"`
		BytesFreed   int64 `json:"bytes_freed"`
		DryRun       bool  `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil || code != 0 {
		t.Fatalf("prune: exit %d, %q", code, out)
	}
	if pr.DryRun {
		t.Fatalf("got %+v", pr)
	}
	if code, _ := runCapture(t, "-r", repo, "prune"); code != 2 {
		t.Fatalf("prune without --yes and no terminal: exit %d, want 2", code)
	}
}

func TestRestoreDirectory(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	for name, body := range map[string]string{"a.txt": "hello", "sub/b.txt": "world"} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, out := runCapture(t, "--json", "-r", repo, "backup", src)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}

	// dry run lists, honours --path, writes nothing
	dest := filepath.Join(t.TempDir(), "out")
	code, out = runCapture(t, "-r", repo, "restore", "-n", "-p", "/sub/", snap.ID[:12], dest)
	if code != 0 || strings.TrimSpace(out) != "sub/\nsub/b.txt" {
		t.Fatalf("dry run: exit %d, stdout %q", code, out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run created %s", dest)
	}

	// real restore creates dest and overwrites what is there
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "a.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runCapture(t, "--json", "-r", repo, "restore", "--cache", "0", snap.ID, dest)
	if code != 0 {
		t.Fatalf("restore exit %d: %s", code, out)
	}
	var res struct {
		SnapshotID    string `json:"snapshot_id"`
		FilesRestored int    `json:"files_restored"`
		BytesRestored int64  `json:"bytes_restored"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not the restore shape: %q", out)
	}
	if res.SnapshotID != snap.ID || res.FilesRestored != 2 || res.BytesRestored != 10 {
		t.Fatalf("got %+v", res)
	}
	for name, body := range map[string]string{"a.txt": "hello", "sub/b.txt": "world"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != body {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
	}

	if code, _ := runCapture(t, "-r", repo, "restore", "nope", dest); code != 4 {
		t.Fatalf("unknown id: exit %d, want 4", code)
	}
	if code, _ := runCapture(t, "-r", repo, "restore", "--cache", "lots", snap.ID, dest); code != 2 {
		t.Fatalf("bad cache: exit %d, want 2", code)
	}
}

// volumeSource is the smallest docker-volume snapshot a test can make.
type volumeSource struct{}

func (volumeSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "pgdata", Type: arkdocker.SourceTypeDockerVolume}
}

func (volumeSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	body := "x"
	return fn(ark.FileInfo{Path: "pg.conf", Size: 1, Mode: 0o644, Type: ark.FileTypeRegular},
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil })
}

// volumeSnapshot writes a volumeSource snapshot into repo and returns its id.
func volumeSnapshot(t *testing.T, repo string) string {
	t.Helper()
	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(context.Background(), b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Backup(context.Background(), volumeSource{})
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(snap.ID)
}

func TestRestoreRefusesVolumeSnapshot(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	id := volumeSnapshot(t, repo)

	dest := filepath.Join(t.TempDir(), "out")
	code, out := runCapture(t, "--json", "-r", repo, "restore", id, dest)
	if code != 1 || !strings.Contains(out, "ark docker restore") || !strings.Contains(out, "tar x") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("refused restore created %s", dest)
	}
}

func TestDockerCommandsWithoutDaemon(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	// nothing listens here, so any daemon call fails fast
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
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

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"help", []string{"docker", "--help"}, 0, "volumes"},
		{"backup usage", []string{"-r", repo, "docker", "backup"}, 2, ""},
		{"backup label without =", []string{"-r", repo, "docker", "backup", "--label", "app", "vol"}, 2, ""},
		{"backup reserved label", []string{"-r", repo, "docker", "backup", "--label", "ark/path=x", "vol"}, 2, ""},
		{"restore usage", []string{"-r", repo, "docker", "restore"}, 2, ""},
		{"restore unknown id", []string{"-r", repo, "docker", "restore", "nope"}, 4, ""},
		{
			"restore file snapshot",
			[]string{"--json", "-r", repo, "docker", "restore", snap.ID, "vol"},
			1,
			"use ark restore",
		},
		{"volumes no daemon", []string{"-r", repo, "docker", "volumes"}, 1, ""},
		{"host flag no daemon", []string{"-r", repo, "docker", "--docker-host", "tcp://127.0.0.1:2", "volumes"}, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out := runCapture(t, tt.args...)
			if code != tt.code || !strings.Contains(out, tt.want) {
				t.Fatalf("exit %d, stdout %q, want %d and %q", code, out, tt.code, tt.want)
			}
		})
	}
}

// TestDockerRoundTrip needs a daemon and skips without one, like arkdocker.
func TestDockerRoundTrip(t *testing.T) {
	c, err := arkdocker.NewClient("")
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	moby, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	defer moby.Close()
	vol := fmt.Sprintf("ark-cli-test-%d", time.Now().UnixNano())
	if _, err := moby.VolumeCreate(context.Background(), dockerclient.VolumeCreateOptions{Name: vol}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	for _, v := range []string{vol, vol + "-copy"} {
		defer moby.VolumeRemove(
			context.Background(),
			v,
			dockerclient.VolumeRemoveOptions{Force: true},
		)
	}

	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	code, out := runCapture(t, "--json", "-r", repo, "docker", "backup", "-t", "n", "--label", "app=db,main", vol)
	if code != 0 {
		t.Fatalf("docker backup exit %d: %s", code, out)
	}
	var snap struct {
		ID         string            `json:"id"`
		SourceName string            `json:"source_name"`
		SourceType string            `json:"source_type"`
		Labels     map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(
		[]byte(out),
		&snap,
	); err != nil || snap.SourceType != "docker-volume" ||
		snap.SourceName != vol || snap.Labels["app"] != "db,main" {
		t.Fatalf("stdout %q, %v", out, err)
	}

	code, out = runCapture(t, "--json", "-r", repo, "docker", "volumes")
	if code != 0 {
		t.Fatalf("docker volumes exit %d: %s", code, out)
	}
	var vols []struct {
		Name         string  `json:"name"`
		LastSnapshot *string `json:"last_snapshot"`
	}
	if err := json.Unmarshal([]byte(out), &vols); err != nil {
		t.Fatalf("stdout is not a list: %q", out)
	}
	found := false
	for _, v := range vols {
		if v.Name == vol {
			found = v.LastSnapshot != nil
		}
	}
	if !found {
		t.Fatalf("%s without last snapshot in %s", vol, out)
	}

	code, out = runCapture(t, "--json", "-r", repo, "docker", "restore", "-n", snap.ID, vol+"-copy")
	if code != 0 || !strings.Contains(out, `"replaces_data": false`) {
		t.Fatalf("dry run: exit %d, stdout %q", code, out)
	}
	if code, _ := runCapture(t, "--json", "-r", repo, "docker", "restore", snap.ID, vol+"-copy"); code != 2 {
		t.Fatalf("restore without --yes under --json: exit %d, want 2", code)
	}
	code, out = runCapture(t, "--json", "-r", repo, "docker", "restore", "-y", snap.ID, vol+"-copy")
	if code != 0 || !strings.Contains(out, `"volume": "`+vol+`-copy"`) {
		t.Fatalf("restore: exit %d, stdout %q", code, out)
	}
	if _, err := moby.VolumeInspect(
		context.Background(),
		vol+"-copy",
		dockerclient.VolumeInspectOptions{},
	); err != nil {
		t.Fatalf("restored volume missing: %v", err)
	}

	// a profile names the volume, the daemon and the snapshot's settings;
	// DOCKER_HOST points nowhere, so only the profile's docker-host works
	dir := profileDirFor(t)
	marker := filepath.Join(t.TempDir(), "hooks")
	writeProfile(t, dir, "vol", "repo: "+repo+"\ndocker-host: "+moby.DaemonHost()+`
backup:
  volume: `+vol+`
  host: profile-host
  tag: [p]
  comment: from the profile
  label:
    app: db
  run-before: echo before >> `+marker+`
  run-after: echo after >> `+marker+`
`)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	code, out = runCapture(t, "--json", "docker", "backup", "-P", "vol", "--label", "env=prod")
	if code != 0 {
		t.Fatalf("docker backup -P exit %d: %s", code, out)
	}
	var ps struct {
		ID         string            `json:"id"`
		SourceName string            `json:"source_name"`
		Host       string            `json:"host"`
		Tags       []string          `json:"tags"`
		Comment    string            `json:"comment"`
		Labels     map[string]string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &ps); err != nil || ps.SourceName != vol || ps.Host != "profile-host" ||
		!slices.Equal(ps.Tags, []string{"p"}) || ps.Comment != "from the profile" ||
		ps.Labels["app"] != "db" || ps.Labels["env"] != "prod" {
		t.Fatalf("docker backup -P: stdout %q, %v", out, err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "before\nafter\n" {
		t.Fatalf("hooks wrote %q, %v", b, err)
	}
	code, out = runCapture(t, "--json", "docker", "backup", "-P", "vol", "-t", "f", "-c", "", "--host", "h2")
	if code != 0 || !strings.Contains(out, `"host": "h2"`) || !strings.Contains(out, `"tags": [`+"\n"+`    "f"`) ||
		strings.Contains(out, "from the profile") {
		t.Fatalf("flags over the profile: exit %d, stdout %q", code, out)
	}
	code, out = runCapture(t, "--json", "docker", "restore", "-P", "vol", "-n", ps.ID)
	if code != 0 || !strings.Contains(out, `"volume": "`+vol+`"`) {
		t.Fatalf("docker restore -P dry run: exit %d, stdout %q", code, out)
	}
	// a profile of another volume refuses the snapshot unless the volume is named
	writeProfile(
		t,
		dir,
		"copy",
		"repo: "+repo+"\ndocker-host: "+moby.DaemonHost()+"\nbackup:\n  volume: "+vol+"-copy\n",
	)
	if code, out = runCapture(t, "docker", "restore", "-P", "copy", "-n", ps.ID); code != 2 {
		t.Fatalf("docker restore -P of another volume: exit %d, stdout %q", code, out)
	}
	code, out = runCapture(t, "--json", "docker", "restore", "-P", "copy", "-n", ps.ID, vol+"-copy")
	if code != 0 || !strings.Contains(out, `"volume": "`+vol+`-copy"`) {
		t.Fatalf("docker restore -P into a named volume: exit %d, stdout %q", code, out)
	}
}

func TestVersionLine(t *testing.T) {
	want := "ark v0.0.0-dev (" + runtime.Version() + ", unknown, unknown)\n"
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out := runCapture(t, args...)
		if code != 0 || out != want {
			t.Fatalf("%v: exit %d, stdout %q, want %q", args, code, out, want)
		}
	}
	code, out := runCapture(t, "--json", "version")
	var v struct{ Version, Commit, Date, Go string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if v.Version != "v0.0.0-dev" || v.Go != runtime.Version() || v.Commit == "" || v.Date == "" {
		t.Fatalf("got %+v", v)
	}
}

func TestRemovedFlagsAndCommands(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")

	tests := []struct {
		args        []string
		replacement string
	}{
		{[]string{"snapshots", "--volume", "x"}, "--source"},
		{[]string{"forget", "--volume", "x"}, "--source"},
		{[]string{"forget", "--until", "1d"}, "--keep-within"},
		{[]string{"forget", "--force"}, "--yes"},
		{[]string{"prune", "--force"}, "--yes"},
		{[]string{"restore", "--force", "a", "b"}, "--yes"},
		{[]string{"restore", "--to", "v", "a", "b"}, "ark docker restore"},
		{[]string{"docker", "restore", "--force", "a"}, "--yes"},
		{[]string{"--docker-socket", "/x", "snapshots"}, "ark docker --docker-host"},
		{[]string{"--stop-timeout", "5s", "snapshots"}, "--stop-timeout"},
		{[]string{"volumes"}, "ark docker volumes"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, out := runCapture(t, append([]string{"--json", "-r", repo}, tt.args...)...)
			var shape struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal([]byte(out), &shape); err != nil || code != 2 {
				t.Fatalf("exit %d, stdout %q", code, out)
			}
			if shape.Error.Code != "usage" || !strings.Contains(shape.Error.Message, "removed") ||
				!strings.Contains(shape.Error.Message, tt.replacement) {
				t.Fatalf("got %+v, want usage naming %q", shape.Error, tt.replacement)
			}
		})
	}

	// the removed names stay out of the help
	for _, args := range [][]string{{"--help"}, {"forget", "--help"}, {"restore", "--help"}, {"snapshots", "--help"}} {
		_, out := runCapture(t, args...)
		for _, name := range []string{"--volume", "--until", "--force", "--to ", "--docker-socket", "  volumes"} {
			if strings.Contains(out, name) {
				t.Fatalf("%v help mentions %q", args, name)
			}
		}
	}
}

func TestPasswordFile(t *testing.T) {
	repo := initProtected(t, "right")
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")

	dir := t.TempDir()
	for _, tt := range []struct {
		body string
		code int
	}{
		{"right", 0},
		{"right\n", 0},
		{"right\r\n", 0},
		{"right\n\n", 6},
		{"\n", 2},
	} {
		file := filepath.Join(dir, "pw")
		if err := os.WriteFile(file, []byte(tt.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _ := runCapture(t, "-r", repo, "--password-file", file, "snapshots"); code != tt.code {
			t.Errorf("password file %q: exit %d, want %d", tt.body, code, tt.code)
		}
	}

	// the file beats the environment
	t.Setenv("ARK_PASSWORD", "wrong")
	if code, _ := runCapture(t, "-r", repo, "--password-file", filepath.Join(dir, "pw"), "snapshots"); code != 2 {
		t.Errorf("file with an empty password lost to ARK_PASSWORD, exit %d", code)
	}
}

func TestPasswordCommand(t *testing.T) {
	repo := initProtected(t, "right")
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")
	t.Setenv("ARK_PASSWORD", "wrong")
	t.Setenv("ARK_PASSWORD_FILE", "")
	t.Setenv("ARK_PASSWORD_COMMAND", "")
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("right"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		args []string
		env  []string
		code int
	}{
		{"flag", []string{"--password-command", "echo right"}, nil, 0},
		{"crlf stripped", []string{"--password-command", `printf 'right\r\n'`}, nil, 0},
		{"wrong", []string{"--password-command", "echo nope"}, nil, 6},
		{"empty output", []string{"--password-command", "true"}, nil, 2},
		{"failing command", []string{"--password-command", "exit 3"}, nil, 1},
		{"with password-file", []string{"--password-command", "echo right", "--password-file", pwFile}, nil, 2},
		{"variable", nil, []string{"ARK_PASSWORD_COMMAND", "echo right"}, 0},
		{"flag beats variable", []string{"--password-command", "echo right"}, []string{"ARK_PASSWORD_COMMAND", "echo nope"}, 0},
		{"variable with file variable", nil, []string{"ARK_PASSWORD_COMMAND", "echo right", "ARK_PASSWORD_FILE", pwFile}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < len(tt.env); i += 2 {
				t.Setenv(tt.env[i], tt.env[i+1])
			}
			args := append([]string{"-r", repo}, tt.args...)
			if code, out := runCapture(t, append(args, "snapshots")...); code != tt.code {
				t.Errorf("exit %d, want %d: %s", code, tt.code, out)
			}
		})
	}
}

func TestKeyPasswd(t *testing.T) {
	repo := initProtected(t, "old")
	t.Setenv("ARK_REPOSITORY", repo)
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")

	t.Setenv("ARK_PASSWORD", "old")
	t.Setenv("ARK_NEW_PASSWORD", "new")
	if code, out := runCapture(t, "--json", "key", "passwd"); code != 0 || !strings.Contains(out, `"password": true`) {
		t.Fatalf("passwd exit %d: %s", code, out)
	}
	if code, _ := runCapture(t, "snapshots"); code != 6 {
		t.Fatalf("old password after change: exit %d, want 6", code)
	}

	t.Setenv("ARK_PASSWORD", "new")
	if code, _ := runCapture(t, "key", "passwd", "--new-insecure-no-password"); code != 2 {
		t.Fatalf("--new-insecure-no-password with ARK_NEW_PASSWORD: exit %d, want 2", code)
	}
	t.Setenv("ARK_NEW_PASSWORD", "")
	if code, _ := runCapture(t, "key", "passwd", "--new-insecure-no-password"); code != 0 {
		t.Fatalf("remove password: exit %d", code)
	}
	t.Setenv("ARK_PASSWORD", "")
	if code, _ := runCapture(t, "--insecure-no-password", "snapshots"); code != 0 {
		t.Fatalf("open without password after removal: exit %d", code)
	}
}

func TestCPUProfileEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpu.prof")
	t.Setenv("ARK_CPUPROFILE", path)
	if code, _ := runCapture(t, "--version"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("profile not written: %v", err)
	}

	t.Setenv("ARK_CPUPROFILE", filepath.Join(t.TempDir(), "missing", "cpu.prof"))
	if code, _ := runCapture(t, "--version"); code == 0 {
		t.Fatal("unwritable profile path exited 0")
	}
}

func TestMemProfileEnv(t *testing.T) {
	dir := t.TempDir()
	mem := filepath.Join(dir, "mem.prof")
	t.Setenv("ARK_MEMPROFILE", mem)
	t.Setenv("ARK_CPUPROFILE", filepath.Join(dir, "cpu.prof"))
	if code, _ := runCapture(t, "--version"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, name := range []string{"mem.prof", "cpu.prof"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() == 0 {
			t.Fatalf("%s not written: %v", name, err)
		}
	}
	f, err := os.Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("heap profile is not a gzipped pprof: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read heap profile: %v", err)
	}
	for _, view := range []string{"alloc_space", "inuse_space"} {
		if !bytes.Contains(raw, []byte(view)) {
			t.Errorf("heap profile lacks sample type %s", view)
		}
	}

	// the profile is also written when the command fails
	failed := filepath.Join(dir, "failed.prof")
	t.Setenv("ARK_MEMPROFILE", failed)
	if code, _ := runCapture(t, "--no-such-flag"); code == 0 {
		t.Fatal("unknown flag exited 0")
	}
	if fi, err := os.Stat(failed); err != nil || fi.Size() == 0 {
		t.Fatalf("profile not written after a failure: %v", err)
	}

	t.Setenv("ARK_MEMPROFILE", filepath.Join(dir, "missing", "mem.prof"))
	if code, _ := runCapture(t, "--version"); code == 0 {
		t.Fatal("unwritable profile path exited 0")
	}

	same := filepath.Join(dir, "same.prof")
	t.Setenv("ARK_MEMPROFILE", same)
	t.Setenv("ARK_CPUPROFILE", same)
	if code, _ := runCapture(t, "--version"); code == 0 {
		t.Fatal("identical profile paths exited 0")
	}
}

func TestRepairIndex(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, "--json", "-r", repo, "backup", src)
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil || code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	repair := func(wantCode int) (res struct {
		PacksIndexed    int      `json:"packs_indexed"`
		BlobsIndexed    int      `json:"blobs_indexed"`
		UnreadablePacks []string `json:"unreadable_packs"`
	}) {
		t.Helper()
		code, out := runCapture(t, "--json", "-r", repo, "repair", "index")
		if err := json.Unmarshal([]byte(out), &res); err != nil || code != wantCode {
			t.Fatalf("repair index: exit %d, want %d: %q", code, wantCode, out)
		}
		return res
	}
	if res := repair(0); res.PacksIndexed != 2 || res.BlobsIndexed != 2 || res.UnreadablePacks == nil {
		t.Fatalf("healthy repository: %+v", res)
	}

	for _, dir := range []string{"dataindex", "treeindex"} {
		files, _ := filepath.Glob(filepath.Join(repo, dir, "*"))
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}
	}
	code, out = runCapture(t, "--json", "-r", repo, "prune", "--yes")
	if code != 5 || !strings.Contains(out, "an index rebuild restores the index") ||
		strings.Contains(out, "snapshot not found") {
		t.Fatalf("prune over a lost index: exit %d, %s", code, out)
	}
	code, out = runCapture(t, "--json", "-r", repo, "restore", snap.ID, filepath.Join(t.TempDir(), "out"))
	if code != 5 || !strings.Contains(out, `"verify_failed"`) || strings.Contains(out, "snapshot not found") {
		t.Fatalf("restore over a lost index: exit %d, %s", code, out)
	}
	code, out = runCapture(t, "--json", "-r", repo, "check")
	if code != 5 || !strings.Contains(out, "an index rebuild lists them") {
		t.Fatalf("check over a lost index: exit %d, %s", code, out)
	}
	repair(0)
	if code, out := runCapture(t, "-r", repo, "check"); code != 0 {
		t.Fatalf("check after repair: exit %d, %s", code, out)
	}

	packs, _ := filepath.Glob(filepath.Join(repo, "datapacks", "*", "*"))
	for _, p := range packs {
		if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if res := repair(5); len(res.UnreadablePacks) != len(packs) {
		t.Fatalf("damaged pack: %+v", res)
	}

	for _, args := range [][]string{{"repair"}, {"repair", "indx"}} {
		if code, out := runCapture(t, append([]string{"--json", "-r", repo}, args...)...); code != 2 ||
			!strings.Contains(out, `"usage"`) {
			t.Errorf("ark %v: exit %d, %s, want a usage error", args, code, out)
		}
	}
}

// TestSnapshotsTableShowsShortIDsAndSeconds: the table cuts the ID to 8
// characters and shows seconds; restore and inspect accept that prefix, an
// ambiguous one fails, and --json keeps the full ID.
func TestSnapshotsTableShowsShortIDsAndSeconds(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, "--json", "-r", repo, "backup", src)
	var snap struct{ ID string }
	if err := json.Unmarshal([]byte(out), &snap); err != nil || code != 0 {
		t.Fatalf("backup: exit %d, %q", code, out)
	}

	code, out = runCapture(t, "-r", repo, "snapshots")
	lines := strings.SplitN(out, "\n", 3)
	if code != 0 || len(lines) < 2 {
		t.Fatalf("snapshots: exit %d, %q", code, out)
	}
	row := strings.Fields(lines[1])
	if len(row) < 3 || row[0] != snap.ID[:8] {
		t.Fatalf("table row %q, want ID column %s", row, snap.ID[:8])
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d$`).MatchString(row[1] + " " + row[2]) {
		t.Fatalf("TIME column %q %q has no seconds", row[1], row[2])
	}
	code, out = runCapture(t, "--json", "-r", repo, "snapshots")
	if code != 0 || !strings.Contains(out, snap.ID) {
		t.Fatalf("--json lost the full ID: exit %d, %q", code, out)
	}

	short := snap.ID[:8]
	dest := filepath.Join(t.TempDir(), "out")
	if code, out := runCapture(t, "-r", repo, "restore", short, dest); code != 0 {
		t.Fatalf("restore %s: exit %d, %q", short, code, out)
	}
	if code, out := runCapture(t, "-r", repo, "inspect", short); code != 0 || !strings.Contains(out, snap.ID) {
		t.Fatalf("inspect %s: exit %d, %q", short, code, out)
	}

	// a second name with the same 8 characters makes the prefix ambiguous
	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	twin := ark.Handle{Type: ark.SnapshotFile, Name: short + strings.Repeat("0", len(snap.ID)-8)}
	if err := b.Put(context.Background(), twin, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"restore", "inspect"} {
		args := []string{"-r", repo, cmd, short}
		if cmd == "restore" {
			args = append(args, filepath.Join(t.TempDir(), "again"))
		}
		code, out := runCapture(t, append([]string{"--json"}, args...)...)
		if code != 4 || !strings.Contains(out, "ambiguous") {
			t.Errorf("%s of an ambiguous prefix: exit %d, %q", cmd, code, out)
		}
	}
}

func TestInitRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, "--json", "-r", dir, "init")
	if code != 1 || !strings.Contains(out, "exists and is not empty") {
		t.Fatalf("exit %d, out %q", code, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("dir changed: %v %v", entries, err)
	}
	if code, _ := runCapture(t, "-r", filepath.Join(dir, "new"), "init"); code != 0 {
		t.Fatalf("init into a missing dir: exit %d", code)
	}
}

// --time dates the snapshot in local time. ark snapshots shows that time,
// and the next backup still takes the newest snapshot by time as its parent.
func TestBackupTime(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	type snapshot struct {
		ID        string    `json:"id"`
		ParentID  string    `json:"parent_id"`
		CreatedAt time.Time `json:"created_at"`
	}
	backup := func(args ...string) snapshot {
		t.Helper()
		code, out := runCapture(t, append([]string{"--json", "-r", repo, "backup"}, args...)...)
		var s snapshot
		if err := json.Unmarshal([]byte(out), &s); code != 0 || err != nil {
			t.Fatalf("backup %v: exit %d, %v: %s", args, code, err, out)
		}
		return s
	}

	first := backup(src)
	dated := backup("--time", "2015-06-01 12:30:15", src)
	if want := time.Date(2015, 6, 1, 12, 30, 15, 0, time.Local); !dated.CreatedAt.Equal(want) {
		t.Errorf("--time stored %s, want %s", dated.CreatedAt, want)
	}
	if next := backup(src); next.ParentID != first.ID {
		t.Errorf("parent after a dated backup = %q, want the newest by time %q", next.ParentID, first.ID)
	}

	for _, bad := range []string{"", "yesterday", "2015-06-01T12:30:15Z", "2999-01-01 00:00:00"} {
		if code, out := runCapture(t, "--json", "-r", repo, "backup", "--time", bad, src); code != 2 {
			t.Errorf("--time %q: exit %d, want 2: %s", bad, code, out)
		}
	}
	code, out := runCapture(t, "-r", repo, "snapshots")
	if code != 0 || strings.Count(out, "2015-06-01 12:30:15") != 1 || strings.Count(out, "\n") != 4 {
		t.Errorf("snapshots: exit %d, want a header and three rows, one at 2015-06-01 12:30:15: %s", code, out)
	}
}

// Berlin skips 02:00 to 03:00 when summer time starts and shows it twice when
// it ends. A --time the clocks never show is refused rather than moved by an
// hour; one they show twice is taken.
func TestBackupTimeInAClockChange(t *testing.T) {
	repo := initRepo(t)
	src := t.TempDir()
	backup := func(at string) (int, string) {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "--json", "-r", repo, "backup", "--time", at, src)
		cmd.Env = append(os.Environ(), envCLIChild+"=1", "TZ=Europe/Berlin")
		out, err := cmd.Output()
		if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
			t.Fatalf("backup --time %q: %v", at, err)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}
	if code, out := backup("2015-03-29 02:30:00"); code != 2 || !strings.Contains(out, "does not exist") {
		t.Errorf("skipped time: exit %d, want 2 and a message that it does not exist: %s", code, out)
	}
	for _, at := range []string{"2015-03-29 01:59:59", "2015-03-29 03:00:00", "2015-10-25 02:30:00"} {
		if code, out := backup(at); code != 0 {
			t.Errorf("--time %q: exit %d, want 0: %s", at, code, out)
		}
	}
}

// ls --json reports mode_t & 07777, as the snapshot stores it: setuid,
// setgid and sticky included.
func TestLsJSONKeepsSpecialBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no setuid on windows")
	}
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	bin := filepath.Join(src, "passwd")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if code, out := runCapture(t, "-r", repo, "backup", "--source", "s", src); code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	code, out := runCapture(t, "--json", "-r", repo, "snapshots")
	var list []struct{ ID string }
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 || len(list) != 1 {
		t.Fatalf("snapshots: exit %d, %q", code, out)
	}
	code, out = runCapture(t, "--json", "-r", repo, "ls", list[0].ID)
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, out)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e struct {
			Path string `json:"path"`
			Mode int    `json:"mode"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not one object per line: %q", line)
		}
		if e.Path == "passwd" {
			if e.Mode != 0o4755 {
				t.Fatalf("mode %o, want 4755", e.Mode)
			}
			return
		}
	}
	t.Fatalf("passwd not listed: %q", out)
}
