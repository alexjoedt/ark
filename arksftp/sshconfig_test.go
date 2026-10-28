package arksftp

import (
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
