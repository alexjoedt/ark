package arkprofile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
)

const fullProfile = `repo: /srv/ark
password-file: /etc/ark/docs.pw
lock-wait: 30s
concurrency: 4
backup:
  path: /home/me/Documents
  tag: [docs, daily]
  exclude: ["*.tmp"]
  exclude-file: [/etc/ark/docs.exclude]
  exclude-cloud-files: true
  host: laptop
  comment: nightly
  name: documents
  run-before: mount /mnt/docs
  run-finally: umount /mnt/docs
forget:
  keep-last: 3
  keep-daily: 7
  keep-within: 30d
  prune: true
  run-after-fail: echo failed
`

func TestDecodeFillsEveryField(t *testing.T) {
	var p arkprofile.Profile
	if err := arkprofile.Decode(strings.NewReader(fullProfile), &p, ""); err != nil {
		t.Fatal(err)
	}
	want := arkprofile.Profile{
		Repo:         "/srv/ark",
		PasswordFile: "/etc/ark/docs.pw",
		LockWait:     30 * time.Second,
		Concurrency:  4,
		Backup: arkprofile.Backup{
			Path:              "/home/me/Documents",
			Tags:              []string{"docs", "daily"},
			Excludes:          []string{"*.tmp"},
			ExcludeFiles:      []string{"/etc/ark/docs.exclude"},
			ExcludeCloudFiles: true,
			Host:              "laptop",
			Comment:           "nightly",
			Name:              "documents",
			Hooks:             arkprofile.Hooks{RunBefore: "mount /mnt/docs", RunFinally: "umount /mnt/docs"},
		},
		Forget: arkprofile.Forget{
			KeepLast:   3,
			KeepDaily:  7,
			KeepWithin: "30d",
			Prune:      true,
			Hooks:      arkprofile.Hooks{RunAfterFail: "echo failed"},
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("decoded\n%+v\nwant\n%+v", p, want)
	}
}

func TestDecodeIntoAStructEmbeddingAProfile(t *testing.T) {
	var s struct {
		arkprofile.Profile `yaml:",inline"`

		Schedule string `yaml:"schedule"`
	}
	in := "repo: /srv/ark\nschedule: daily 03:00\nbackup:\n  path: /data\n"
	if err := arkprofile.Decode(strings.NewReader(in), &s, ""); err != nil {
		t.Fatal(err)
	}
	if s.Schedule != "daily 03:00" || s.Repo != "/srv/ark" || s.Backup.Path != "/data" {
		t.Fatalf("decoded %+v", s)
	}

	err := arkprofile.Decode(strings.NewReader("schedule: daily\nschedul: x\n"), &s, "")
	if err == nil || err.Error() != "line 2: schedul: unknown field" {
		t.Fatalf("unknown key next to schedule: %v", err)
	}
}

func TestDecodeMergesSharedHooks(t *testing.T) {
	in := `backup:
  path: /data
  <<: &hooks
    run-before: mount /data
    run-after: umount /data
forget:
  <<: [*hooks, {run-finally: sync}]
  run-after: echo done
`
	var p arkprofile.Profile
	if err := arkprofile.Decode(strings.NewReader(in), &p, ""); err != nil {
		t.Fatal(err)
	}
	want := arkprofile.Hooks{RunBefore: "mount /data", RunAfter: "echo done", RunFinally: "sync"}
	if p.Backup.RunBefore != "mount /data" || p.Backup.RunAfter != "umount /data" || p.Forget.Hooks != want {
		t.Fatalf("backup hooks %+v, forget hooks %+v", p.Backup.Hooks, p.Forget.Hooks)
	}
}

func TestDecodeRefusesAStructThatCannotHoldAProfile(t *testing.T) {
	var shared struct {
		arkprofile.Profile `yaml:",inline"`

		Repo string `yaml:"repo"`
	}
	if err := arkprofile.Decode(strings.NewReader("repo: x\n"), &shared, ""); err == nil ||
		errors.Is(err, arkprofile.ErrInvalid) {
		t.Errorf("key shared with the profile: %v", err)
	}

	var pointer struct {
		*arkprofile.Profile `yaml:",inline"`
	}
	if err := arkprofile.Decode(strings.NewReader("repo: x\n"), &pointer, ""); err == nil ||
		errors.Is(err, arkprofile.ErrInvalid) {
		t.Errorf("inline *Profile: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"unknown key", "repo: /r\nrepos: /r\n", "line 2: repos: unknown field"},
		{"unknown key in a section", "backup:\n  path: /p\n  pahts: [/q]\n", "line 3: backup.pahts: unknown field"},
		{"unknown section", "prune:\n  keep-last: 1\n", "line 1: prune: unknown field"},
		{"name is not a key", "name: docs\n", "line 1: name: unknown field"},
		{"duplicate key", "repo: /a\nrepo: /b\n", "line 2: repo: already defined at line 1"},
		{
			"duplicate key in a section",
			"forget:\n  keep-last: 1\n  keep-last: 2\n",
			"line 3: forget.keep-last: already defined at line 2",
		},
		{"duplicate section", "backup: {}\nbackup: {}\n", "line 2: backup: already defined at line 1"},
		{
			"password",
			"repo: /r\npassword: hunter2\n",
			"line 2: password: a profile holds no password, name a file with password-file or a variable with password-env",
		},
		{"second document", "repo: /a\n---\nrepo: /b\n", "line 2: a second YAML document, a profile is one document"},
		{"empty", "", "empty profile"},
		{"only a comment", "# nothing\n", "empty profile"},
		{"null document", "~\n", "empty profile"},
		{"not a mapping", "- repo\n", "line 1: profile: want a mapping of keys"},
		{"section not a mapping", "backup: /data\n", "line 1: backup: want a mapping of keys"},
		{"wrong kind", "forget:\n  keep-last: three\n", "line 2: forget.keep-last: want a whole number"},
		{"scalar for a list", "backup:\n  tag: docs\n", "line 2: backup.tag: want a list of strings"},
		{"bare number as duration", "lock-wait: 30\n", "line 1: lock-wait: want a duration such as 30s or 5m"},
		{"bad bool", "insecure-no-password: maybe\n", "line 1: insecure-no-password: want true or false"},
		{"fraction for a whole number", "concurrency: 1.5\n", "line 1: concurrency: want a whole number"},
		{"fraction in a section", "forget:\n  keep-daily: 7.5\n", "line 2: forget.keep-daily: want a whole number"},
		{"complex key", "? [a]\n: 1\n", "line 1: profile: a key must be a plain name, not a list or a mapping"},
		{
			"merge of a scalar",
			"forget:\n  <<: x\n",
			"line 2: forget.<<: want a mapping or a list of mappings to merge",
		},
		{
			"unknown key through a merge",
			"backup: &b\n  path: /p\nforget:\n  <<: *b\n",
			"line 2: forget.path: unknown field",
		},
		{"syntax", "repo: [\n", "line 1: did not find expected node content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p arkprofile.Profile
			err := arkprofile.Decode(strings.NewReader(tt.in), &p, "")
			if err == nil {
				t.Fatalf("decoded %+v", p)
			}
			if !errors.Is(err, arkprofile.ErrInvalid) {
				t.Errorf("%v does not match ErrInvalid", err)
			}
			if err.Error() != tt.want {
				t.Errorf("got  %q\nwant %q", err, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := arkprofile.Profile{Name: "docs", Repo: "/srv/ark", PasswordEnv: "DOCS_PW"}
	ok.Backup.Path = "/data"
	ok.Forget.KeepWithin = "30d"
	if err := ok.Validate(); err != nil {
		t.Fatalf("profile built in code: %v", err)
	}

	tests := []struct {
		name string
		edit func(*arkprofile.Profile)
	}{
		{"file and env", func(p *arkprofile.Profile) { p.PasswordFile = "/pw" }},
		{"insecure and env", func(p *arkprofile.Profile) { p.InsecureNoPassword = true }},
		{"insecure and file", func(p *arkprofile.Profile) {
			p.PasswordEnv, p.PasswordFile, p.InsecureNoPassword = "", "/pw", true
		}},
		{"no name", func(p *arkprofile.Profile) { p.Name = "" }},
		{"upper case", func(p *arkprofile.Profile) { p.Name = "Docs" }},
		{"leading dot", func(p *arkprofile.Profile) { p.Name = ".docs" }},
		{"slash", func(p *arkprofile.Profile) { p.Name = "a/b" }},
		{"negative concurrency", func(p *arkprofile.Profile) { p.Concurrency = -4 }},
		{"negative lock-wait", func(p *arkprofile.Profile) { p.LockWait = -time.Second }},
		{"negative keep-daily", func(p *arkprofile.Profile) { p.Forget.KeepDaily = -1 }},
		{"negative keep-yearly", func(p *arkprofile.Profile) { p.Forget.KeepYearly = -1 }},
		{"bad keep-within", func(p *arkprofile.Profile) { p.Forget.KeepWithin = "30x" }},
		{"zero keep-within", func(p *arkprofile.Profile) { p.Forget.KeepWithin = "0d" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ok
			tt.edit(&p)
			if err := p.Validate(); !errors.Is(err, arkprofile.ErrInvalid) {
				t.Fatalf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func writeFile(t *testing.T, path, data string, perm os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p, err := arkprofile.Load(writeFile(t, filepath.Join(dir, "docs.v2_a-b.yaml"), "repo: /srv/ark\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "docs.v2_a-b" || p.Repo != "/srv/ark" {
		t.Fatalf("loaded %+v", p)
	}

	tests := []struct {
		name, file, data string
		perm             os.FileMode
		want             string
	}{
		{"group writable", "g.yaml", "repo: /r\n", 0o620, "writable by group or others (mode 0620)"},
		{"others writable", "o.yaml", "repo: /r\n", 0o602, "writable by group or others (mode 0602)"},
		{"bad name", "Docs.yaml", "repo: /r\n", 0o600, `name "Docs"`},
		{
			"decode error names file and line",
			"bad.yaml", "repo: /r\nrepo: /s\n", 0o600,
			"bad.yaml: line 2: repo: already defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := arkprofile.Load(writeFile(t, filepath.Join(dir, tt.file), tt.data, tt.perm))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want %q", err, tt.want)
			}
		})
	}
}

type ownedBy struct {
	fs.FileInfo
	uid uint32
}

func (o ownedBy) Mode() fs.FileMode { return 0o600 }
func (o ownedBy) Sys() any          { return &syscall.Stat_t{Uid: o.uid} }

func TestCheckFileRefusesAForeignOwner(t *testing.T) {
	me := uint32(os.Getuid())
	if err := arkprofile.CheckFile(ownedBy{uid: me}); err != nil {
		t.Errorf("own file: %v", err)
	}
	if err := arkprofile.CheckFile(ownedBy{uid: 0}); err != nil {
		t.Errorf("root's file: %v", err)
	}
	if err := arkprofile.CheckFile(ownedBy{uid: me + 1}); err == nil {
		t.Error("a file of another user was accepted")
	}
}

func TestPassphrase(t *testing.T) {
	dir := t.TempDir()
	pwFile := writeFile(t, filepath.Join(dir, "pw"), "secret\r\n", 0o600)
	emptyFile := writeFile(t, filepath.Join(dir, "empty"), "\n", 0o600)
	t.Setenv("ARK_PASSWORD", "from-ark-password")
	t.Setenv("ARK_PASSWORD_FILE", pwFile)
	t.Setenv("DOCS_PW", "from-env")
	t.Setenv("EMPTY_PW", "")

	tests := []struct {
		name    string
		p       arkprofile.Profile
		want    ark.Passphrase
		wantErr string
	}{
		{"file", arkprofile.Profile{PasswordFile: pwFile}, ark.WithPassword("secret"), ""},
		{"env", arkprofile.Profile{PasswordEnv: "DOCS_PW"}, ark.WithPassword("from-env"), ""},
		{"insecure", arkprofile.Profile{InsecureNoPassword: true}, ark.WithoutPassword(), ""},
		{"empty file", arkprofile.Profile{PasswordFile: emptyFile}, ark.Passphrase{}, "is empty"},
		{
			"unset env",
			arkprofile.Profile{PasswordEnv: "NO_SUCH_PW"},
			ark.Passphrase{},
			"variable NO_SUCH_PW is not set",
		},
		{"empty env", arkprofile.Profile{PasswordEnv: "EMPTY_PW"}, ark.Passphrase{}, "variable EMPTY_PW is not set"},
		{
			"conflict",
			arkprofile.Profile{PasswordEnv: "DOCS_PW", InsecureNoPassword: true},
			ark.Passphrase{},
			"conflicts",
		},
		{"none ignores ARK_ variables", arkprofile.Profile{}, ark.Passphrase{}, arkprofile.ErrNoPassword.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw, err := tt.p.Passphrase()
			if tt.wantErr == "" {
				if err != nil || pw != tt.want {
					t.Fatalf("Passphrase = %v, %v; want %v", pw, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Passphrase error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if _, err := (&arkprofile.Profile{}).Passphrase(); !errors.Is(err, arkprofile.ErrNoPassword) {
		t.Errorf("no source: %v, want ErrNoPassword", err)
	}
}
