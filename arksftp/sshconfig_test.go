package arksftp

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture parses an ssh_config from a literal, so no test reads the
// developer's own ~/.ssh/config.
func fixture(t *testing.T, text string) sshConfig {
	t.Helper()
	sc, err := decodeSSHConfig(strings.NewReader(text))
	if err != nil {
		t.Fatalf("decode ssh_config: %v", err)
	}
	return sc
}

const aliasConfig = `
Host backup
  HostName storage.example.com
  User ark
  Port 2222
  IdentityFile ~/.ssh/id_backup

Host *
  IdentityFile ~/.ssh/id_fallback
`

func TestResolveFillsFromAnAliasStanza(t *testing.T) {
	got := resolve(Config{Host: "backup", Path: "/srv/ark"}, fixture(t, aliasConfig))

	if got.Host != "storage.example.com" {
		t.Errorf("Host = %q, want the HostName from the stanza", got.Host)
	}
	if got.User != "ark" {
		t.Errorf("User = %q, want ark", got.User)
	}
	if got.Port != 2222 {
		t.Errorf("Port = %d, want 2222", got.Port)
	}
	if !strings.HasSuffix(got.IdentityFile, "/.ssh/id_backup") {
		t.Errorf("IdentityFile = %q, want the stanza key", got.IdentityFile)
	}
	if strings.HasPrefix(got.IdentityFile, "~") {
		t.Errorf("IdentityFile = %q, ~ was not expanded", got.IdentityFile)
	}
	if got.Path != "/srv/ark" {
		t.Errorf("Path = %q, ssh_config must not touch it", got.Path)
	}
}

func TestResolveFallsBackToAWildcardStanza(t *testing.T) {
	got := resolve(Config{Host: "other.example.com"}, fixture(t, aliasConfig))

	if got.Host != "other.example.com" {
		t.Errorf("Host = %q, want it unchanged", got.Host)
	}
	if !strings.HasSuffix(got.IdentityFile, "/.ssh/id_fallback") {
		t.Errorf("IdentityFile = %q, want the wildcard key", got.IdentityFile)
	}
	if got.Port != defaultPort {
		t.Errorf("Port = %d, want the default %d", got.Port, defaultPort)
	}
}

func TestResolveKeepsExplicitFields(t *testing.T) {
	cfg := Config{
		Host:         "backup",
		User:         "root",
		Port:         2022,
		IdentityFile: "/keys/explicit",
	}

	got := resolve(cfg, fixture(t, aliasConfig))

	if got.User != "root" || got.Port != 2022 || got.IdentityFile != "/keys/explicit" {
		t.Errorf("ssh_config overrode explicit fields: %+v", got)
	}
	if got.Host != "storage.example.com" {
		t.Errorf("Host = %q, HostName still resolves the alias", got.Host)
	}
}

func TestResolveAppliesDefaultsWithoutSSHConfig(t *testing.T) {
	got := resolve(Config{Host: "storage.example.com"}, nil)

	if got.Port != defaultPort {
		t.Errorf("Port = %d, want %d", got.Port, defaultPort)
	}
	if got.User == "" {
		t.Error("User is empty, want the current OS user")
	}
	if got.IdentityFile != "" {
		t.Errorf("IdentityFile = %q, want it left empty", got.IdentityFile)
	}
}

func TestResolveExpandsPercentTokens(t *testing.T) {
	const cfgText = `
Host tokens
  HostName storage.example.com
  User ark
  Port 2222
  IdentityFile /keys/%r@%h:%p
`
	got := resolve(Config{Host: "tokens"}, fixture(t, cfgText))

	if want := "/keys/ark@storage.example.com:2222"; got.IdentityFile != want {
		t.Errorf("IdentityFile = %q, want %q", got.IdentityFile, want)
	}
}

func TestLoadSSHConfigSurvivesMissingFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	sc := loadSSHConfig()
	if sc == nil {
		t.Fatal("loadSSHConfig returned nil")
	}
	if got := sc.Get("anything", "HostName"); got != "" {
		t.Errorf("Get returned %q from a config that does not exist", got)
	}
}

func TestMatchBlocksAreSkippedAndTheRestApplies(t *testing.T) {
	sc := fixture(t, `
Host before
  User one

Match host a user b
  User matched
  Port 9

match all
  User matched2

Host after
  User two
  IdentityFile "Match me"
  # Match comment
  Port 2200
`)
	if got := sc.Get("before", "User"); got != "one" {
		t.Errorf("before User = %q, want one", got)
	}
	if got := sc.Get("after", "User"); got != "two" {
		t.Errorf("after User = %q, want two", got)
	}
	if got := sc.Get("after", "Port"); got != "2200" {
		t.Errorf("after Port = %q, want 2200 (a Match in a comment is no block)", got)
	}
	if got := sc.Get("after", "IdentityFile"); got != "Match me" {
		t.Errorf("after IdentityFile = %q, want a value that contains Match kept", got)
	}
}

func TestStripMatchBlocksReportsRemoval(t *testing.T) {
	if _, stripped := stripMatchBlocks("Host a\n  User x\n"); stripped {
		t.Error("stripped = true for a file without Match")
	}
	text := "Host a\n  User x\nMatch all\n  User y\nHost b\n  User z\n"
	got, stripped := stripMatchBlocks(text)
	if !stripped || strings.Contains(got, "User y") || !strings.Contains(got, "User z") {
		t.Errorf("got %q, stripped %v", got, stripped)
	}
}

// userSSHConfig points loadSSHConfig at a user ssh_config of its own, with
// files given by their path under ~/.ssh, and returns that directory.
func userSSHConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	orig := sshConfigFiles
	t.Cleanup(func() { sshConfigFiles = orig })
	sshConfigFiles = sshConfigFiles[:1]

	dir := filepath.Join(home, ".ssh")
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMatchBlocksInIncludedFilesAreSkippedAndTheRestApplies(t *testing.T) {
	dir := userSSHConfig(t, map[string]string{
		"config": `Include config.d/*
Port 2022

Host nested
  Include nested.conf
  IdentityFile ~/.ssh/n
`,
		"config.d/work": `Host work
  User worker

Match exec "true"
  User matched

Host after
  User later`,
		"nested.conf": `User inner

Host other
  User leaked
`,
	})
	var logs bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	sc := loadSSHConfig()
	loadSSHConfig()

	for _, tc := range []struct{ alias, key, want string }{
		{"work", "User", "worker"},
		{"after", "User", "later"},
		{"work", "Port", "2022"}, // a line after the Include is no part of the included stanzas
		{"nested", "User", "inner"},
		{"nested", "IdentityFile", "~/.ssh/n"},
		{"work", "IdentityFile", ""},
		{"other", "User", ""}, // a stanza included inside a Host block is dropped
	} {
		if got := sc.Get(tc.alias, tc.key); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.alias, tc.key, got, tc.want)
		}
	}
	warning := "ignoring Match blocks in ssh_config\" path=" + filepath.Join(dir, "config.d", "work")
	if n := strings.Count(logs.String(), warning); n != 1 {
		t.Errorf("%d Match warnings for the included file after two loads, want 1:\n%s", n, &logs)
	}
}

func TestOversizedSSHConfigIsSkipped(t *testing.T) {
	userSSHConfig(t, map[string]string{
		"config": "Host big\n  User u\n" + strings.Repeat("# padding\n", maxSSHConfigSize/10),
	})
	if got := loadSSHConfig().Get("big", "User"); got != "" {
		t.Errorf("User = %q from a file over the size limit, want it skipped", got)
	}
}
