package arkprofile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
)

func TestDecodeExpandsAndResolves(t *testing.T) {
	t.Setenv("BACKUP_ROOT", "/data")
	t.Setenv("DAYS", "7")
	t.Setenv("HOME", "/home/me")
	t.Setenv("EMPTY", "")
	t.Setenv("NULL", "null")
	t.Setenv("TILDE", "~")
	const dir = "/etc/ark/profiles"

	tests := []struct {
		name string
		in   string
		got  func(*arkprofile.Profile) any
		want any
	}{
		{
			"plain variable",
			"backup:\n  path: $BACKUP_ROOT/docs\n",
			func(p *arkprofile.Profile) any { return p.Backup.Path },
			"/data/docs",
		},
		{
			"braced variable",
			"backup:\n  comment: ${BACKUP_ROOT}x\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment },
			"/datax",
		},
		{
			"dollar dollar",
			"backup:\n  comment: cost $$5\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment },
			"cost $5",
		},
		{
			"lone dollar stays",
			"backup:\n  comment: 5$ or $1\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment },
			"5$ or $1",
		},
		{
			"empty variable is set",
			"backup:\n  comment: a${EMPTY}b\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment },
			"ab",
		},
		{
			"quoted string",
			"backup:\n  comment: \"$DAYS\"\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment },
			"7",
		},
		{"list item", "backup:\n  tag: [a, $DAYS]\n", func(p *arkprofile.Profile) any { return p.Backup.Tags[1] }, "7"},
		{
			"number from variable",
			"forget:\n  keep-daily: $DAYS\n",
			func(p *arkprofile.Profile) any { return p.Forget.KeepDaily },
			ark.KeepCount(7),
		},
		{
			"null from variable stays a string",
			"backup:\n  comment: $NULL\n  path: $TILDE\n",
			func(p *arkprofile.Profile) any { return p.Backup.Comment + " " + p.Backup.Path },
			"null /home/me",
		},
		{
			"hook stays raw",
			"backup:\n  run-before: echo $HOME $$\n",
			func(p *arkprofile.Profile) any { return p.Backup.RunBefore },
			"echo $HOME $$",
		},
		{
			"hook through merge stays raw",
			"backup:\n  <<: {run-after: \"echo $X\"}\n",
			func(p *arkprofile.Profile) any { return p.Backup.RunAfter },
			"echo $X",
		},
		{
			"relative path",
			"backup:\n  path: docs\n",
			func(p *arkprofile.Profile) any { return p.Backup.Path },
			dir + "/docs",
		},
		{
			"dot path",
			"backup:\n  path: ../docs\n",
			func(p *arkprofile.Profile) any { return p.Backup.Path },
			"/etc/ark/docs",
		},
		{
			"absolute path",
			"backup:\n  path: /srv/x\n",
			func(p *arkprofile.Profile) any { return p.Backup.Path },
			"/srv/x",
		},
		{
			"tilde",
			"password-file: ~/ark.pw\n",
			func(p *arkprofile.Profile) any { return p.PasswordFile },
			"/home/me/ark.pw",
		},
		{
			"bare tilde",
			"backup:\n  path: \"~\"\n",
			func(p *arkprofile.Profile) any { return p.Backup.Path },
			"/home/me",
		},
		{
			"exclude files",
			"backup:\n  exclude-file: [a.txt, ~/b.txt, $BACKUP_ROOT/c]\n",
			func(p *arkprofile.Profile) any { return strings.Join(p.Backup.ExcludeFiles, " ") },
			dir + "/a.txt /home/me/b.txt /data/c",
		},
		{
			"repo URI untouched",
			"repo: sftp://nas/rel/path\n",
			func(p *arkprofile.Profile) any { return p.Repo },
			"sftp://nas/rel/path",
		},
		{
			"repo path resolves",
			"repo: ../ark-repo\n",
			func(p *arkprofile.Profile) any { return p.Repo },
			"/etc/ark/ark-repo",
		},
		{
			"repo tilde",
			"repo: ~/backups/ark\n",
			func(p *arkprofile.Profile) any { return p.Repo },
			"/home/me/backups/ark",
		},
		{
			"exclude pattern untouched",
			"backup:\n  exclude: [\"*.tmp\", build]\n",
			func(p *arkprofile.Profile) any { return strings.Join(p.Backup.Excludes, " ") },
			"*.tmp build",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p arkprofile.Profile
			if err := arkprofile.Decode(strings.NewReader(tt.in), &p, dir); err != nil {
				t.Fatal(err)
			}
			if got := tt.got(&p); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecodeExpandsFieldsOfAnEmbedder(t *testing.T) {
	t.Setenv("WHEN", "03:00")
	var s struct {
		arkprofile.Profile `yaml:",inline"`

		Schedule string `yaml:"schedule"`
	}
	if err := arkprofile.Decode(
		strings.NewReader("schedule: daily $WHEN\nbackup:\n  path: x\n"),
		&s,
		"/d",
	); err != nil {
		t.Fatal(err)
	}
	if s.Schedule != "daily 03:00" || s.Backup.Path != "/d/x" {
		t.Errorf("schedule %q, path %q", s.Schedule, s.Backup.Path)
	}
}

func TestDecodeRefusesAnUnsetVariable(t *testing.T) {
	os.Unsetenv("ARK_TEST_UNSET")
	t.Setenv("FRACTION", "2.0")
	t.Setenv("WHOLE", "7")
	tests := map[string]string{
		"backup:\n  path: $ARK_TEST_UNSET/x\n":                "line 2: backup.path: ARK_TEST_UNSET not set",
		"backup:\n  tag:\n    - a\n    - ${ARK_TEST_UNSET}\n": "line 4: backup.tag: ARK_TEST_UNSET not set",
		"repo: ${bad name}\n":                                 "line 1: repo: ${bad name} is not a variable name",
		"repo: ${OPEN\n":                                      "line 1: repo: unterminated ${",
		"forget:\n  keep-daily: $ARK_TEST_UNSET\n":            "line 2: forget.keep-daily: ARK_TEST_UNSET not set",
		"password-file: ~/x\nbackup:\n  comment: $A$B\n":      "line 3: backup.comment: A not set",
		"forget:\n  keep-daily: $FRACTION\n":                  "line 2: forget.keep-daily: want a whole number",
		"concurrency: !!str $WHOLE\n":                         "line 1: concurrency: want a whole number",
		"backup:\n  path: ~bob/data\n":                        "line 2: backup.path: ~bob/data: only ~ and ~/ name a home directory, give the full path",
	}
	for in, want := range tests {
		var p arkprofile.Profile
		err := arkprofile.Decode(strings.NewReader(in), &p, "")
		if err == nil || !errors.Is(err, arkprofile.ErrInvalid) || err.Error() != want {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
}

func TestLoadResolvesAgainstTheFilesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docs.yaml")
	if err := os.WriteFile(path, []byte("backup:\n  path: data\npassword-file: pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := arkprofile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Backup.Path != filepath.Join(dir, "data") || p.PasswordFile != filepath.Join(dir, "pw") {
		t.Errorf("path %q, password-file %q", p.Backup.Path, p.PasswordFile)
	}
}
