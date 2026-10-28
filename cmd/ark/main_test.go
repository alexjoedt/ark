package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arklocal"
	dockerclient "github.com/moby/moby/client"
)

// TestMain runs every test without a password: argon2id costs a noticeable
// fraction of a second per run. Password tests clear the variable. Stdin is
// not a terminal, so no test ever prompts.
func TestMain(m *testing.M) {
	os.Setenv("ARK_INSECURE_NO_PASSWORD", "1")
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := run(append([]string{"ark"}, args...))
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
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
		{"unknown subcommand flag", []string{"-r", repo, "snapshots", "--bogus"}, 2, "usage", nil},
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

	// the parent is resolved by source name, --name changes that name
	second := backup(src)
	if second.ParentID != first.ID {
		t.Fatalf("parent %q, want %q", second.ParentID, first.ID)
	}
	named := backup("--name", "app", src)
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

	// corrupt every chunk
	chunks, _ := filepath.Glob(filepath.Join(repo, "chunks", "*", "*", "*"))
	if len(chunks) == 0 {
		t.Fatal("no chunks on disk")
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
	old := os.Stdin
	os.Stdin = f
	code, out = runCapture(t, "--json", "-r", repo, "import", "-")
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
	// two sources, three snapshots of the first
	for _, name := range []string{"one", "one", "one", "two"} {
		if code, out := runCapture(t, "-r", repo, "backup", "--name", name, "-t", "n", src); code != 0 {
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
	if counts["one"] != 2 || counts["two"] != 1 || len(list) != 3 {
		t.Fatalf("got %v", counts)
	}
	code, out = runCapture(t, "--json", "-r", repo, "snapshots", "-N", "two", "-t", "n", "-t", "missing")
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

func TestForgetAndPrune(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("y", 8192)), 0o644); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, name := range []string{"one", "one", "one", "two"} {
		code, out := runCapture(t, "--json", "-r", repo, "backup", "--name", name, src)
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
		Keep   []struct{ ID string }
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
	forget(2, ids[0], "--keep-last", "1")
	// prompt without a terminal and without --yes is exit 2 as well
	forget(2, ids[0])

	// dry run keeps everything: --keep-last per source name
	r := forget(0, "--keep-last", "1", "--dry-run")
	if !r.DryRun || len(r.Remove) != 2 || len(r.Keep) != 2 {
		t.Fatalf("got %d remove, %d keep, dry %v", len(r.Remove), len(r.Keep), r.DryRun)
	}
	if code, out := runCapture(t, "--json", "-r", repo, "snapshots"); code != 0 || strings.Count(out, `"id"`) != 4 {
		t.Fatalf("dry run deleted something: %s", out)
	}

	// a 30d window keeps fresh snapshots, --all with a filter removes them
	if r := forget(0, "--keep-within", "30d", "--dry-run"); len(r.Remove) != 0 {
		t.Fatalf("keep-within removed %d", len(r.Remove))
	}
	if r := forget(0, "--all", "-N", "two", "--yes"); len(r.Remove) != 1 || r.Remove[0].ID != ids[3] {
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
		strings.NewReader(body))
}

func TestRestoreRefusesVolumeSnapshot(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")

	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(context.Background(), b, ark.WithoutPassword())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Backup(context.Background(), volumeSource{})
	r.Close()
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	code, out := runCapture(t, "--json", "-r", repo, "restore", snap.ID, dest)
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
	code, out := runCapture(t, "--json", "-r", repo, "docker", "backup", "-t", "n", vol)
	if code != 0 {
		t.Fatalf("docker backup exit %d: %s", code, out)
	}
	var snap struct {
		ID         string `json:"id"`
		SourceName string `json:"source_name"`
		SourceType string `json:"source_type"`
	}
	if err := json.Unmarshal(
		[]byte(out),
		&snap,
	); err != nil || snap.SourceType != "docker-volume" ||
		snap.SourceName != vol {
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
		{[]string{"snapshots", "--volume", "x"}, "--name"},
		{[]string{"forget", "--volume", "x"}, "--name"},
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
