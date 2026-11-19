package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerProfileWithoutDaemon covers what -P resolves before the daemon
// is asked: the volume, the refusals and the daemon address. Nothing listens
// on the tcp addresses, so a run that gets past resolution fails with exit 1
// and an error naming the address it tried.
func TestDockerProfileWithoutDaemon(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	marker := filepath.Join(t.TempDir(), "hooks")
	writeProfile(t, dir, "db", "repo: "+repo+`
docker-host: tcp://127.0.0.1:3
backup:
  volume: pgdata
  host: dbhost
  run-before: echo before >> `+marker+`
  run-after-fail: echo failed >> `+marker+`
`)
	writeProfile(t, dir, "files", "repo: "+repo+"\nbackup:\n  path: "+t.TempDir()+"\n")
	writeProfile(t, dir, "named", "repo: "+repo+"\nbackup:\n  source: imported\n")
	writeProfile(t, dir, "plain", "repo: "+repo+"\n")
	writeProfile(t, dir, "cache", "repo: "+repo+"\nbackup:\n  volume: cache\n")
	pg := volumeSnapshot(t, repo)

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"backup of a volume profile", []string{"backup", "-P", "db"}, 2, "use ark docker backup -P db"},
		{
			"backup of a volume profile with a path",
			[]string{"backup", "-P", "db", t.TempDir()},
			2,
			"ark docker backup -P db",
		},
		{"docker backup of a path profile", []string{"docker", "backup", "-P", "files"}, 2, "use ark backup -P files"},
		{
			"docker backup of a source profile",
			[]string{"docker", "backup", "-P", "named", "vol"},
			2,
			"ark backup -P named",
		},
		{
			"docker backup of another volume",
			[]string{"docker", "backup", "-P", "db", "other"},
			2,
			"not the backup.volume of profile db",
		},
		{"docker backup without volume", []string{"docker", "backup", "-P", "plain"}, 2, "backup.volume"},
		{"docker backup two volumes", []string{"docker", "backup", "-P", "db", "pgdata", "x"}, 2, ""},
		{"profile volume and daemon", []string{"docker", "backup", "-P", "db"}, 1, "docker backup pgdata"},
		{"own volume given", []string{"docker", "backup", "-P", "db", "pgdata"}, 1, "127.0.0.1:3"},
		{
			"argument with a profile without volume",
			[]string{"docker", "backup", "-P", "plain", "vol"},
			1,
			"127.0.0.1:1",
		},
		{
			"flag beats profile",
			[]string{"docker", "--docker-host", "tcp://127.0.0.1:2", "backup", "-P", "db"},
			1,
			"127.0.0.1:2",
		},
		{"daemon address as host", []string{"docker", "backup", "-P", "db", "--host", "tcp://x"}, 2, "--docker-host"},
		{"restore unknown id", []string{"docker", "restore", "-P", "db", "nope"}, 4, ""},
		{"restore into backup.volume", []string{"docker", "restore", "-P", "db", "-y", pg}, 1, "127.0.0.1:3"},
		{
			"restore of another volume",
			[]string{"docker", "restore", "-P", "cache", "-y", pg},
			2,
			"not the backup.volume cache",
		},
		{
			"restore of another volume named",
			[]string{"docker", "restore", "-P", "cache", "-y", pg, "x"},
			1,
			"127.0.0.1:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCaptureAll(t, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.want) {
				t.Fatalf("exit %d, stderr %q, want %d and %q", code, stderr, tt.code, tt.want)
			}
		})
	}

	// only the three db runs that got past resolution ran the hooks
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), strings.Repeat("before\nfailed\n", 3); got != want {
		t.Fatalf("hook output %q, want %q", got, want)
	}
}
