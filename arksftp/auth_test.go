package arksftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// isolate clears the ambient ssh environment so a developer's own agent, keys
// and password never leak into a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv(EnvPassword, "")
	t.Setenv(EnvKeyPassphrase, "")
}

// writeEncryptedIdentity writes a PEM key locked with passphrase and returns
// its path together with the matching signer.
//
//nolint:unparam // the passphrase is a fixture
func writeEncryptedIdentity(t *testing.T, passphrase string) (string, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	file := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return file, signer
}

// serveAgent starts an ssh-agent holding key on a unix socket and points
// SSH_AUTH_SOCK at it. The socket lives in a short temp dir because the path
// has to fit in a sockaddr_un.
func serveAgent(t *testing.T, key ed25519.PrivateKey) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ark") //nolint:usetesting // t.TempDir paths overflow sockaddr_un
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: &key}); err != nil {
		t.Fatalf("add key: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()

	t.Setenv("SSH_AUTH_SOCK", sock)
}

// recorder is a server config that accepts one specific public key and
// remembers what the client offered.
type recorder struct {
	mu      sync.Mutex
	offered []string
}

func (r *recorder) config(accept ssh.PublicKey) *ssh.ServerConfig {
	return &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			r.mu.Lock()
			r.offered = append(r.offered, ssh.FingerprintSHA256(key))
			r.mu.Unlock()
			if accept != nil && string(key.Marshal()) == string(accept.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
}

func (r *recorder) sawKey(key ssh.PublicKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := ssh.FingerprintSHA256(key)
	return slices.Contains(r.offered, want)
}

func TestAuthUsesTheAgent(t *testing.T) {
	isolate(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	serveAgent(t, priv)

	rec := &recorder{}
	host := newSigner(t)
	addr := serveSSH(t, host, rec.config(signer))

	if err := dial(t, addr, writeKnownHosts(t, addr, host.PublicKey())); err != nil {
		t.Fatalf("dial with agent key: %v", err)
	}
	if !rec.sawKey(signer) {
		t.Error("the agent key was never offered")
	}
}

func TestAuthUsesAnEncryptedIdentityFile(t *testing.T) {
	isolate(t)
	file, signer := writeEncryptedIdentity(t, "correct horse")
	t.Setenv(EnvKeyPassphrase, "correct horse")

	rec := &recorder{}
	host := newSigner(t)
	addr := serveSSH(t, host, rec.config(signer.PublicKey()))

	cfg := Config{
		Host:           "127.0.0.1",
		User:           "ark",
		IdentityFile:   file,
		KnownHostsFile: writeKnownHosts(t, addr, host.PublicKey()),
	}
	sshCfg, chain, err := buildSSHConfig(cfg)
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer chain.close()

	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		t.Fatalf("dial with encrypted key: %v", err)
	}
	_ = conn.Close()
}

func TestAuthFallsBackToTheDefaultKeys(t *testing.T) {
	isolate(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	rec := &recorder{}
	host := newSigner(t)
	addr := serveSSH(t, host, rec.config(signer.PublicKey()))

	hostName, port := splitAddr(t, addr)
	sshCfg, chain, err := buildSSHConfig(Config{
		Host:           hostName,
		Port:           port,
		User:           "ark",
		KnownHostsFile: writeKnownHosts(t, addr, host.PublicKey()),
	})
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer chain.close()

	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		t.Fatalf("dial with default key: %v", err)
	}
	_ = conn.Close()
}

func TestAuthSkipsAnEncryptedDefaultKeyWithNoPassphrase(t *testing.T) {
	isolate(t)
	t.Setenv(EnvPassword, "hunter2")
	file, _ := writeEncryptedIdentity(t, "correct horse")
	dir := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(file, filepath.Join(dir, "id_ed25519")); err != nil {
		t.Fatalf("move identity: %v", err)
	}

	chain, err := buildAuthChain(Config{Host: "example.com"})
	if err != nil {
		t.Fatalf("an unusable default key blocked the password: %v", err)
	}
	defer chain.close()
	if !strings.Contains(chain.describe(), EnvKeyPassphrase) {
		t.Errorf("the skipped default key is not reported:\n%s", chain.describe())
	}
}

func TestAuthReportsAWrongPassphrase(t *testing.T) {
	isolate(t)
	file, _ := writeEncryptedIdentity(t, "correct horse")
	t.Setenv(EnvKeyPassphrase, "wrong")

	_, _, err := buildSSHConfig(Config{IdentityFile: file})
	if err == nil {
		t.Fatal("a wrong passphrase was accepted")
	}
	for _, want := range []string{file, EnvKeyPassphrase} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestAuthReportsAnEncryptedKeyWithNoPassphrase(t *testing.T) {
	isolate(t)
	file, _ := writeEncryptedIdentity(t, "correct horse")

	_, _, err := buildSSHConfig(Config{IdentityFile: file})
	if err == nil {
		t.Fatal("an encrypted key with no passphrase was accepted")
	}
	if !strings.Contains(err.Error(), EnvKeyPassphrase) {
		t.Errorf("error does not name the passphrase variable:\n%v", err)
	}
}

func TestAuthReportsNoUsableMethod(t *testing.T) {
	isolate(t)

	_, _, err := buildSSHConfig(Config{Host: "example.com"})
	if err == nil {
		t.Fatal("a config with no auth method was accepted")
	}
	for _, want := range []string{
		"SSH_AUTH_SOCK not set",
		"Config.IdentityFile not set",
		EnvPassword,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestAuthReportsWhichMethodsWereTried(t *testing.T) {
	isolate(t)
	t.Setenv(EnvPassword, "hunter2")
	file := writeIdentity(t)

	rec := &recorder{}
	hostKey := newSigner(t)
	addr := serveSSH(t, hostKey, rec.config(nil)) // rejects everything

	host, port := splitAddr(t, addr)
	_, err := New(Config{
		Host:           host,
		Port:           port,
		User:           "ark",
		IdentityFile:   file,
		KnownHostsFile: writeKnownHosts(t, addr, hostKey.PublicKey()),
	})
	if err == nil {
		t.Fatal("connected to a server that accepts nothing")
	}
	for _, want := range []string{
		"tried identity file " + file,
		EnvPassword,
		"skipped ssh-agent (SSH_AUTH_SOCK not set)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestAuthKeepsSecretsOutOfConfig(t *testing.T) {
	isolate(t)
	t.Setenv(EnvPassword, "hunter2")

	chain, err := buildAuthChain(Config{})
	if err != nil {
		t.Fatalf("buildAuthChain: %v", err)
	}
	defer chain.close()

	if strings.Contains(chain.describe(), "hunter2") {
		t.Errorf("the password leaked into the chain description: %s", chain.describe())
	}
}

// A running agent whose keys the server refuses must not stop the identity
// file from being offered. macOS starts an agent per login session, so this
// is the default shape there.
func TestAuthFallsThroughAnAgentWithoutTheRightKey(t *testing.T) {
	isolate(t)
	_, stray, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serveAgent(t, stray)
	file, signer := writeEncryptedIdentity(t, "correct horse")
	t.Setenv(EnvKeyPassphrase, "correct horse")

	rec := &recorder{}
	host := newSigner(t)
	addr := serveSSH(t, host, rec.config(signer.PublicKey()))

	h, port := splitAddr(t, addr)
	sshCfg, chain, err := buildSSHConfig(Config{
		Host:           h,
		Port:           port,
		User:           "ark",
		IdentityFile:   file,
		KnownHostsFile: writeKnownHosts(t, addr, host.PublicKey()),
	})
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer chain.close()

	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		t.Fatalf("dial past an agent with the wrong key: %v", err)
	}
	_ = conn.Close()
	if !rec.sawKey(signer.PublicKey()) {
		t.Error("the identity file key was never offered")
	}
}
