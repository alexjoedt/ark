package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark/arkprofile"
)

// writeProfile writes body to dir/name.yaml, readable by the owner only.
func writeProfile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// profileDirFor points XDG_CONFIG_HOME at a temporary directory and returns
// the profile directory below it.
func profileDirFor(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	return filepath.Join(cfg, "ark", "profiles")
}

type profileSnapshot struct {
	ID       string   `json:"id"`
	Name     string   `json:"source_name"`
	Tags     []string `json:"tags"`
	Excludes []string `json:"excludes"`
}

func snapshotsJSON(t *testing.T, args ...string) []profileSnapshot {
	t.Helper()
	code, out := runCapture(t, append([]string{"--json"}, append(args, "snapshots")...)...)
	if code != 0 {
		t.Fatalf("snapshots exit %d: %s", code, out)
	}
	var list []profileSnapshot
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("stdout is not a snapshot list: %q", out)
	}
	return list
}

func profileBackup(t *testing.T, args ...string) profileSnapshot {
	t.Helper()
	code, out := runCapture(t, append([]string{"--json", "backup"}, args...)...)
	if code != 0 {
		t.Fatalf("backup %v exit %d: %s", args, code, out)
	}
	var s profileSnapshot
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	return s
}

func names(list []profileSnapshot) []string {
	var n []string
	for _, s := range list {
		n = append(n, s.Name)
	}
	slices.Sort(n)
	return n
}

