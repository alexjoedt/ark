package arksftp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// newSigner returns a fresh ed25519 host or client key.
func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

// writeKnownHosts writes one known_hosts file mapping addr to key and returns
// its path. An empty addr writes a file with no usable entry.
func writeKnownHosts(t *testing.T, addr string, key ssh.PublicKey) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "known_hosts")
	line := ""
	if addr != "" {
		line = knownhosts.Line([]string{knownhosts.Normalize(addr)}, key) + "\n"
	}
	if err := os.WriteFile(file, []byte(line), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return file
}

// callback builds the host key callback for a known_hosts file.
func callback(t *testing.T, file string) ssh.HostKeyCallback {
	t.Helper()
	cb, err := hostKeys(Config{Host: "example.com", KnownHostsFile: file})
	if err != nil {
		t.Fatalf("hostKeys: %v", err)
	}
	return cb
}

func TestHostKeysAcceptsAKnownKey(t *testing.T) {
	host := newSigner(t)
	file := writeKnownHosts(t, "example.com:22", host.PublicKey())

	if err := callback(t, file)("example.com:22", &net.TCPAddr{}, host.PublicKey()); err != nil {
		t.Fatalf("known key rejected: %v", err)
	}
}

func TestHostKeysRejectsAnUnknownHost(t *testing.T) {
	host := newSigner(t)
	file := writeKnownHosts(t, "other.example.com:22", newSigner(t).PublicKey())

	err := callback(t, file)("example.com:2222", &net.TCPAddr{}, host.PublicKey())
	if err == nil {
		t.Fatal("unknown host accepted")
	}
	for _, want := range []string{
		"is not in " + file,
		ssh.FingerprintSHA256(host.PublicKey()),
		"ssh-keyscan -p 2222 -H example.com >> " + file,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestHostKeysRejectsAChangedKey(t *testing.T) {
	known := newSigner(t)
	offered := newSigner(t)
	file := writeKnownHosts(t, "example.com:22", known.PublicKey())

	err := callback(t, file)("example.com:22", &net.TCPAddr{}, offered.PublicKey())
	if err == nil {
		t.Fatal("changed host key accepted")
	}
	if !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("error does not name the mismatch:\n%v", err)
	}
	for _, want := range []string{
		ssh.FingerprintSHA256(offered.PublicKey()),
		ssh.FingerprintSHA256(known.PublicKey()),
		file + ":1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestHostKeysFailsOnAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "known_hosts")

	cb, err := hostKeys(Config{Host: "example.com", KnownHostsFile: missing})
	if err == nil {
		t.Fatalf("missing known_hosts accepted, callback %v", cb != nil)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the file:\n%v", err)
	}
}

func TestHostKeysInsecureAcceptsAnything(t *testing.T) {
	cb, err := hostKeys(Config{Host: "example.com", InsecureIgnoreHostKey: true})
	if err != nil {
		t.Fatalf("hostKeys: %v", err)
	}
	if err := cb("example.com:22", &net.TCPAddr{}, newSigner(t).PublicKey()); err != nil {
		t.Fatalf("insecure callback rejected a key: %v", err)
	}
}

// TestHostKeysInsecureWarnsOnce: opting out of host key verification is
// logged at warn level, once per connection setup, naming the host and not
// the key.
func TestHostKeysInsecureWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cb, err := hostKeys(Config{Host: "example.com", InsecureIgnoreHostKey: true})
	if err != nil {
		t.Fatalf("hostKeys: %v", err)
	}
	for range 3 {
		if err := cb("example.com:22", &net.TCPAddr{}, newSigner(t).PublicKey()); err != nil {
			t.Fatalf("insecure callback rejected a key: %v", err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "host=example.com") {
		t.Errorf("warning does not name the host:\n%s", logs.String())
	}
}

// writeIdentity writes a fresh PEM private key file and returns its path.
// buildSSHConfig needs one auth method before it will build a config at all,
// and the test server accepts any key.
func writeIdentity(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	file := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return file
}

// acceptAnyKey is a server config that lets any public key in.
func acceptAnyKey() *ssh.ServerConfig {
	return &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
}

// serveSSH starts an ssh server on loopback with cfg and returns its address.
// It completes the handshake and rejects every channel: the tests here are
// about the client config, not about serving sftp.
func serveSSH(t *testing.T, host ssh.Signer, cfg *ssh.ServerConfig) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cfg.AddHostKey(host)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "no channels")
				}
			}()
		}
	}()

	return ln.Addr().String()
}

// splitAddr splits a "host:port" listener address into the Config fields.
func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %s: %v", portStr, err)
	}
	return host, port
}