func TestProfileBackupAndForget(t *testing.T) {
	dir := profileDirFor(t)
	shared, other := initRepo(t), initRepo(t)
	docs, photos := t.TempDir(), t.TempDir()
	for _, f := range []string{"a.txt", "skip.tmp"} {
		if err := os.WriteFile(filepath.Join(docs, f), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile(t, dir, "docs", `repo: `+shared+`
backup:
  path: `+docs+`
  tag: [docs, daily]
  exclude: ["*.tmp"]
forget:
  keep-last: 1
  prune: true
`)
	writeProfile(t, dir, "photos", "repo: "+shared+"\nbackup:\n  path: "+photos+"\n")
	// the profile's repo beats the variable
	t.Setenv("ARK_REPOSITORY", other)

	first := profileBackup(t, "-P", "docs")
	// the path names the snapshot, as a plain backup of it does
	if first.Name != docs || !slices.Equal(first.Tags, []string{"docs", "daily"}) ||
		!slices.Equal(first.Excludes, []string{"*.tmp"}) {
		t.Fatalf("snapshot %+v, want name %s, tags [docs daily], excludes [*.tmp]", first, docs)
	}
	if paths := lsPaths(t, shared, first.ID); slices.Contains(paths, "skip.tmp") {
		t.Fatalf("excluded file stored: %q", paths)
	}
	second := profileBackup(t, "-P", "docs")
	profileBackup(t, "-P", "photos")
	profileBackup(t, "-P", "photos")
	if list := snapshotsJSON(t, "-r", other); len(list) != 0 {
		t.Fatalf("ARK_REPOSITORY beat the profile's repo: %d snapshots there", len(list))
	}
	if list := snapshotsJSON(t, "-P", "docs"); len(list) != 2 {
		t.Fatalf("snapshots -P docs listed %d, want the 2 docs snapshots", len(list))
	}

	code, out := runCapture(t, "--json", "forget", "-P", "docs", "--yes")
	if code != 0 {
		t.Fatalf("forget exit %d: %s", code, out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var forget struct {
		Remove []profileSnapshot `json:"remove"`
	}
	var prune map[string]any
	if err := dec.Decode(&forget); err != nil {
		t.Fatalf("no forget result: %q", out)
	}
	if err := dec.Decode(&prune); err != nil || prune["packs_deleted"] == nil {
		t.Fatalf("forget did not prune: %q", out)
	}
	if len(forget.Remove) != 1 || forget.Remove[0].ID != first.ID {
		t.Fatalf("removed %+v, want only the older docs snapshot %s", forget.Remove, first.ID)
	}
	list := snapshotsJSON(t, "-r", shared)
	if got, want := names(list), []string{docs, photos, photos}; !slices.Equal(got, want) {
		t.Fatalf("left %q, want one docs and both photos %q", got, want)
	}
	for _, s := range list {
		if s.Name == docs && s.ID != second.ID {
			t.Fatalf("kept %s, want the newer %s", s.ID, second.ID)
		}
	}
}

func TestProfileFlagsWin(t *testing.T) {
	dir := profileDirFor(t)
	repo, other := initRepo(t), initRepo(t)
	src := t.TempDir()
	writeProfile(t, dir, "docs", "repo: "+repo+"\nbackup:\n  path: "+src+"\n  tag: [docs, daily]\n")

	if s := profileBackup(t, "-P", "docs", "-t", "cli"); !slices.Equal(s.Tags, []string{"cli"}) {
		t.Fatalf("tags %q, want the flag's [cli] alone", s.Tags)
	}
	profileBackup(t, "-r", other, "-P", "docs")
	if list := snapshotsJSON(t, "-r", other); len(list) != 1 {
		t.Fatalf("--repo lost to the profile: %d snapshots in the flag's repository", len(list))
	}
	// -P before the command works too
	if code, _ := runCapture(t, "-P", "docs", "snapshots"); code != 0 {
		t.Fatalf("ark -P docs snapshots: exit %d", code)
	}

	outside := writeProfile(t, t.TempDir(), "elsewhere", "repo: "+other+"\nbackup:\n  path: "+src+"\n")
	if s := profileBackup(t, "-f", outside); s.Name != src {
		t.Fatalf("-f snapshot named %q, want %s", s.Name, src)
	}
	if code, _ := runCapture(t, "backup", "-P", "docs", "-f", outside); code != 2 {
		t.Fatalf("-P with -f: exit %d, want 2", code)
	}
}

func TestProfilePasswordOrder(t *testing.T) {
	repo, open := initProtected(t, "right"), initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")
	dir := t.TempDir()
	pwFile := func(name, pw string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(pw), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	right, wrong := pwFile("right.pw", "right"), pwFile("wrong.pw", "wrong")
	// profiles with repository settings only: snapshots -P lists every snapshot
	head := "repo: " + repo + "\n"
	withFile := writeProfile(t, dir, "file", head+"password-file: right.pw\n")
	withEnv := writeProfile(t, dir, "env", head+"password-env: DOCS_PW\n")
	bare := writeProfile(t, dir, "bare", head)
	insecure := writeProfile(t, dir, "insecure", head+"insecure-no-password: true\n")
	t.Setenv("DOCS_PW", "right")

	for _, tt := range []struct {
		name string
		args []string
		env  []string
		code int
	}{
		{"profile file beats ARK_PASSWORD", []string{"-f", withFile}, []string{"ARK_PASSWORD", "wrong"}, 0},
		{"profile file beats ARK_PASSWORD_FILE", []string{"-f", withFile}, []string{"ARK_PASSWORD_FILE", wrong}, 0},
		{"flag beats profile", []string{"-f", withFile, "--password-file", wrong}, nil, 6},
		{"profile env beats ARK_PASSWORD_FILE", []string{"-f", withEnv}, []string{"ARK_PASSWORD_FILE", wrong}, 0},
		{"profile env unset", []string{"-f", withEnv}, []string{"DOCS_PW", ""}, 2},
		{
			"ARK_PASSWORD_FILE beats ARK_PASSWORD",
			[]string{"-f", bare},
			[]string{"ARK_PASSWORD_FILE", right, "ARK_PASSWORD", "wrong"},
			0,
		},
		{"no source", []string{"-f", bare}, nil, 2},
		// the conflicts open a repository without a password, so only the conflict fails
		{"profile insecure alone", []string{"-r", open, "-f", insecure}, nil, 0},
		{"profile insecure and ARK_PASSWORD", []string{"-r", open, "-f", insecure}, []string{"ARK_PASSWORD", "right"}, 2},
		{"flag insecure and profile file", []string{"-r", open, "-f", withFile, "--insecure-no-password"}, nil, 2},
		{"flag insecure and profile env", []string{"-r", open, "-f", withEnv, "--insecure-no-password"}, nil, 2},
		{
			"ARK_INSECURE_NO_PASSWORD and profile file",
			[]string{"-r", open, "-f", withFile},
			[]string{"ARK_INSECURE_NO_PASSWORD", "1"},
			2,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < len(tt.env); i += 2 {
				t.Setenv(tt.env[i], tt.env[i+1])
			}
			if code, _ := runCapture(t, append(tt.args, "snapshots")...); code != tt.code {
				t.Fatalf("exit %d, want %d", code, tt.code)
			}
		})
	}
}

func TestProfileErrorsExit2(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	src := t.TempDir()
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("ARK_TEST_UNSET", "")
	os.Unsetenv("ARK_TEST_UNSET")
	writable := writeProfile(t, dir, "writable", "repo: "+repo+"\n")
	if err := os.Chmod(writable, 0o620); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, profile, body string
		want                []string
	}{
		{"missing", "nope", "", []string{"nope.yaml"}},
		{"unset variable", "unset", "backup:\n  path: $ARK_TEST_UNSET\n", []string{"unset.yaml", "backup.path", "ARK_TEST_UNSET"}},
		{"unknown key", "key", "backup:\n  pahts: /x\n", []string{"key.yaml", "backup.pahts"}},
		{"unknown section", "section", "schedule: daily\n", []string{"section.yaml", "schedule"}},
		{"password", "pw", "password: secret\n", []string{"pw.yaml", "password"}},
		{"bad name", "Bad", "repo: " + repo + "\n", []string{"Bad.yaml", "name"}},
		{"group-writable", "writable", "", []string{"writable.yaml", "chmod"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--json", "backup", "-P", tt.profile}
			if tt.body != "" {
				args = []string{"--json", "backup", "-f", writeProfile(t, t.TempDir(), tt.profile, tt.body)}
			}
			code, out := runCapture(t, args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("error %s does not name %q", out, w)
				}
			}
		})
	}
	if code, _ := runCapture(t, "backup", "-P", "../x"); code != 2 {
		t.Errorf("-P ../x: exit %d, want 2", code)
	}
	// the profile's name never stands in for a source: forget refuses, snapshots
	// and find do not narrow
	held := initRepo(t)
	writeProfile(t, dir, "nosrc", "repo: "+held+"\nforget:\n  keep-last: 1\n")
	if code, out := runCapture(t, "backup", "-r", held, src); code != 0 {
		t.Fatalf("backup %s: exit %d: %s", src, code, out)
	}
	if code, out := runCapture(t, "--json", "forget", "-P", "nosrc"); code != 2 ||
		!strings.Contains(out, "backup.path, backup.source or backup.volume") {
		t.Errorf("forget without a source: exit %d, want 2 naming both keys: %s", code, out)
	}
	if list := snapshotsJSON(t, "-P", "nosrc"); len(list) == 0 {
		t.Errorf("snapshots -P nosrc listed nothing, want every snapshot")
	}
	if code, out := runCapture(t, "find", "-P", "nosrc", "x"); code != 0 {
		t.Errorf("find -P nosrc: exit %d: %s", code, out)
	}

	// a failing run-before skips the backup and runs run-after-fail
	hooks := writeProfile(t, dir, "hooks", "repo: "+repo+"\nbackup:\n  path: "+src+`
  run-before: exit 3
  run-after-fail: 'printf %s "$ARK_ERROR" > failed.txt'
`)
	var code int
	capture(t, &os.Stderr, func() { code, _ = runCapture(t, "backup", "-P", "hooks") })
	if code != 1 {
		t.Fatalf("failing run-before: exit %d, want 1", code)
	}
	f, err := os.Open(filepath.Join(filepath.Dir(hooks), "failed.txt"))
	if err != nil {
		t.Fatalf("run-after-fail did not run: %v", err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if !strings.Contains(string(b), "run-before") {
		t.Fatalf("ARK_ERROR = %q, want the run-before failure", b)
	}
	if list := snapshotsJSON(t, "-r", repo); len(list) != 0 {
		t.Fatalf("backup ran after a failing run-before: %d snapshots", len(list))
	}
}

func TestProfileReviewCases(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	docs, other := t.TempDir(), t.TempDir()
	path := writeProfile(t, dir, "docs", "repo: "+repo+"\nbackup:\n  path: "+docs+`
forget:
  keep-daily: 7
  keep-within: 30d
  run-after: touch forgot.txt
`)

	// without backup.source another directory names its own snapshots, as a
	// plain backup of it would, and stays out of the profile's scope
	if s := profileBackup(t, "-P", "docs", other); s.Name != other {
		t.Fatalf("backup -P docs %s: snapshot named %q, want the path", other, s.Name)
	}
	if list := snapshotsJSON(t, "-P", "docs"); len(list) != 0 {
		t.Fatalf("snapshots -P docs listed %+v, want none of the other path", list)
	}
	// with backup.source it would be filed under the profile's source
	writeProfile(t, dir, "named", "repo: "+repo+"\nbackup:\n  path: "+docs+"\n  source: named\n")
	if code, _ := runCapture(t, "backup", "-P", "named", other); code != 2 {
		t.Fatalf("backup -P named with another path: exit %d, want 2", code)
	}
	if s := profileBackup(t, "-P", "named", "--source", "other", other); s.Name != "other" {
		t.Fatalf("snapshot named %q, want other", s.Name)
	}
	// an empty --source keeps the profile's source scope
	if code, _ := runCapture(t, "backup", "-P", "named", "--source", "", other); code != 2 {
		t.Fatalf("backup -P named --source '' with another path: exit %d, want 2", code)
	}
	own := profileBackup(t, "-P", "docs")
	foreign := profileBackup(t, "-r", repo, "--host", "elsewhere", other)

	if code, out := runCapture(t, "--json", "snapshots", "-P", "docs", "-s", ""); code != 0 ||
		strings.Contains(out, foreign.ID) || !strings.Contains(out, own.ID) {
		t.Fatalf("snapshots -P docs -s '': exit %d, %s", code, out)
	}
	code, out := runCapture(t, "--json", "forget", "-P", "docs", "--host", "elsewhere", "-s", "", "--all", "--dry-run")
	if code != 0 || strings.Contains(out, foreign.ID) {
		t.Fatalf("forget -P docs -s '' --all: exit %d, %s", code, out)
	}

	// a dry run runs no hooks
	if code, out := runCapture(t, "forget", "-P", "docs", "--dry-run"); code != 0 {
		t.Fatalf("forget --dry-run exit %d: %s", code, out)
	}
	ran := filepath.Join(filepath.Dir(path), "forgot.txt")
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("run-after ran on a dry run")
	}
	// an empty --keep-within drops the profile's rule
	if code, out := runCapture(t, "forget", "-P", "docs", "--keep-within", "", "--dry-run"); code != 0 {
		t.Fatalf("forget --keep-within '' exit %d: %s", code, out)
	}

	// named ids ignore the profile's policy and scope
	code, out = runCapture(t, "--json", "forget", "-P", "docs", "--yes", own.ID, foreign.ID)
	if code != 0 {
		t.Fatalf("forget -P docs ids exit %d: %s", code, out)
	}
	var res struct {
		Remove []profileSnapshot `json:"remove"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Remove) != 2 {
		t.Fatalf("removed %q, want both named ids", out)
	}
	if code, _ := runCapture(t, "forget", "-P", "docs", "--keep-last", "1", own.ID); code != 2 {
		t.Fatalf("ids with --keep-last: exit %d, want 2", code)
	}

	// version reads no profile
	if code, _ := runCapture(t, "-P", "missing", "version"); code != 0 {
		t.Fatalf("version with a missing profile: exit %d, want 0", code)
	}
}

func TestProfileShowSources(t *testing.T) {
	dir := profileDirFor(t)
	t.Setenv("ARK_REPOSITORY", "/env/repo")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")
	t.Setenv("SHOW_SECRET", "hunter2-secret")
	writeProfile(t, dir, "docs", "password-env: SHOW_SECRET\nlock-wait: 10s\nbackup:\n  path: /data/\n  tag: [a, b]\n")

	type result struct {
		Profile string
		Values  []struct{ Key, Value, Source string }
	}
	show := func(args ...string) map[string][2]string {
		t.Helper()
		code, out := runCapture(t, append([]string{"--json"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, out)
		}
		if strings.Contains(out, "hunter2-secret") {
			t.Fatalf("the password leaked: %s", out)
		}
		var r result
		if err := json.Unmarshal([]byte(out), &r); err != nil || r.Profile == "" {
			t.Fatalf("not a show result: %q", out)
		}
		got := map[string][2]string{}
		for _, v := range r.Values {
			got[v.Key] = [2]string{v.Value, v.Source}
		}
		return got
	}
	want := map[string][2]string{
		"repo":          {"/env/repo", "env"},
		"concurrency":   {"3", "cli"},
		"lock-wait":     {"10s", "profile"},
		"backup.tag":    {"a, b", "profile"},
		"backup.path":   {"/data/", "profile"},
		"backup.source": {"/data", "default"},
		"password":      {"variable SHOW_SECRET", "profile"},
		"forget.prune":  {"false", "default"},
	}
	for _, args := range [][]string{
		{"profile", "show", "docs", "--concurrency", "3"},
		{"-P", "docs", "--concurrency", "3", "profile", "show"},
		{"profile", "show", "-f", filepath.Join(dir, "docs.yaml"), "--concurrency", "3"},
	} {
		got := show(args...)
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%v: %s = %v, want %v", args, k, got[k], w)
			}
		}
	}
	// every backup and forget key of a profile is shown, so one added to
	// arkprofile cannot miss it
	got := show("profile", "show", "docs")
	for prefix, st := range map[string]reflect.Type{
		"":        reflect.TypeFor[arkprofile.Profile](),
		"backup.": reflect.TypeFor[arkprofile.Backup](),
		"forget.": reflect.TypeFor[arkprofile.Forget](),
	} {
		for f := range st.Fields() {
			key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if key == "" || key == "-" || key == "backup" || key == "forget" || key == "password-file" ||
				key == "password-env" || key == "insecure-no-password" {
				continue
			}
			if _, ok := got[prefix+key]; !ok {
				t.Errorf("profile show lacks %s%s", prefix, key)
			}
		}
	}
	writeProfile(t, dir, "db", "docker-host: unix:///run/docker.sock\nbackup:\n  volume: pgdata\n  stop-timeout: 1m\n")
	vol := show("profile", "show", "db")
	for k, w := range map[string][2]string{
		"docker-host":         {"unix:///run/docker.sock", "profile"},
		"backup.volume":       {"pgdata", "profile"},
		"backup.source":       {"", "default"},
		"backup.no-stop":      {"false", "default"},
		"backup.stop-timeout": {"1m0s", "profile"},
	} {
		if vol[k] != w {
			t.Errorf("volume profile: %s = %v, want %v", k, vol[k], w)
		}
	}
	t.Setenv("ARK_PASSWORD", "x")
	t.Setenv("SHOW_SECRET", "")
	writeProfile(t, dir, "plain", "repo: /p/repo\n")
	if code, out := runCapture(t, "profile", "show", "plain"); code != 0 || !strings.Contains(out, "ARK_PASSWORD") {
		t.Errorf("env password source missing: exit %d %q", code, out)
	}
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "yes")
	code, out := runCapture(t, "profile", "show", "plain")
	if code != 0 || !strings.Contains(out, "want true or false") {
		t.Errorf("show with a bad ARK_INSECURE_NO_PASSWORD: exit %d %q, want the error backup gives", code, out)
	}
	code, _, logs := runCaptureAll(t, "profile", "show", "Docs")
	if code != 2 || !strings.Contains(logs, "profile name") || strings.Contains(logs, "--profile") {
		t.Errorf("show of an invalid name: exit %d %q, want 2 without --profile", code, logs)
	}
	if code, _ := runCapture(t, "profile", "show"); code != 2 {
		t.Errorf("show without a profile: exit %d, want 2", code)
	}
	if code, _ := runCapture(t, "profile", "show", "missing"); code == 0 {
		t.Error("show of a missing profile succeeded")
	}
}

func TestProfileList(t *testing.T) {
	dir := profileDirFor(t)
	type entry struct{ Name, Repo, Path, Volume, Error string }
	list := func(wantCode int) []entry {
		t.Helper()
		code, out := runCapture(t, "--json", "profile", "list")
		if code != wantCode {
			t.Fatalf("list exit %d, want %d: %s", code, wantCode, out)
		}
		var es []entry
		if err := json.Unmarshal([]byte(out), &es); err != nil {
			t.Fatalf("not a list: %q", out)
		}
		return es
	}
	if es := list(0); len(es) != 0 {
		t.Fatalf("no directory listed %v", es)
	}
	writeProfile(t, dir, "bad", "backup:\n  pahts: x\n")
	if es := list(1); len(es) != 1 || es[0].Error == "" {
		t.Fatalf("only a broken profile: %+v, want exit 1 and its error", es)
	}
	writeProfile(t, dir, "docs", "repo: /srv/ark\nbackup:\n  path: /data\n")
	es := list(0)
	if len(es) != 2 || es[0].Name != "bad" || es[0].Error == "" ||
		es[1] != (entry{Name: "docs", Repo: "/srv/ark", Path: "/data"}) {
		t.Fatalf("list %+v, want bad with an error and docs", es)
	}
	if code, out := runCapture(t, "profile", "list"); code != 0 || !strings.Contains(out, "/srv/ark") {
		t.Fatalf("table output: exit %d %q", code, out)
	}
	writeProfile(t, dir, "db", "repo: /srv/ark\nbackup:\n  volume: pgdata\n")
	if es := list(0); len(es) != 3 || es[1] != (entry{Name: "db", Repo: "/srv/ark", Volume: "pgdata"}) {
		t.Fatalf("list %+v, want db with its volume and no path", es)
	}
	_, out := runCapture(t, "--json", "profile", "list")
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) != 3 {
		t.Fatalf("json list %q: %v", out, err)
	}
	if _, ok := raw[1]["path"]; ok {
		t.Errorf("volume profile lists a path: %v", raw[1])
	}
	if _, ok := raw[2]["volume"]; ok {
		t.Errorf("path profile lists a volume: %v", raw[2])
	}
	if code, out := runCapture(t, "profile", "list"); code != 0 || !strings.Contains(out, "volume pgdata") {
		t.Fatalf("table output of a volume profile: exit %d %q", code, out)
	}
	if code, out := runCapture(t, "-P", "bad", "profile", "list"); code != 0 || !strings.Contains(out, "/srv/ark") {
		t.Fatalf("list with a broken -P: exit %d %q, want the list", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.yml"), []byte("repo: /x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if es := list(0); len(es) != 4 || es[3].Name != "old.yml" || es[3].Error == "" {
		t.Fatalf("list %+v, want old.yml reported as ignored", es)
	}
}

func TestProfileBackupCommand(t *testing.T) {
	dir := profileDirFor(t)
	repo, docs := initRepo(t), t.TempDir()
	t.Setenv("ARK_REPOSITORY", "")
	if err := os.WriteFile(filepath.Join(docs, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hooks")
	writeProfile(t, dir, "docs", "repo: "+repo+"\nbackup:\n  path: "+docs+
		"\n  tag: [docs]\n  run-before: echo before >> "+marker+"\n")
	writeProfile(t, dir, "plain", "repo: "+repo+"\n")
	backup := func(args ...string) profileSnapshot {
		t.Helper()
		code, out := runCapture(t, append([]string{"--json"}, args...)...)
		var s profileSnapshot
		if code != 0 || json.Unmarshal([]byte(out), &s) != nil {
			t.Fatalf("%v: exit %d %q", args, code, out)
		}
		return s
	}
	for _, args := range [][]string{
		{"profile", "backup", "docs"},
		{"-P", "docs", "profile", "backup"},
		{"-f", filepath.Join(dir, "docs.yaml"), "profile", "backup"},
	} {
		if s := backup(args...); s.Name != docs || !slices.Equal(s.Tags, []string{"docs"}) {
			t.Fatalf("%v: snapshot %+v, want name %s and tag docs", args, s, docs)
		}
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != strings.Repeat("before\n", 3) {
		t.Fatalf("hook output %q, %v, want three runs", b, err)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"profile", "backup"}, "usage: ark profile backup NAME"},
		{[]string{"profile", "backup", "docs", "plain"}, "usage: ark profile backup NAME"},
		{[]string{"-P", "docs", "profile", "backup", "docs"}, "not both"},
		{[]string{"profile", "backup", "plain"}, "names no backup.path or backup.volume"},
		{[]string{"profile", "backup", "docs", "--tag", "x"}, "tag"},
	} {
		if code, _, stderr := runCaptureAll(t, tt.args...); code != 2 || !strings.Contains(stderr, tt.want) {
			t.Errorf("%v: exit %d %q, want 2 and %q", tt.args, code, stderr, tt.want)
		}
	}
}