// dial builds the client config the same way New does and completes a
// handshake against addr.
func dial(t *testing.T, addr, knownHosts string) error {
	t.Helper()
	host, port := splitAddr(t, addr)
	cfg, chain, err := buildSSHConfig(Config{
		Host:           host,
		Port:           port,
		User:           "ark",
		IdentityFile:   writeIdentity(t),
		KnownHostsFile: knownHosts,
	})
	if err != nil {
		return err
	}
	defer chain.close()

	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

func TestHandshakeVerifiesTheServerHostKey(t *testing.T) {
	isolate(t)
	host := newSigner(t)
	addr := serveSSH(t, host, acceptAnyKey())

	t.Run("known host connects", func(t *testing.T) {
		file := writeKnownHosts(t, addr, host.PublicKey())
		if err := dial(t, addr, file); err != nil {
			t.Fatalf("dial: %v", err)
		}
	})

	t.Run("unknown host is refused", func(t *testing.T) {
		file := writeKnownHosts(t, "", nil)
		err := dial(t, addr, file)
		if err == nil {
			t.Fatal("connected to a host that is not in known_hosts")
		}
		if !strings.Contains(err.Error(), "ssh-keyscan") {
			t.Errorf("error does not point at the fix:\n%v", err)
		}
	})
}

func TestHandshakeAcceptsAnEd25519OnlyKnownHost(t *testing.T) {
	isolate(t)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ssh.NewSignerFromKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ed := newSigner(t)
	cfg := acceptAnyKey()
	cfg.AddHostKey(ec)
	addr := serveSSH(t, ed, cfg)

	file := writeKnownHosts(t, addr, ed.PublicKey())
	if err := dial(t, addr, file); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

// TestHandshakeAcceptsAHostCertificateBesideAStalePlainLine pins that a plain
// known_hosts line does not hide the certificate algorithms from a host an
// @cert-authority line covers.
func TestHandshakeAcceptsAHostCertificateBesideAStalePlainLine(t *testing.T) {
	ca, host := newSigner(t), newSigner(t)
	cert := &ssh.Certificate{
		Key:             host.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1"},
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	certSigner, err := ssh.NewCertSigner(cert, host)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveSSH(t, certSigner, acceptAnyKey())

	file := writeKnownHosts(t, addr, newSigner(t).PublicKey())
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, ca.PublicKey())
	_, err = f.WriteString("@cert-authority " + line + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dial(t, addr, file); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

func TestHostKeyAlgorithms(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ed := newSigner(t).PublicKey()
	file := filepath.Join(t.TempDir(), "known_hosts")
	lines := knownhosts.Line([]string{"[rsa.example]:22"}, rsaPub) + "\n" +
		knownhosts.Line([]string{"rsa.example", "both.example"}, ed) + "\n" +
		knownhosts.Line([]string{"both.example"}, rsaPub) + "\n" +
		"# a comment and a blank line, so line numbers differ from entry numbers\n\n" +
		"@cert-authority " + knownhosts.Line([]string{"ca.example", "caonly.example"}, rsaPub) + "\n" +
		knownhosts.Line([]string{"ca.example"}, ed) + "\n"
	if err := os.WriteFile(file, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	verify, err := hostKeys(Config{KnownHostsFile: file})
	if err != nil {
		t.Fatal(err)
	}

	for host, want := range map[string][]string{
		"rsa.example":  {"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa", "ssh-ed25519"},
		"both.example": {"ssh-ed25519", "rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"},
		"none.example": nil,
		"ca.example": {
			"ssh-ed25519-cert-v01@openssh.com",
			"ecdsa-sha2-nistp256-cert-v01@openssh.com",
			"ecdsa-sha2-nistp384-cert-v01@openssh.com",
			"ecdsa-sha2-nistp521-cert-v01@openssh.com",
			"rsa-sha2-512-cert-v01@openssh.com",
			"rsa-sha2-256-cert-v01@openssh.com",
			"ssh-ed25519",
		},
		"caonly.example": nil,
	} {
		got := hostKeyAlgorithms(verify, Config{Host: host, Port: 22})
		if !slices.Equal(got, want) {
			t.Errorf("%s: algorithms %v, want %v", host, got, want)
		}
	}
}

func TestHostKeysExpandsHomeInTheFile(t *testing.T) {
	isolate(t)
	home := os.Getenv("HOME")
	line := knownhosts.Line([]string{"example.com"}, newSigner(t).PublicKey()) + "\n"
	if err := os.WriteFile(filepath.Join(home, "kh"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostKeys(Config{Host: "example.com", KnownHostsFile: "~/kh"}); err != nil {
		t.Fatalf("hostKeys with ~/kh: %v", err)
	}
}
